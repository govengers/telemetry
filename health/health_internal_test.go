package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

func TestAStuckCheckIsNotStartedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		probes, err := New(&Options{Checks: map[string]Check{
			"postgres": func(context.Context) error { calls.Add(1); <-release; return nil }, // ignores its context
		}})
		require.NoError(t, err)
		clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		probes.now = func() time.Time { return clock }
		ready := func() *evaluation { return probes.evaluate(t.Context()) }

		require.Equal(t, CheckTimeout, ready().checks["postgres"])
		clock = clock.Add(DefaultCacheFor)
		require.Equal(t, CheckTimeout, ready().checks["postgres"], "still running from the first evaluation")
		require.Equal(t, int32(1), calls.Load(), "not started a second time")

		close(release)
		synctest.Wait() // the first run returns
		clock = clock.Add(DefaultCacheFor)
		require.True(t, ready().ready)
		require.Equal(t, int32(2), calls.Load(), "started again once the first run returned")
	})
}

func TestACheckThatEndsThroughGoexitIsStartedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		probes, err := New(&Options{Checks: map[string]Check{
			"postgres": func(context.Context) error {
				if calls.Add(1) == 1 {
					runtime.Goexit() // what require does in a consumer's test check
				}
				return nil
			},
		}})
		require.NoError(t, err)
		clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
		probes.now = func() time.Time { return clock }

		require.Equal(t, CheckTimeout, probes.evaluate(t.Context()).checks["postgres"])
		clock = clock.Add(DefaultCacheFor)
		require.True(t, probes.evaluate(t.Context()).ready, "the flag cleared when the first run ended")
		require.Equal(t, int32(2), calls.Load())
	})
}
