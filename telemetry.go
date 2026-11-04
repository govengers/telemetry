// Package telemetry is the metrics bootstrap of a govengers service: the Prometheus registry
// with the runtime collectors, the internal /metrics server and the bounded method label.
//
// One tool owns each signal. Wide events (github.com/govengers/wide-event-logger) are the logs,
// Prometheus is the metrics, OpenTelemetry is used for traces only. The subpackages add the
// rest: ginadapter records rate, errors and duration of every Gin request, tracing sets up the
// OpenTelemetry tracer provider from the standard OTEL_* environment variables and puts the
// trace ID on the wide event. This package imports client_golang and nothing else, so a
// service that only wants metrics pulls in neither Gin nor the OpenTelemetry SDK.
//
// The package has no global state: main builds the registry, passes it to every component
// through its options and runs and shuts down the metrics server itself.
package telemetry

import (
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultMetricsAddress is the metrics server's address when MetricsServerOptions.Address is
// empty: every interface, port 9090, which the Service exposes to Prometheus and the Ingress
// never routes.
const DefaultMetricsAddress = ":9090"

// maximumHeaderBytes bounds the request line and headers a metrics server reads.
const maximumHeaderBytes = 64 << 10

// NewRegistry returns a new registry with the Go runtime, process and build-info collectors
// registered. Every component gets it as its prometheus.Registerer, never the default registry.
func NewRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister( // a fresh registry cannot hold a duplicate
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	return registry
}

// MetricsServerOptions configures NewMetricsServer.
type MetricsServerOptions struct {
	// Registry is served on GET /metrics; required. The handler's own error counter
	// (promhttp_metric_handler_errors_total) is registered on it too.
	Registry *prometheus.Registry
	// Address is the listen address; empty means DefaultMetricsAddress.
	Address string
	// ErrorLog receives the server's connection errors and the handler's gathering errors; nil
	// discards them.
	ErrorLog *log.Logger
}

// NewMetricsServer returns a server that serves GET /metrics of options.Registry, in the
// OpenMetrics format when the scraper asks for it, and 404 on every other path. Every phase
// of a connection is bounded, so a slow or idle client cannot hold it. It does not listen: the
// caller runs ListenAndServe and calls Shutdown in its drain sequence.
func NewMetricsServer(options *MetricsServerOptions) (*http.Server, error) {
	if options == nil || options.Registry == nil {
		return nil, errors.New("telemetry metrics server: Registry is required")
	}
	address := options.Address
	if address == "" {
		address = DefaultMetricsAddress
	}
	errorLog := options.ErrorLog
	if errorLog == nil {
		errorLog = log.New(io.Discard, "", 0)
	}
	handlerOptions := promhttp.HandlerOpts{Registry: options.Registry, EnableOpenMetrics: true, ErrorLog: errorLog}
	routes := http.NewServeMux()
	routes.Handle("GET /metrics", promhttp.HandlerFor(options.Registry, handlerOptions))
	return &http.Server{
		Addr:              address,
		Handler:           routes,
		MaxHeaderBytes:    maximumHeaderBytes,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          errorLog,
	}, nil
}

// MethodOther is the method label of every method outside the standard set.
const MethodOther = "other"

// MethodLabel bounds an HTTP method to a label value: one of the nine standard methods, or
// MethodOther for anything a client made up, so a client cannot create series at will.
func MethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return MethodOther
	}
}
