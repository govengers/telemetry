package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/govengers/telemetry/health"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type response struct {
	statusCode int
	status     string
	checks     map[string]string
	raw        string
}

func probe(t *testing.T, handler http.Handler) response {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.ReadinessRoute, nil))
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	return response{statusCode: recorder.Code, status: body.Status, checks: body.Checks, raw: recorder.Body.String()}
}

func newHealth(t *testing.T, options *health.Options) *health.Health {
	t.Helper()
	probes, err := health.New(options)
	require.NoError(t, err)
	return probes
}

func passing(context.Context) error { return nil }

func TestLivenessAnswersOKEvenWhileDraining(t *testing.T) {
	probes := newHealth(t, &health.Options{Checks: map[string]health.Check{
		"postgres": func(context.Context) error { return errors.New("down") },
	}})
	probes.SetDraining()
	recorder := httptest.NewRecorder()

	probes.LivenessHandler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, health.LivenessRoute, nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "ok\n", recorder.Body.String())
}

func TestReadinessIsReadyWhenEveryCheckPasses(t *testing.T) {
	probes := newHealth(t, &health.Options{Checks: map[string]health.Check{"postgres": passing, "redis": passing}})

	got := probe(t, probes.ReadinessHandler())

	require.Equal(t, http.StatusOK, got.statusCode)
	require.Equal(t, health.StatusReady, got.status)
	require.Equal(t, map[string]string{"postgres": health.CheckOK, "redis": health.CheckOK}, got.checks)
}

func TestReadinessWithoutChecksIsReady(t *testing.T) {
	for name, options := range map[string]*health.Options{"nil options": nil, "no checks": {}} {
		t.Run(name, func(t *testing.T) {
			got := probe(t, newHealth(t, options).ReadinessHandler())

			require.Equal(t, http.StatusOK, got.statusCode)
			require.Equal(t, health.StatusReady, got.status)
		})
	}
}

func TestReadinessReportsAFailedCheckWithoutItsError(t *testing.T) {
	var reported []string
	probes := newHealth(t, &health.Options{
		Checks: map[string]health.Check{
			"postgres": func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: connection refused") },
			"redis":    passing,
		},
		OnCheckFailure: func(name string, err error) { reported = append(reported, name+": "+err.Error()) },
	})

	got := probe(t, probes.ReadinessHandler())

	require.Equal(t, http.StatusServiceUnavailable, got.statusCode)
	require.Equal(t, health.StatusUnavailable, got.status)
	require.Equal(t, map[string]string{"postgres": health.CheckFailed, "redis": health.CheckOK}, got.checks)
	require.NotContains(t, got.raw, "10.0.0.5", "error text stays out of the unauthenticated body")
	require.Equal(t, []string{"postgres: dial tcp 10.0.0.5:5432: connection refused"}, reported)
}

func TestReadinessTimesOutASlowCheck(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	probes := newHealth(t, &health.Options{
		Timeout: time.Millisecond,
		Checks: map[string]health.Check{
			"honours the context": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			"ignores the context": func(context.Context) error { <-release; return nil },
		},
	})

	got := probe(t, probes.ReadinessHandler())

	require.Equal(t, http.StatusServiceUnavailable, got.statusCode)
	require.Equal(t, map[string]string{"honours the context": health.CheckTimeout, "ignores the context": health.CheckTimeout}, got.checks)
}

func TestReadinessTurnsAPanickingCheckIntoAFailure(t *testing.T) {
	var reported error
	probes := newHealth(t, &health.Options{
		Checks:         map[string]health.Check{"broken": func(context.Context) error { panic("nil pool") }},
		OnCheckFailure: func(_ string, err error) { reported = err },
	})

	got := probe(t, probes.ReadinessHandler())

	require.Equal(t, http.StatusServiceUnavailable, got.statusCode)
	require.Equal(t, map[string]string{"broken": health.CheckFailed}, got.checks)
	require.ErrorContains(t, reported, "nil pool")
}

func TestReadinessFailsWithoutCheckingOnceDraining(t *testing.T) {
	var calls atomic.Int32
	probes := newHealth(t, &health.Options{Checks: map[string]health.Check{
		"postgres": func(context.Context) error { calls.Add(1); return nil },
	}})
	require.Equal(t, http.StatusOK, probe(t, probes.ReadinessHandler()).statusCode)

	probes.SetDraining()
	got := probe(t, probes.ReadinessHandler())

	require.Equal(t, http.StatusServiceUnavailable, got.statusCode)
	require.Equal(t, health.StatusDraining, got.status)
	require.Nil(t, got.checks)
	require.Equal(t, int32(1), calls.Load())
}

// serve runs one readiness request and returns its status code; safe off the test goroutine.
func serve(handler http.Handler) int {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, health.ReadinessRoute, nil))
	return recorder.Code
}

func TestConcurrentProbesShareOneEvaluation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		probes := newHealth(t, &health.Options{Checks: map[string]health.Check{
			"postgres": func(context.Context) error { calls.Add(1); <-release; return nil },
		}})
		handler := probes.ReadinessHandler()

		var group sync.WaitGroup
		for range 8 {
			group.Go(func() {
				if code := serve(handler); code != http.StatusOK {
					t.Errorf("status %d, want the shared evaluation's 200", code)
				}
			})
		}
		synctest.Wait() // every probe runs the evaluation or waits for it
		close(release)
		group.Wait()

		require.Equal(t, int32(1), calls.Load())
	})
}

func TestReadinessFailsWhenDrainingStartsDuringTheEvaluation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		probes := newHealth(t, &health.Options{Checks: map[string]health.Check{
			"postgres": func(context.Context) error { <-release; return nil },
		}})
		codes := make(chan int, 1)
		go func() { codes <- serve(probes.ReadinessHandler()) }()
		synctest.Wait() // the probe is past the draining flag, inside the check

		probes.SetDraining()
		close(release)

		require.Equal(t, http.StatusServiceUnavailable, <-codes)
	})
}

func TestAPanickingOnCheckFailureDoesNotBlockLaterProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		probes := newHealth(t, &health.Options{
			Checks:         map[string]health.Check{"postgres": func(context.Context) error { return errors.New("down") }},
			OnCheckFailure: func(string, error) { panic("logger bug") },
		})
		require.PanicsWithValue(t, "logger bug", func() { serve(probes.ReadinessHandler()) })

		got := probe(t, probes.ReadinessHandler()) // blocked forever (a synctest deadlock) before the fix

		require.Equal(t, http.StatusServiceUnavailable, got.statusCode)
		require.Equal(t, map[string]string{"postgres": health.CheckFailed}, got.checks)
	})
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	for name, options := range map[string]*health.Options{
		"negative timeout":   {Timeout: -time.Second},
		"negative cache":     {CacheFor: -time.Second},
		"check without name": {Checks: map[string]health.Check{"": passing}},
		"nil check":          {Checks: map[string]health.Check{"postgres": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			probes, err := health.New(options)

			require.Error(t, err)
			require.Nil(t, probes)
		})
	}
}
