package ginadapter_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/govengers/telemetry/ginadapter"
)

func newEngine(t *testing.T, registry *prometheus.Registry, skipRoutes ...string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	middleware, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registry, Namespace: "billing", SkipRoutes: skipRoutes})
	require.NoError(t, err)
	engine := gin.New()
	engine.Use(gin.CustomRecoveryWithWriter(io.Discard, func(ginContext *gin.Context, _ any) { ginContext.AbortWithStatus(http.StatusInternalServerError) }), middleware)
	engine.Handle(http.MethodGet, "/api/v1/products/:id", func(ginContext *gin.Context) { ginContext.Status(http.StatusOK) })
	engine.Handle("PROPFIND", "/api/v1/products/:id", func(ginContext *gin.Context) { ginContext.Status(http.StatusOK) })
	engine.GET("/api/health", func(ginContext *gin.Context) { ginContext.Status(http.StatusNoContent) })
	engine.GET("/api/panic", func(*gin.Context) { panic("boom") })
	return engine
}

func serve(t *testing.T, engine *gin.Engine, method, path string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return recorder.Code
}

const requestsHeader = `
# HELP billing_http_requests_total HTTP requests by route template, method and status.
# TYPE billing_http_requests_total counter
`

func TestMiddlewareCountsTheRouteTemplateNeverThePath(t *testing.T) {
	registry := prometheus.NewRegistry()
	engine := newEngine(t, registry)

	serve(t, engine, http.MethodGet, "/api/v1/products/1")
	serve(t, engine, http.MethodGet, "/api/v1/products/2")
	serve(t, engine, http.MethodGet, "/no/such/route")

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(requestsHeader+`
billing_http_requests_total{method="GET",route="/api/v1/products/:id",status="200"} 2
billing_http_requests_total{method="GET",route="unmatched",status="404"} 1
`), "billing_http_requests_total"))
	require.Equal(t, 2, testutil.CollectAndCount(registry, "billing_http_request_duration_seconds"))
}

func TestMiddlewareBoundsTheMethod(t *testing.T) {
	registry := prometheus.NewRegistry()
	engine := newEngine(t, registry)

	serve(t, engine, "PROPFIND", "/api/v1/products/1")

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(requestsHeader+`
billing_http_requests_total{method="other",route="/api/v1/products/:id",status="200"} 1
`), "billing_http_requests_total"))
}

func TestMiddlewareSkipsRoutes(t *testing.T) {
	registry := prometheus.NewRegistry()
	engine := newEngine(t, registry, "/api/health")

	require.Equal(t, http.StatusNoContent, serve(t, engine, http.MethodGet, "/api/health"))

	require.Equal(t, 0, testutil.CollectAndCount(registry, "billing_http_requests_total"))
	require.Equal(t, 0, testutil.CollectAndCount(registry, "billing_http_request_duration_seconds"))
}

func TestMiddlewareCountsAPanicAs500AndRaisesIt(t *testing.T) {
	registry := prometheus.NewRegistry()
	engine := newEngine(t, registry)

	require.Equal(t, http.StatusInternalServerError, serve(t, engine, http.MethodGet, "/api/panic"))

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(requestsHeader+`
billing_http_requests_total{method="GET",route="/api/panic",status="500"} 1
`), "billing_http_requests_total"))
	gauges, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range gauges {
		if family.GetName() == "billing_http_requests_in_flight" {
			require.Zero(t, family.GetMetric()[0].GetGauge().GetValue())
		}
	}
}

func TestMiddlewareCountsInFlightRequests(t *testing.T) {
	registry := prometheus.NewRegistry()
	middleware, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registry, Namespace: "billing"})
	require.NoError(t, err)
	var during float64
	engine := gin.New()
	engine.Use(middleware)
	engine.GET("/slow", func(ginContext *gin.Context) {
		during = gatheredValue(t, registry, "billing_http_requests_in_flight")
		ginContext.Status(http.StatusOK)
	})

	serve(t, engine, http.MethodGet, "/slow")

	require.InDelta(t, 1.0, during, 0)
	require.InDelta(t, 0.0, gatheredValue(t, registry, "billing_http_requests_in_flight"), 0)
}

func gatheredValue(t *testing.T, gatherer prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := gatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no metric %s", name)
	return 0
}

func TestMiddlewareUsesTheBuckets(t *testing.T) {
	registry := prometheus.NewRegistry()
	middleware, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registry, Namespace: "billing", Buckets: []float64{0.1, 1}})
	require.NoError(t, err)
	engine := gin.New()
	engine.Use(middleware)
	engine.GET("/", func(ginContext *gin.Context) { ginContext.Status(http.StatusOK) })

	serve(t, engine, http.MethodGet, "/")

	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "billing_http_request_duration_seconds" {
			require.Len(t, family.GetMetric()[0].GetHistogram().GetBucket(), 2)
		}
	}
}

func TestMetricsPassLint(t *testing.T) {
	registry := prometheus.NewRegistry()
	engine := newEngine(t, registry)
	serve(t, engine, http.MethodGet, "/api/v1/products/1")

	problems, err := testutil.GatherAndLint(registry)

	require.NoError(t, err)
	require.Empty(t, problems)
}

func TestNewMetricsMiddlewareValidatesOptions(t *testing.T) {
	tests := map[string]*ginadapter.MetricsOptions{
		"nil options":   nil,
		"no registerer": {Namespace: "billing"},
		"no namespace":  {Registerer: prometheus.NewRegistry()},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			middleware, err := ginadapter.NewMetricsMiddleware(options)

			require.Error(t, err)
			require.Nil(t, middleware)
		})
	}
}

// failingRegisterer fails the registration of its third collector and records the rest.
type failingRegisterer struct {
	registered, unregistered []prometheus.Collector
}

func (r *failingRegisterer) Register(collector prometheus.Collector) error {
	if len(r.registered) == 2 {
		return errors.New("duplicate")
	}
	r.registered = append(r.registered, collector)
	return nil
}

func (r *failingRegisterer) MustRegister(...prometheus.Collector) { panic("not used") }

func (r *failingRegisterer) Unregister(collector prometheus.Collector) bool {
	r.unregistered = append(r.unregistered, collector)
	return true
}

func TestNewMetricsMiddlewareRegistersAllOrNone(t *testing.T) {
	registerer := &failingRegisterer{}

	middleware, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registerer, Namespace: "billing"})

	require.EqualError(t, err, "telemetry ginadapter: register the billing HTTP metrics: duplicate")
	require.Nil(t, middleware)
	require.Equal(t, registerer.registered, registerer.unregistered)
}

func TestNewMetricsMiddlewareReturnsADuplicate(t *testing.T) {
	registry := prometheus.NewRegistry()
	_, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registry, Namespace: "billing"})
	require.NoError(t, err)

	_, err = ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{Registerer: registry, Namespace: "billing"})

	var alreadyRegistered prometheus.AlreadyRegisteredError
	require.ErrorAs(t, err, &alreadyRegistered)
}
