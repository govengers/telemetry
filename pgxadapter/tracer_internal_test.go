package pgxadapter

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// fakeClock is the Tracer's clock in these tests; advance moves it.
type fakeClock struct {
	mutex   sync.Mutex
	current time.Time
}

func (c *fakeClock) now() time.Time {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.current
}

func (c *fakeClock) advance(duration time.Duration) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.current = c.current.Add(duration)
}

func newClockedTracer() (*Tracer, *fakeClock) {
	clock := &fakeClock{current: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	tracer := NewTracer()
	tracer.now = clock.now
	return tracer, clock
}

func newUnitOfWork(t *testing.T) (context.Context, *wideevent.Event) {
	t.Helper()
	we := wideevent.Begin(nil, "test")
	return wideevent.NewContext(t.Context(), we), we
}

func fieldsOf(we *wideevent.Event) map[string]any {
	fields := map[string]any{}
	for _, field := range we.Fields() {
		fields[field.Key] = field.Value
	}
	return fields
}

func (t *Tracer) unitCount() int {
	count := 0
	t.units.Range(func(any, any) bool {
		count++
		return true
	})
	return count
}

func TestTracerAddsUpTheStatementsOfAUnitOfWork(t *testing.T) {
	tracer, clock := newClockedTracer()
	ctx, we := newUnitOfWork(t)

	query := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "select 1"})
	clock.advance(3 * time.Millisecond)
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{})
	query = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "select 2"})
	clock.advance(5 * time.Millisecond)
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{})

	fields := fieldsOf(we)
	require.Equal(t, 2, fields[KeyQueries])
	require.Equal(t, 8*time.Millisecond, fields[KeyDuration])
	require.Equal(t, 5*time.Millisecond, fields[KeySlowest])
	require.NotContains(t, fields, KeyFailure)
	require.NotContains(t, fields, KeySQLState)
}

func TestTracerCountsEachStatementOfABatchAndTimesTheBatchOnce(t *testing.T) {
	tracer, clock := newClockedTracer()
	ctx, we := newUnitOfWork(t)

	batch := tracer.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
	for range 3 {
		clock.advance(time.Millisecond)
		tracer.TraceBatchQuery(batch, nil, pgx.TraceBatchQueryData{})
	}
	clock.advance(time.Millisecond)
	tracer.TraceBatchEnd(batch, nil, pgx.TraceBatchEndData{})

	fields := fieldsOf(we)
	require.Equal(t, 3, fields[KeyQueries])
	require.Equal(t, 4*time.Millisecond, fields[KeyDuration])
	require.Equal(t, 4*time.Millisecond, fields[KeySlowest])
}

func TestABatchThatEndsTwiceAddsItsTimeOnce(t *testing.T) {
	tracer, clock := newClockedTracer()
	ctx, we := newUnitOfWork(t)
	failure := &pgconn.PgError{Code: "42P01"}

	// pgx ends a batch that fails before its first result in SendBatch and again in Close
	batch := tracer.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
	clock.advance(2 * time.Millisecond)
	tracer.TraceBatchEnd(batch, nil, pgx.TraceBatchEndData{Err: failure})
	clock.advance(9 * time.Millisecond)
	tracer.TraceBatchEnd(batch, nil, pgx.TraceBatchEndData{Err: failure})

	fields := fieldsOf(we)
	require.Equal(t, 0, fields[KeyQueries])
	require.Equal(t, 2*time.Millisecond, fields[KeyDuration])
	require.Equal(t, 2*time.Millisecond, fields[KeySlowest])
	require.Equal(t, "42P01", fields[KeySQLState])
}

func TestTracerCountsACopyFromAsOneStatement(t *testing.T) {
	tracer, clock := newClockedTracer()
	ctx, we := newUnitOfWork(t)

	copying := tracer.TraceCopyFromStart(ctx, nil, pgx.TraceCopyFromStartData{TableName: pgx.Identifier{"orders"}})
	clock.advance(7 * time.Millisecond)
	tracer.TraceCopyFromEnd(copying, nil, pgx.TraceCopyFromEndData{})

	fields := fieldsOf(we)
	require.Equal(t, 1, fields[KeyQueries])
	require.Equal(t, 7*time.Millisecond, fields[KeyDuration])
	require.Equal(t, 7*time.Millisecond, fields[KeySlowest])
}

func TestTracerKeepsTheFirstFailure(t *testing.T) {
	tracer, _ := newClockedTracer()
	ctx, we := newUnitOfWork(t)

	query := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "23505", Detail: "Key (email)=(a@example.com) already exists."}})
	query = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: context.Canceled})

	fields := fieldsOf(we)
	require.Equal(t, 2, fields[KeyQueries])
	require.Equal(t, FailureServer, fields[KeyFailure])
	require.Equal(t, "23505", fields[KeySQLState])
}

func TestTracerRecordsNothingOutsideAUnitOfWork(t *testing.T) {
	tracer, _ := newClockedTracer()

	query := tracer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{})
	tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})

	require.Equal(t, t.Context(), query, "no start time without an event")
	require.Zero(t, tracer.unitCount())
}

func TestConcurrentStatementsOfOneUnitOfWorkAllCount(t *testing.T) {
	tracer, clock := newClockedTracer()
	ctx, we := newUnitOfWork(t)
	const statements = 64

	var group sync.WaitGroup
	for range statements {
		group.Go(func() {
			query := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
			clock.advance(time.Millisecond)
			tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{})
		})
	}
	group.Wait()

	fields := fieldsOf(we)
	require.Equal(t, statements, fields[KeyQueries])
	require.Equal(t, 1, tracer.unitCount())
}

func TestTheTotalsOfAUnitOfWorkGoWithItsEvent(t *testing.T) {
	tracer, _ := newClockedTracer()
	func() {
		ctx, _ := newUnitOfWork(t)
		query := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{})
		tracer.TraceQueryEnd(query, nil, pgx.TraceQueryEndData{})
	}()
	require.Equal(t, 1, tracer.unitCount())

	// The cleanup runs on its own goroutine after a collection; give it a bounded number of tries.
	for attempt := 0; attempt < 100 && tracer.unitCount() > 0; attempt++ {
		runtime.GC()
		runtime.Gosched()
	}

	require.Zero(t, tracer.unitCount(), "the entry of a collected event is removed")
}

func TestAnEndWithoutItsStartCountsWithoutATime(t *testing.T) {
	tracer, _ := newClockedTracer()
	ctx, we := newUnitOfWork(t)

	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	fields := fieldsOf(we)
	require.Equal(t, 1, fields[KeyQueries])
	require.Equal(t, time.Duration(0), fields[KeyDuration])
}
