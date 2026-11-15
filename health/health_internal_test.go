package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReadinessReusesAResultForCacheFor(t *testing.T) {
	calls := 0
	probes, err := New(&Options{
		CacheFor: 5 * time.Second,
		Checks:   map[string]Check{"postgres": func(context.Context) error { calls++; return nil }},
	})
	require.NoError(t, err)
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	probes.now = func() time.Time { return clock }
	ready := func() int {
		recorder := httptest.NewRecorder()
		probes.ReadinessHandler().ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, ReadinessRoute, nil))
		return recorder.Code
	}

	require.Equal(t, http.StatusOK, ready())
	clock = clock.Add(5*time.Second - time.Nanosecond)
	require.Equal(t, http.StatusOK, ready())
	require.Equal(t, 1, calls, "inside CacheFor")

	clock = clock.Add(time.Nanosecond)
	require.Equal(t, http.StatusOK, ready())
	require.Equal(t, 2, calls, "CacheFor passed")
}

func TestAProbeWaitsForTheRunningEvaluation(t *testing.T) {
	probes, err := New(&Options{})
	require.NoError(t, err)
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	probes.now = func() time.Time { return clock }
	running := make(chan struct{})
	probes.inFlight = running
	finished := &evaluation{checks: map[string]string{"postgres": CheckFailed}, finishedAt: clock}
	results := make(chan *evaluation, 1)

	go func() { results <- probes.evaluate(t.Context()) }()
	probes.mutex.Lock()
	probes.last, probes.inFlight = finished, nil
	probes.mutex.Unlock()
	close(running)

	require.Same(t, finished, <-results, "the waiting probe takes the evaluation it waited for, without running its own")
}
