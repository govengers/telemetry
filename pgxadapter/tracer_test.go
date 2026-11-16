package pgxadapter_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	wideevent "github.com/govengers/wide-event-logger"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/require"

	"github.com/govengers/telemetry/pgxadapter"
)

func newUnitOfWork(ctx context.Context) (context.Context, *wideevent.Event) {
	we := wideevent.Begin(nil, "test")
	return wideevent.NewContext(ctx, we), we
}

func fieldsOf(we *wideevent.Event) map[string]any {
	fields := map[string]any{}
	for _, field := range we.Fields() {
		fields[field.Key] = field.Value
	}
	return fields
}

// failWith runs one statement that fails with err in a context ctx and returns the event's fields.
func failWith(ctx context.Context, err error) map[string]any {
	tracer := pgxadapter.NewTracer()
	ctx, we := newUnitOfWork(ctx)
	tracer.TraceQueryEnd(tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}), nil, pgx.TraceQueryEndData{Err: err})
	return fieldsOf(we)
}

func TestTracerClassifiesTheFailure(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancelExpired()
	queryCanceled := &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}

	for name, test := range map[string]struct {
		ctx      context.Context //nolint:containedctx // a table of test cases
		err      error
		failure  string
		sqlState string
	}{
		"unique violation":                  {t.Context(), fmt.Errorf("insert order: %w", &pgconn.PgError{Code: "23505"}), pgxadapter.FailureServer, "23505"},
		"statement timeout of the server":   {t.Context(), queryCanceled, pgxadapter.FailureServer, "57014"},
		"server cancel of a canceled call":  {canceled, queryCanceled, pgxadapter.FailureCanceled, ""},
		"server cancel of an expired call":  {expired, queryCanceled, pgxadapter.FailureTimeout, ""},
		"context canceled":                  {canceled, context.Canceled, pgxadapter.FailureCanceled, ""},
		"context deadline":                  {expired, fmt.Errorf("read: %w", context.DeadlineExceeded), pgxadapter.FailureTimeout, ""},
		"connection reset":                  {t.Context(), &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, pgxadapter.FailureConnection, ""},
		"connection ended":                  {t.Context(), fmt.Errorf("receive message: %w", io.ErrUnexpectedEOF), pgxadapter.FailureConnection, ""},
		"connection closed":                 {t.Context(), pgconn.ErrConnClosed, pgxadapter.FailureConnection, ""},
		"argument the driver cannot encode": {t.Context(), errors.New("failed to encode args[0]"), pgxadapter.FailureClient, ""},
	} {
		t.Run(name, func(t *testing.T) {
			fields := failWith(test.ctx, test.err)

			require.Equal(t, test.failure, fields[pgxadapter.KeyFailure])
			if test.sqlState == "" {
				require.NotContains(t, fields, pgxadapter.KeySQLState)
			} else {
				require.Equal(t, test.sqlState, fields[pgxadapter.KeySQLState])
			}
		})
	}
}

func TestTracerNeverRecordsTheErrorText(t *testing.T) {
	fields := failWith(t.Context(), &pgconn.PgError{Code: "23505", Message: "duplicate key", Detail: "Key (email)=(a@example.com) already exists."})

	for key, value := range fields {
		require.NotContains(t, fmt.Sprint(value), "example.com", key)
	}
}

// contextTracer stands in for otelpgx: it puts its own value into the statement's context.
type contextTracer struct{ seen []any }

type contextTracerKey struct{}

func (c *contextTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, contextTracerKey{}, "span")
}

func (c *contextTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	c.seen = append(c.seen, ctx.Value(contextTracerKey{}))
}

func TestTracerComposesWithAnotherTracerThroughMultitracer(t *testing.T) {
	other := &contextTracer{}
	for name, tracer := range map[string]*multitracer.Tracer{
		"after the other":  multitracer.New(other, pgxadapter.NewTracer()),
		"before the other": multitracer.New(pgxadapter.NewTracer(), other),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, we := newUnitOfWork(t.Context())

			tracer.TraceQueryEnd(tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}), nil, pgx.TraceQueryEndData{})
			batch := tracer.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
			tracer.TraceBatchQuery(batch, nil, pgx.TraceBatchQueryData{})
			tracer.TraceBatchEnd(batch, nil, pgx.TraceBatchEndData{})

			require.Equal(t, 2, fieldsOf(we)[pgxadapter.KeyQueries])
			require.Equal(t, "span", other.seen[len(other.seen)-1])
		})
	}
}

// fakeServer answers pgx over an in-memory pipe in the simple query protocol: "fail" gets a unique
// violation, any other statement one row with the value 1. It is enough to run pgx's real call
// paths without a PostgreSQL server.
func fakeServer(t *testing.T, conn net.Conn) {
	t.Helper()
	backend := pgproto3.NewBackend(conn, conn)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
	backend.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	for backend.Flush() == nil {
		message, err := backend.Receive()
		if err != nil {
			return
		}
		query, ok := message.(*pgproto3.Query)
		if !ok {
			return // Terminate
		}
		if strings.Contains(query.String, "fail") {
			backend.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint"})
		} else {
			backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("n"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1}}})
			backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
			backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
		}
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	}
}

func connectToFakeServer(t *testing.T, tracer pgx.QueryTracer) *pgx.Conn {
	t.Helper()
	config, err := pgx.ParseConfig("postgres://test@fake/test?sslmode=disable")
	require.NoError(t, err)
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.Tracer = tracer
	config.LookupFunc = func(_ context.Context, host string) ([]string, error) { return []string{host}, nil }
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go fakeServer(t, server)
		return client, nil
	}
	conn, err := pgx.ConnectConfig(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close(context.Background())) })
	return conn
}

func TestTracerRecordsPgxStatementsOnTheWideEvent(t *testing.T) {
	conn := connectToFakeServer(t, multitracer.New(&contextTracer{}, pgxadapter.NewTracer()))
	ctx, we := newUnitOfWork(t.Context())

	_, err := conn.Exec(ctx, "select 1")
	require.NoError(t, err)
	var value int
	require.NoError(t, conn.QueryRow(ctx, "select 1").Scan(&value))
	rows, err := conn.Query(ctx, "select 1")
	require.NoError(t, err)
	before := fieldsOf(we)[pgxadapter.KeyQueries]
	rows.Close()
	_, err = conn.Exec(ctx, "fail")
	require.Error(t, err)
	_, err = conn.Exec(t.Context(), "select 1") // no wide event: not counted

	require.NoError(t, err)
	fields := fieldsOf(we)
	require.Equal(t, 2, before, "a query counts when its rows are closed")
	require.Equal(t, 4, fields[pgxadapter.KeyQueries])
	require.Equal(t, pgxadapter.FailureServer, fields[pgxadapter.KeyFailure])
	require.Equal(t, "23505", fields[pgxadapter.KeySQLState])
	require.IsType(t, time.Duration(0), fields[pgxadapter.KeyDuration])
	require.IsType(t, time.Duration(0), fields[pgxadapter.KeySlowest])
	require.GreaterOrEqual(t, fields[pgxadapter.KeyDuration], fields[pgxadapter.KeySlowest])
}
