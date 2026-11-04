package tracing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/govengers/telemetry/internal/otlptest"
	"github.com/govengers/telemetry/tracing"
)

// clearEnvironment unsets every variable New reads, for the duration of the test.
func clearEnvironment(t *testing.T) {
	t.Helper()
	for _, variable := range []string{
		"OTEL_SDK_DISABLED", "OTEL_TRACES_EXPORTER", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(variable, "")
	}
}

func TestNewRequiresAServiceName(t *testing.T) {
	for name, options := range map[string]*tracing.Options{"nil options": nil, "no name": {ServiceVersion: "1.0.0"}} {
		t.Run(name, func(t *testing.T) {
			created, err := tracing.New(t.Context(), options)

			require.Error(t, err)
			require.Nil(t, created)
		})
	}
}

func TestNewIsANoOpWithoutAnEndpointOrWhenDisabled(t *testing.T) {
	tests := map[string]map[string]string{
		"no endpoint":           {},
		"SDK disabled":          {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1", "OTEL_SDK_DISABLED": "true"},
		"SDK disabled in caps":  {"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://127.0.0.1:1", "OTEL_SDK_DISABLED": "TRUE"},
		"exporter none":         {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:1", "OTEL_TRACES_EXPORTER": "none"},
		"protocol without need": {"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"},
	}
	for name, environment := range tests {
		t.Run(name, func(t *testing.T) {
			clearEnvironment(t)
			for variable, value := range environment {
				t.Setenv(variable, value)
			}

			created, err := tracing.New(t.Context(), &tracing.Options{ServiceName: "billing"})

			require.NoError(t, err)
			require.False(t, created.Enabled())
			_, span := created.TracerProvider().Tracer("test").Start(t.Context(), "work")
			require.False(t, span.IsRecording())
			span.End()
			require.NoError(t, created.Shutdown(t.Context()))
		})
	}
}

func TestNewRejectsWhatItCannotExport(t *testing.T) {
	tests := map[string]struct {
		environment map[string]string
		wantError   string
	}{
		"other exporter":          {environment: map[string]string{"OTEL_TRACES_EXPORTER": "zipkin"}, wantError: `OTEL_TRACES_EXPORTER="zipkin"`},
		"grpc":                    {environment: map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, wantError: `OTEL_EXPORTER_OTLP_PROTOCOL="grpc"`},
		"grpc for traces":         {environment: map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"}, wantError: `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL="grpc"`},
		"json is not implemented": {environment: map[string]string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "http/json"}, wantError: `"http/json"`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			clearEnvironment(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			for variable, value := range test.environment {
				t.Setenv(variable, value)
			}

			_, err := tracing.New(t.Context(), &tracing.Options{ServiceName: "billing"})

			require.ErrorContains(t, err, test.wantError)
		})
	}
}

func TestNewExportsToTheEndpoint(t *testing.T) {
	tests := map[string]struct {
		variable, protocol string
	}{
		"endpoint":                     {variable: "OTEL_EXPORTER_OTLP_ENDPOINT"},
		"traces endpoint":              {variable: "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"},
		"endpoint with protocol named": {variable: "OTEL_EXPORTER_OTLP_ENDPOINT", protocol: "http/protobuf"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			receiver := otlptest.NewReceiver()
			defer receiver.Close()
			clearEnvironment(t)
			endpoint := receiver.URL
			if test.variable == "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT" {
				endpoint += "/v1/traces" // the signal-specific variable is the full URL
			}
			t.Setenv(test.variable, endpoint)
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", test.protocol)
			created, err := tracing.New(t.Context(), &tracing.Options{ServiceName: "billing", ServiceVersion: "1.2.3"})
			require.NoError(t, err)
			require.True(t, created.Enabled())

			_, span := created.TracerProvider().Tracer("test").Start(t.Context(), "checkout")
			span.End()
			require.NoError(t, created.Shutdown(t.Context()))

			spans := receiver.Spans()
			require.Len(t, spans, 1)
			require.Equal(t, "checkout", spans[0].GetName())
			require.Equal(t, "billing", receiver.ResourceAttribute("service.name"))
			require.Equal(t, "1.2.3", receiver.ResourceAttribute("service.version"))
		})
	}
}

func TestTheEnvironmentWinsOverTheOptions(t *testing.T) {
	receiver := otlptest.NewReceiver()
	defer receiver.Close()
	clearEnvironment(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	t.Setenv("OTEL_SERVICE_NAME", "billing-canary")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=staging")
	created, err := tracing.New(t.Context(), &tracing.Options{ServiceName: "billing"})
	require.NoError(t, err)

	_, span := created.TracerProvider().Tracer("test").Start(t.Context(), "checkout")
	span.End()
	require.NoError(t, created.Shutdown(t.Context()))

	require.Equal(t, "billing-canary", receiver.ResourceAttribute("service.name"))
	require.Equal(t, "staging", receiver.ResourceAttribute("deployment.environment.name"))
}

func TestShutdownReportsAnExportThatCannotFinish(t *testing.T) {
	release := make(chan struct{})
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		<-release                                 // the export hangs until the test ends
		writer.WriteHeader(http.StatusBadRequest) // not retried, so the exporter stops at once
	}))
	defer collector.Close()
	defer close(release) // before Close, which waits for the handler
	clearEnvironment(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	created, err := tracing.New(t.Context(), &tracing.Options{ServiceName: "billing"})
	require.NoError(t, err)
	_, span := created.TracerProvider().Tracer("test").Start(t.Context(), "checkout")
	span.End()
	deadline, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err = created.Shutdown(deadline)

	require.ErrorContains(t, err, "telemetry tracing: shutdown")
}

// field returns the value of key on we, nil when we lacks it.
func field(we *wideevent.Event, key string) any {
	for _, recorded := range we.Fields() {
		if recorded.Key == key {
			return recorded.Value
		}
	}
	return nil
}

func spanContext(t *testing.T) trace.SpanContext {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
}

func TestRecordTraceID(t *testing.T) {
	tests := map[string]struct {
		spanContext             trace.SpanContext
		wantTraceID, wantSpanID any
	}{
		"valid span":   {spanContext: spanContext(t), wantTraceID: "4bf92f3577b34da6a3ce929d0e0e4736", wantSpanID: "00f067aa0ba902b7"},
		"invalid span": {spanContext: trace.SpanContext{}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			we := wideevent.Begin(wideevent.EmitterFunc(func(*wideevent.Event) {}), "test")

			tracing.RecordTraceID(trace.ContextWithSpanContext(t.Context(), test.spanContext), we)

			require.Equal(t, test.wantTraceID, field(we, tracing.KeyTraceID))
			require.Equal(t, test.wantSpanID, field(we, tracing.KeySpanID))
		})
	}
}

func TestRecordTraceIDWithoutASpanWritesNothing(t *testing.T) {
	we := wideevent.Begin(wideevent.EmitterFunc(func(*wideevent.Event) {}), "test")

	tracing.RecordTraceID(t.Context(), we)

	require.Nil(t, field(we, tracing.KeyTraceID))
	require.Nil(t, field(we, tracing.KeySpanID))
}

func TestRecordTraceIDOnANilEventDoesNothing(t *testing.T) {
	require.NotPanics(t, func() {
		tracing.RecordTraceID(trace.ContextWithSpanContext(context.Background(), spanContext(t)), nil)
	})
}

// The documented Gin wiring: otelgin before the wide-event middleware, RecordTraceID in its
// PreEnricher. The event carries the IDs of the request's server span, which continues the
// caller's trace.
func TestGinWiringPutsTheRequestSpanOnTheWideEvent(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	var events []*wideevent.Event
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{
		Emitter: wideevent.EmitterFunc(func(we *wideevent.Event) { events = append(events, we) }),
		PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
			tracing.RecordTraceID(ginContext.Request.Context(), we)
		},
	})
	require.NoError(t, err)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(otelgin.Middleware("billing", otelgin.WithTracerProvider(provider), otelgin.WithPropagators(propagation.TraceContext{})), wideEvents)
	engine.GET("/api/v1/invoices/:id", func(ginContext *gin.Context) { ginContext.Status(http.StatusOK) })
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/invoices/42", nil)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	engine.ServeHTTP(httptest.NewRecorder(), request)

	spans := recorder.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, "GET /api/v1/invoices/:id", spans[0].Name())
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", spans[0].SpanContext().TraceID().String())
	require.Equal(t, "00f067aa0ba902b7", spans[0].Parent().SpanID().String())
	require.Len(t, events, 1)
	require.Equal(t, spans[0].SpanContext().TraceID().String(), field(events[0], tracing.KeyTraceID))
	require.Equal(t, spans[0].SpanContext().SpanID().String(), field(events[0], tracing.KeySpanID))
}
