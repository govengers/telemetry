package pgxadapter

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"time"
	"weak"

	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The wide-event fields the Tracer writes (standards LOG-004, namespace db). The Tracer owns
// them: a repository that writes them too overwrites them until the next statement.
const (
	KeyQueries  = "db.queries"     // int: statements run, each statement of a batch and each CopyFrom counted
	KeyDuration = "db.duration_ms" // duration: the sum of the statements' times, written in milliseconds
	KeySlowest  = "db.slowest_ms"  // duration: the longest single call (statement, whole batch, CopyFrom)
	KeyFailure  = "db.failure"     // string: the class of the first failed statement, one of the Failure values
	KeySQLState = "db.sqlstate"    // string: the SQLSTATE of that failure, when the server reported it
)

// Values of KeyFailure. Never the error text: a server error carries the values of the row
// that failed (the Detail of a unique violation names the duplicate key).
const (
	FailureCanceled   = "canceled"   // the statement's context was canceled
	FailureTimeout    = "timeout"    // the statement's context deadline passed, or a network timeout
	FailureConnection = "connection" // the connection broke or was closed
	FailureServer     = "server"     // the server rejected the statement; KeySQLState holds its code
	FailureClient     = "client"     // pgx failed on this side: encoding an argument, scanning a row
)

// sqlStateQueryCanceled is the server's answer to a cancel request, which pgx may send when the
// statement's context ends.
const sqlStateQueryCanceled = "57014"

// Tracer records the database work of each unit of work on the wide event in the statement's
// context (wideevent.FromContext): how many statements ran, their total time, the slowest one
// and the class of the first failure. It never records SQL text or arguments. Statements
// without a wide event in their context (migrations, startup) record nothing.
//
// Tracer implements pgx.QueryTracer, pgx.BatchTracer and pgx.CopyFromTracer. Install it on the
// pool's connection config, next to otelpgx through pgx's multitracer:
//
//	poolConfig.ConnConfig.Tracer = multitracer.New(otelpgx.NewTracer(), pgxadapter.NewTracer())
//
// A statement's time runs from the call to its end: for Query until the rows are closed, so it
// includes reading the rows; the implicit prepare of pgx's statement cache is part of it, waiting
// for a pool connection is not (the pool metrics show that). Statements that run concurrently in
// one unit of work add up, so the total can exceed the unit of work's duration. A batch counts
// each of its statements and is timed as one call. Statements that end after the event was
// emitted are dropped (the event is frozen).
//
// A Tracer is safe for concurrent use; one Tracer serves every pool of the process.
type Tracer struct {
	units sync.Map // weak.Pointer[wideevent.Event] -> *unitOfWork, removed when the event is collected
	now   func() time.Time
}

// unitOfWork is the running total of one wide event.
type unitOfWork struct {
	mutex   sync.Mutex
	queries int
	total   time.Duration
	slowest time.Duration
	failed  bool
}

// startKey is the context key of a call's start time, set by the Trace*Start methods.
type startKey struct{}

var (
	_ pgx.QueryTracer    = (*Tracer)(nil)
	_ pgx.BatchTracer    = (*Tracer)(nil)
	_ pgx.CopyFromTracer = (*Tracer)(nil)
)

// NewTracer returns a Tracer.
func NewTracer() *Tracer {
	return &Tracer{now: time.Now}
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return t.start(ctx)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *Tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	t.record(ctx, 1, t.elapsed(ctx), data.Err)
}

// TraceBatchStart implements pgx.BatchTracer.
func (t *Tracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return t.start(ctx)
}

// TraceBatchQuery implements pgx.BatchTracer: it counts the statement; the batch's time is
// recorded once, by TraceBatchEnd.
func (t *Tracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	t.record(ctx, 1, 0, data.Err)
}

// TraceBatchEnd implements pgx.BatchTracer.
func (t *Tracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	t.record(ctx, 0, t.elapsed(ctx), data.Err)
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (t *Tracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	return t.start(ctx)
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (t *Tracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	t.record(ctx, 1, t.elapsed(ctx), data.Err)
}

func (t *Tracer) start(ctx context.Context) context.Context {
	if wideevent.FromContext(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, startKey{}, t.now())
}

// elapsed is the time since the call's start, or 0 when the start was not recorded.
func (t *Tracer) elapsed(ctx context.Context) time.Duration {
	started, ok := ctx.Value(startKey{}).(time.Time)
	if !ok {
		return 0
	}
	return t.now().Sub(started)
}

// record adds a call to the unit of work's totals and writes them to its event. The unit's mutex
// is held while writing, so concurrent statements cannot write their totals out of order.
func (t *Tracer) record(ctx context.Context, queries int, duration time.Duration, err error) {
	we := wideevent.FromContext(ctx)
	if we == nil {
		return
	}
	unit := t.unitOf(we)
	unit.mutex.Lock()
	defer unit.mutex.Unlock()
	unit.queries += queries
	unit.total += duration
	unit.slowest = max(unit.slowest, duration)
	we.Int(KeyQueries, unit.queries).Duration(KeyDuration, unit.total).Duration(KeySlowest, unit.slowest)
	if err == nil || unit.failed {
		return
	}
	unit.failed = true
	failure, sqlState := classify(ctx, err)
	we.String(KeyFailure, failure)
	if sqlState != "" {
		we.String(KeySQLState, sqlState)
	}
}

// unitOf returns the totals of we, creating them on the event's first statement. The map holds
// the event only weakly and a cleanup removes the entry once the event is collected, so the
// Tracer needs no hook at the end of the unit of work and keeps nothing after it.
func (t *Tracer) unitOf(we *wideevent.Event) *unitOfWork {
	key := weak.Make(we)
	if unit, ok := t.units.Load(key); ok {
		return unit.(*unitOfWork)
	}
	unit, loaded := t.units.LoadOrStore(key, &unitOfWork{})
	if !loaded {
		runtime.AddCleanup(we, t.forget, key)
	}
	return unit.(*unitOfWork)
}

func (t *Tracer) forget(key weak.Pointer[wideevent.Event]) {
	t.units.Delete(key)
}

// classify maps a statement's error to a Failure value and, for a server error, its SQLSTATE.
func classify(ctx context.Context, err error) (failure, sqlState string) {
	var serverError *pgconn.PgError
	if errors.As(err, &serverError) && (serverError.Code != sqlStateQueryCanceled || ctx.Err() == nil) {
		return FailureServer, serverError.Code
	}
	var netError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return FailureCanceled, ""
	case errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err):
		return FailureTimeout, ""
	case serverError != nil: // the server canceled the statement because its context ended
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return FailureTimeout, ""
		}
		return FailureCanceled, ""
	case errors.As(err, &netError), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, pgconn.ErrConnClosed):
		return FailureConnection, ""
	default:
		return FailureClient, ""
	}
}
