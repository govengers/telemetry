// Package ginadapter records rate, errors and duration of every Gin request on a Prometheus
// registry the caller passes in. Mount the middleware on the engine with engine.Use, so no
// route can miss it.
package ginadapter

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/govengers/telemetry"
)

// RouteUnmatched is the route label of a request that matched no route, so a client's raw
// path never becomes a label.
const RouteUnmatched = "unmatched"

const subsystem = "http"

// MetricsOptions configures NewMetricsMiddleware.
type MetricsOptions struct {
	Registerer prometheus.Registerer // required; the service's registry, never the default one
	Namespace  string                // required: the service name, e.g. "billing" for billing_http_requests_total
	// SkipRoutes are route templates the middleware records nothing for, such as the health
	// and readiness probes or a socket route whose request lives as long as the socket.
	SkipRoutes []string
	// Buckets of the duration histogram in seconds; nil means prometheus.DefBuckets (5 ms to 10 s).
	Buckets []float64
}

// requestMetrics are the collectors of one NewMetricsMiddleware call.
type requestMetrics struct {
	requests   *prometheus.CounterVec
	durations  *prometheus.HistogramVec
	inFlight   prometheus.Gauge
	skipRoutes map[string]bool
}

// NewMetricsMiddleware registers three collectors on options.Registerer and returns the
// middleware that feeds them:
//
//	<namespace>_http_requests_total{route,method,status}
//	<namespace>_http_request_duration_seconds{route,method}
//	<namespace>_http_requests_in_flight
//
// route is the route template (ginContext.FullPath()) or RouteUnmatched, method is bounded by
// telemetry.MethodLabel, status is the response status code. A request whose handler panics
// counts as 500, and the panic goes on to the recovery middleware. Registration is all or
// none: when one collector fails to register, a duplicate for example, the ones before it are
// unregistered and the error is returned, wrapping the registry's (prometheus.AlreadyRegisteredError
// for a duplicate).
func NewMetricsMiddleware(options *MetricsOptions) (gin.HandlerFunc, error) {
	if options == nil || options.Registerer == nil {
		return nil, errors.New("telemetry ginadapter: Registerer is required")
	}
	if options.Namespace == "" {
		return nil, errors.New("telemetry ginadapter: Namespace is required")
	}
	buckets := options.Buckets
	if buckets == nil {
		buckets = prometheus.DefBuckets
	}
	metrics := &requestMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: options.Namespace, Subsystem: subsystem, Name: "requests_total",
			Help: "HTTP requests by route template, method and status.",
		}, []string{"route", "method", "status"}),
		durations: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: options.Namespace, Subsystem: subsystem, Name: "request_duration_seconds",
			Help: "HTTP request duration by route template and method.", Buckets: buckets,
		}, []string{"route", "method"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: options.Namespace, Subsystem: subsystem, Name: "requests_in_flight",
			Help: "HTTP requests being served.",
		}),
		skipRoutes: make(map[string]bool, len(options.SkipRoutes)),
	}
	for _, route := range options.SkipRoutes {
		metrics.skipRoutes[route] = true
	}
	if err := registerAll(options.Registerer, []prometheus.Collector{metrics.requests, metrics.durations, metrics.inFlight}); err != nil {
		return nil, fmt.Errorf("telemetry ginadapter: register the %s HTTP metrics: %w", options.Namespace, err)
	}
	return metrics.handle, nil
}

func registerAll(registerer prometheus.Registerer, collectors []prometheus.Collector) error {
	for index, collector := range collectors {
		if err := registerer.Register(collector); err != nil {
			for _, registered := range collectors[:index] {
				registerer.Unregister(registered)
			}
			return err
		}
	}
	return nil
}

func (m *requestMetrics) handle(ginContext *gin.Context) {
	route := ginContext.FullPath()
	if m.skipRoutes[route] {
		ginContext.Next()
		return
	}
	if route == "" {
		route = RouteUnmatched
	}
	method := telemetry.MethodLabel(ginContext.Request.Method)
	start := time.Now()
	m.inFlight.Inc()
	defer func() {
		m.inFlight.Dec()
		status := ginContext.Writer.Status()
		recovered := recover()
		if recovered != nil {
			status = http.StatusInternalServerError
		}
		m.requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
		m.durations.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
		if recovered != nil {
			panic(recovered) // the recovery middleware outside answers it
		}
	}()
	ginContext.Next()
}
