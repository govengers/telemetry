//go:build e2e

// Package e2e runs the documented service wiring against real collaborators: a public and an
// internal metrics listener on TCP ports, an OTLP/HTTP receiver, and a downstream service
// reached through otelhttp.
package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/govengers/telemetry"
	"github.com/govengers/telemetry/ginadapter"
	"github.com/govengers/telemetry/internal/otlptest"
	"github.com/govengers/telemetry/tracing"
)

const (
	callerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpanID  = "00f067aa0ba902b7"
)

// lockedBuffer collects the JSON lines of the wide-event emitter.
type lockedBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Write(data)
}

func (b *lockedBuffer) events(t *testing.T) []map[string]any {
	t.Helper()
	b.mutex.Lock()
	defer b.mutex.Unlock()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buffer.String()), "\n") {
		event := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(line), &event))
		events = append(events, event)
	}
	return events
}

func TestServiceWiring(t *testing.T) {
	receiver := otlptest.NewReceiver()
	defer receiver.Close()
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_TRACES_EXPORTER", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", receiver.URL)
	var downstreamTraceparent string
	downstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		downstreamTraceparent = request.Header.Get("traceparent")
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer downstream.Close()

	// main, as the README shows it, with the provider passed through the options instead of the globals
	traces, err := tracing.New(t.Context(), &tracing.Options{ServiceName: "billing", ServiceVersion: "1.4.0"})
	require.NoError(t, err)
	require.True(t, traces.Enabled())
	propagator := propagation.TraceContext{}
	registry := telemetry.NewRegistry()
	requestMetrics, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registry, Namespace: "billing", SkipRoutes: []string{"/api/health"}})
	require.NoError(t, err)
	logs := &lockedBuffer{}
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{
		Emitter:   wideevent.NewJSONEmitter(&wideevent.JSONEmitterOptions{Writer: logs}),
		SkipPaths: []string{"/api/health"},
		PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
			tracing.RecordTraceID(ginContext.Request.Context(), we)
		},
	})
	require.NoError(t, err)
	client := &http.Client{Timeout: 5 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithTracerProvider(traces.TracerProvider()), otelhttp.WithPropagators(propagator))}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(otelgin.Middleware("billing",
		otelgin.WithTracerProvider(traces.TracerProvider()),
		otelgin.WithPropagators(propagator),
		otelgin.WithGinFilter(func(ginContext *gin.Context) bool { return ginContext.FullPath() != "/api/health" }),
	), wideEvents, requestMetrics)
	engine.GET("/api/health", func(ginContext *gin.Context) { ginContext.Status(http.StatusNoContent) })
	engine.GET("/api/v1/invoices/:id", func(ginContext *gin.Context) {
		request, err := http.NewRequestWithContext(ginContext.Request.Context(), http.MethodGet, downstream.URL+"/ledger?email=alice%40example.com", nil)
		if err != nil {
			ginContext.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		response, err := client.Do(request)
		if err != nil {
			ginContext.AbortWithStatus(http.StatusBadGateway)
			return
		}
		_ = response.Body.Close()
		ginContext.Status(http.StatusOK)
	})
	public := httptest.NewServer(engine)
	defer public.Close()
	metricsServer, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: registry})
	require.NoError(t, err)
	metricsListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = metricsServer.Serve(metricsListener) }()
	defer func() { require.NoError(t, metricsServer.Shutdown(t.Context())) }()

	// a caller in a trace of its own
	caller := &http.Client{Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+"/api/v1/invoices/42", nil)
	require.NoError(t, err)
	request.Header.Set("traceparent", "00-"+callerTraceID+"-"+callerSpanID+"-01")
	response, err := caller.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	health, err := http.NewRequestWithContext(t.Context(), http.MethodGet, public.URL+"/api/health", nil)
	require.NoError(t, err)
	response, err = caller.Do(health)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	require.NoError(t, traces.Shutdown(t.Context()))

	t.Run("the spans reach the collector as one trace", func(t *testing.T) {
		server, outbound := spanOfKind(t, receiver, tracev1.Span_SPAN_KIND_SERVER), spanOfKind(t, receiver, tracev1.Span_SPAN_KIND_CLIENT)
		require.Equal(t, "GET /api/v1/invoices/:id", server.GetName())
		require.Equal(t, callerTraceID, hexOf(server.GetTraceId()))
		require.Equal(t, callerSpanID, hexOf(server.GetParentSpanId()))
		require.Equal(t, callerTraceID, hexOf(outbound.GetTraceId()))
		require.Equal(t, hexOf(server.GetSpanId()), hexOf(outbound.GetParentSpanId()))
		require.Equal(t, "billing", receiver.ResourceAttribute("service.name"))
		require.Equal(t, "1.4.0", receiver.ResourceAttribute("service.version"))
		require.Len(t, receiver.Spans(), 2, "the health route is filtered")
	})

	t.Run("the outbound URL reaches the collector without its query values", func(t *testing.T) {
		outbound := spanOfKind(t, receiver, tracev1.Span_SPAN_KIND_CLIENT)
		var urls []string
		for _, attribute := range outbound.GetAttributes() {
			if attribute.GetKey() == "url.full" {
				urls = append(urls, attribute.GetValue().GetStringValue())
			}
		}
		require.Equal(t, []string{downstream.URL + "/ledger?email=redacted"}, urls)
	})

	t.Run("the downstream service joins the trace", func(t *testing.T) {
		require.True(t, strings.HasPrefix(downstreamTraceparent, "00-"+callerTraceID+"-"), downstreamTraceparent)
	})

	t.Run("the wide event leads to the trace", func(t *testing.T) {
		events := logs.events(t)
		require.Len(t, events, 1, "the health route has no event")
		trace, isMap := events[0]["trace"].(map[string]any)
		require.True(t, isMap, "event %v", events[0])
		require.Equal(t, callerTraceID, trace["id"])
		require.Equal(t, hexOf(spanOfKind(t, receiver, tracev1.Span_SPAN_KIND_SERVER).GetSpanId()), trace["span_id"])
	})

	t.Run("the metrics listener serves the request metrics", func(t *testing.T) {
		scrape, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+metricsListener.Addr().String()+"/metrics", nil)
		require.NoError(t, err)
		response, err := caller.Do(scrape)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, response.Body.Close())
		require.NoError(t, err)

		require.Contains(t, string(body), `billing_http_requests_total{method="GET",route="/api/v1/invoices/:id",status="200"} 1`)
		require.NotContains(t, string(body), `route="/api/health"`)
		require.Contains(t, string(body), "go_build_info")
		problems, err := testutil.GatherAndLint(registry)
		require.NoError(t, err)
		require.Empty(t, problems)
	})
}

func spanOfKind(t *testing.T, receiver *otlptest.Receiver, kind tracev1.Span_SpanKind) *tracev1.Span {
	t.Helper()
	for _, span := range receiver.Spans() {
		if span.GetKind() == kind {
			return span
		}
	}
	t.Fatalf("no %s span", kind)
	return nil
}

func hexOf(id []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, 0, 2*len(id))
	for _, value := range id {
		encoded = append(encoded, digits[value>>4], digits[value&0x0f])
	}
	return string(encoded)
}
