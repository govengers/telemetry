// Package tracing sets up the OpenTelemetry tracer provider of a govengers service and puts the
// trace ID on its wide events. OpenTelemetry is used for traces only: Prometheus is the metrics
// and wide events are the logs, so the package installs no OpenTelemetry meter or logger
// provider.
//
// Configuration comes from the standard OTEL_* environment variables, read by New, the SDK
// and the OTLP/HTTP exporter, not from the service's own configuration. Without an OTLP
// endpoint New returns a no-op provider: a service without a trace backend pays nothing.
//
// New sets no global state. main installs the provider and the W3C trace context propagator
// itself, or passes the provider to each instrumentation through its options:
//
//	otel.SetTracerProvider(provider.TracerProvider())
//	otel.SetTextMapPropagator(propagation.TraceContext{})
package tracing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	wideevent "github.com/govengers/wide-event-logger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// The wide-event fields RecordTraceID writes.
const (
	KeyTraceID = "trace.id"
	KeySpanID  = "trace.span_id"
)

// The environment variables New reads itself; the SDK and the exporter read the rest
// (OTEL_EXPORTER_OTLP_HEADERS, OTEL_EXPORTER_OTLP_TIMEOUT, OTEL_TRACES_SAMPLER,
// OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME, ...).
const (
	variableSDKDisabled    = "OTEL_SDK_DISABLED"
	variableTracesExporter = "OTEL_TRACES_EXPORTER"
	variableEndpoint       = "OTEL_EXPORTER_OTLP_ENDPOINT"
	variableTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	variableProtocol       = "OTEL_EXPORTER_OTLP_PROTOCOL"
	variableTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
)

const (
	supportedProtocol = "http/protobuf"
	supportedExporter = "otlp"
	disabledExporter  = "none"
)

// Options configures New.
type Options struct {
	ServiceName    string // required: service.name of every span, unless OTEL_SERVICE_NAME overrides it
	ServiceVersion string // service.version, injected at build time; empty records none
}

// Provider holds the tracer provider of the process. Build it with New and call Shutdown in the
// drain sequence.
type Provider struct {
	tracerProvider trace.TracerProvider     // what instrumentations get: the SDK's, or a no-op
	exporting      *sdktrace.TracerProvider // the SDK's, to flush; nil when tracing is off
}

// New builds the tracer provider. Tracing is off, and New returns a no-op provider without an
// error, when OTEL_SDK_DISABLED is true, OTEL_TRACES_EXPORTER is none, or neither
// OTEL_EXPORTER_OTLP_ENDPOINT nor OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is set. Otherwise spans go
// in batches to the OTLP/HTTP exporter. Only the otlp exporter and the http/protobuf protocol
// are supported; any other value of OTEL_TRACES_EXPORTER or OTEL_EXPORTER_OTLP_(TRACES_)PROTOCOL
// is an error, so a misconfigured deployment fails at startup instead of losing its traces.
//
// The sampler is the SDK's default, ParentBased(AlwaysOn), which OTEL_TRACES_SAMPLER can
// change; keep it and decide in the collector which traces to keep (tail sampling). The
// resource holds the service name and version, the SDK's own attributes and
// OTEL_RESOURCE_ATTRIBUTES, where the environment wins. Query values in url.full and url.query
// are replaced by RedactedValue before export, so an outbound URL's tokens and personal
// data never reach the trace backend.
func New(ctx context.Context, options *Options) (*Provider, error) {
	if options == nil || options.ServiceName == "" {
		return nil, errors.New("telemetry tracing: ServiceName is required")
	}
	enabled, err := enabledByEnvironment()
	if err != nil || !enabled {
		return &Provider{tracerProvider: noop.NewTracerProvider()}, err
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry tracing: OTLP exporter: %v", err) //nolint:errorlint // the exporter's errors are no part of this API (LIB-010)
	}
	serviceResource, err := newResource(ctx, options)
	if err != nil {
		return nil, errors.Join(err, exporter.Shutdown(ctx))
	}
	exporting := sdktrace.NewTracerProvider(sdktrace.WithBatcher(redactingExporter{exporter}), sdktrace.WithResource(serviceResource))
	return &Provider{tracerProvider: exporting, exporting: exporting}, nil
}

// enabledByEnvironment says whether the OTEL_* variables ask for traces, and rejects what the
// package cannot export.
func enabledByEnvironment() (bool, error) {
	if disabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(variableSDKDisabled))); disabled {
		return false, nil
	}
	switch exporter := strings.TrimSpace(os.Getenv(variableTracesExporter)); exporter {
	case disabledExporter:
		return false, nil
	case "", supportedExporter: // unset means otlp
	default:
		return false, fmt.Errorf("telemetry tracing: %s=%q is not supported, only %q or %q", variableTracesExporter, exporter, supportedExporter, disabledExporter)
	}
	if os.Getenv(variableEndpoint) == "" && os.Getenv(variableTracesEndpoint) == "" {
		return false, nil
	}
	protocolVariable, protocol := variableTracesProtocol, os.Getenv(variableTracesProtocol)
	if protocol == "" {
		protocolVariable, protocol = variableProtocol, os.Getenv(variableProtocol)
	}
	if protocol != "" && protocol != supportedProtocol {
		return false, fmt.Errorf("telemetry tracing: %s=%q is not supported, only %q", protocolVariable, protocol, supportedProtocol)
	}
	return true, nil
}

func newResource(ctx context.Context, options *Options) (*resource.Resource, error) {
	attributes := []attribute.KeyValue{attribute.String("service.name", options.ServiceName)}
	if options.ServiceVersion != "" {
		attributes = append(attributes, attribute.String("service.version", options.ServiceVersion))
	}
	serviceResource, err := resource.New(ctx,
		resource.WithAttributes(attributes...),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(), // last, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry tracing: resource: %v", err) //nolint:errorlint // the SDK's errors are no part of this API (LIB-010)
	}
	return serviceResource, nil
}

// Enabled reports whether spans are exported; false means the provider is a no-op.
func (p *Provider) Enabled() bool {
	return p.exporting != nil
}

// TracerProvider returns the provider to install with otel.SetTracerProvider or to pass to an
// instrumentation's options.
func (p *Provider) TracerProvider() trace.TracerProvider {
	return p.tracerProvider
}

// Shutdown exports the spans still buffered and stops the exporter. Call it once in the drain
// sequence, after the servers stopped, with a deadline; a no-op provider returns nil at once.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.exporting == nil {
		return nil
	}
	if err := p.exporting.Shutdown(ctx); err != nil {
		return fmt.Errorf("telemetry tracing: shutdown: %v", err) //nolint:errorlint // the SDK's errors are no part of this API (LIB-010)
	}
	return nil
}

// RecordTraceID writes the trace and span ID of the span in ctx onto we as trace.id and
// trace.span_id, so a log line leads to its trace. A context without a valid span context
// writes nothing. With a no-op provider and the W3C propagator installed, an incoming
// traceparent still yields the caller's IDs: the request joins the caller's trace in the logs.
//
// With Gin, call it in the wide-event middleware's PreEnricher and mount otelgin.Middleware
// before the wide-event middleware, so the request's span is in the context when the event
// begins:
//
//	PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
//		tracing.RecordTraceID(ginContext.Request.Context(), we)
//	},
func RecordTraceID(ctx context.Context, we *wideevent.Event) {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return
	}
	we.String(KeyTraceID, spanContext.TraceID().String()).String(KeySpanID, spanContext.SpanID().String())
}
