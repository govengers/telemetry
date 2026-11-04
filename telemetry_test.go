package telemetry_test

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/govengers/telemetry"
)

func TestNewRegistryHasTheRuntimeCollectors(t *testing.T) {
	registry := telemetry.NewRegistry()

	families, err := registry.Gather()

	require.NoError(t, err)
	names := map[string]bool{}
	for _, family := range families {
		names[family.GetName()] = true
	}
	require.True(t, names["go_goroutines"], "Go collector")
	require.True(t, names["go_build_info"], "build-info collector")
}

func TestNewRegistryReturnsAnIndependentRegistry(t *testing.T) {
	first, second := telemetry.NewRegistry(), telemetry.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_total", Help: "A test counter."})

	require.NoError(t, first.Register(counter))
	require.NoError(t, second.Register(counter))
}

func TestNewMetricsServerRequiresARegistry(t *testing.T) {
	for name, options := range map[string]*telemetry.MetricsServerOptions{"nil options": nil, "nil registry": {}} {
		t.Run(name, func(t *testing.T) {
			server, err := telemetry.NewMetricsServer(options)

			require.Error(t, err)
			require.Nil(t, server)
		})
	}
}

func TestNewMetricsServerBoundsEveryPhase(t *testing.T) {
	server, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: telemetry.NewRegistry()})

	require.NoError(t, err)
	require.Equal(t, telemetry.DefaultMetricsAddress, server.Addr)
	require.Equal(t, 64<<10, server.MaxHeaderBytes)
	require.Positive(t, server.ReadHeaderTimeout)
	require.Positive(t, server.ReadTimeout)
	require.Positive(t, server.WriteTimeout)
	require.Positive(t, server.IdleTimeout)
	require.NotNil(t, server.ErrorLog, "a discarding logger, not the standard one")
}

func TestNewMetricsServerKeepsAddressAndErrorLog(t *testing.T) {
	errorLog := log.New(&bytes.Buffer{}, "", 0)

	server, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: telemetry.NewRegistry(), Address: "127.0.0.1:9191", ErrorLog: errorLog})

	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:9191", server.Addr)
	require.Same(t, errorLog, server.ErrorLog)
}

func TestMetricsServerServesOnlyGetMetrics(t *testing.T) {
	registry := telemetry.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Namespace: "billing", Name: "checkouts_total", Help: "Checkouts."})
	registry.MustRegister(counter)
	counter.Inc()
	server, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: registry})
	require.NoError(t, err)

	tests := map[string]struct {
		method, path string
		wantStatus   int
		wantBody     string
	}{
		"metrics":         {method: http.MethodGet, path: "/metrics", wantStatus: http.StatusOK, wantBody: "billing_checkouts_total 1"},
		"other path":      {method: http.MethodGet, path: "/debug/pprof/", wantStatus: http.StatusNotFound},
		"other method":    {method: http.MethodPost, path: "/metrics", wantStatus: http.StatusMethodNotAllowed},
		"handler counter": {method: http.MethodGet, path: "/metrics", wantStatus: http.StatusOK, wantBody: "promhttp_metric_handler_errors_total"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()

			server.Handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), test.method, test.path, nil))

			require.Equal(t, test.wantStatus, recorder.Code)
			require.Contains(t, recorder.Body.String(), test.wantBody)
		})
	}
}

func TestMetricsServerAnswersOpenMetricsWhenAsked(t *testing.T) {
	server, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: telemetry.NewRegistry()})
	require.NoError(t, err)
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	request.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	recorder := httptest.NewRecorder()

	server.Handler.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/openmetrics-text"), recorder.Header().Get("Content-Type"))
	require.True(t, strings.HasSuffix(strings.TrimSpace(recorder.Body.String()), "# EOF"))
}

func TestRegistryPassesLint(t *testing.T) {
	problems, err := testutil.GatherAndLint(telemetry.NewRegistry())

	require.NoError(t, err)
	require.Empty(t, problems)
}

func TestMethodLabel(t *testing.T) {
	tests := map[string]string{
		http.MethodGet: http.MethodGet, http.MethodHead: http.MethodHead, http.MethodPost: http.MethodPost,
		http.MethodPut: http.MethodPut, http.MethodPatch: http.MethodPatch, http.MethodDelete: http.MethodDelete,
		http.MethodConnect: http.MethodConnect, http.MethodOptions: http.MethodOptions, http.MethodTrace: http.MethodTrace,
		"PROPFIND": telemetry.MethodOther, "get": telemetry.MethodOther, "": telemetry.MethodOther,
	}
	for method, want := range tests {
		t.Run(method, func(t *testing.T) {
			require.Equal(t, want, telemetry.MethodLabel(method))
		})
	}
}
