package pgxadapter_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/govengers/telemetry/pgxadapter"
)

// fakeStat stands in for *pgxpool.Stat.
type fakeStat struct {
	acquired, idle, constructing, max int32
	acquires, waits, canceled         int64
	acquireDuration, waitDuration     time.Duration
}

func (s *fakeStat) AcquireCount() int64                 { return s.acquires }
func (s *fakeStat) AcquireDuration() time.Duration      { return s.acquireDuration }
func (s *fakeStat) AcquiredConns() int32                { return s.acquired }
func (s *fakeStat) CanceledAcquireCount() int64         { return s.canceled }
func (s *fakeStat) ConstructingConns() int32            { return s.constructing }
func (s *fakeStat) EmptyAcquireCount() int64            { return s.waits }
func (s *fakeStat) EmptyAcquireWaitTime() time.Duration { return s.waitDuration }
func (s *fakeStat) IdleConns() int32                    { return s.idle }
func (s *fakeStat) MaxConns() int32                     { return s.max }

func TestPoolCollectorReadsTheStatAtScrapeTime(t *testing.T) {
	registry := prometheus.NewRegistry()
	stat := &fakeStat{}
	_, err := pgxadapter.NewPoolCollector(&pgxadapter.PoolOptions{
		Registerer: registry, Namespace: "billing", Name: "main", Stat: func() pgxadapter.Stat { return stat },
	})
	require.NoError(t, err)

	*stat = fakeStat{
		acquired: 3, idle: 5, constructing: 1, max: 10,
		acquires: 120, acquireDuration: 1500 * time.Millisecond,
		waits: 7, waitDuration: 250 * time.Millisecond, canceled: 2,
	}

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP billing_db_pool_acquire_duration_seconds_total Time spent in successful acquires from the pool.
# TYPE billing_db_pool_acquire_duration_seconds_total counter
billing_db_pool_acquire_duration_seconds_total{pool="main"} 1.5
# HELP billing_db_pool_acquire_wait_duration_seconds_total Time successful acquires waited because the pool was empty.
# TYPE billing_db_pool_acquire_wait_duration_seconds_total counter
billing_db_pool_acquire_wait_duration_seconds_total{pool="main"} 0.25
# HELP billing_db_pool_acquire_waits_total Successful acquires that waited because the pool was empty.
# TYPE billing_db_pool_acquire_waits_total counter
billing_db_pool_acquire_waits_total{pool="main"} 7
# HELP billing_db_pool_acquires_total Successful acquires from the pool.
# TYPE billing_db_pool_acquires_total counter
billing_db_pool_acquires_total{pool="main"} 120
# HELP billing_db_pool_canceled_acquires_total Acquires canceled by their context.
# TYPE billing_db_pool_canceled_acquires_total counter
billing_db_pool_canceled_acquires_total{pool="main"} 2
# HELP billing_db_pool_connections Connections in the pool by state.
# TYPE billing_db_pool_connections gauge
billing_db_pool_connections{pool="main",state="acquired"} 3
billing_db_pool_connections{pool="main",state="constructing"} 1
billing_db_pool_connections{pool="main",state="idle"} 5
# HELP billing_db_pool_max_connections Maximum size of the pool.
# TYPE billing_db_pool_max_connections gauge
billing_db_pool_max_connections{pool="main"} 10
`)))
	problems, err := testutil.GatherAndLint(registry)
	require.NoError(t, err)
	require.Empty(t, problems)
}

func TestPoolCollectorSendsNothingWithoutAStat(t *testing.T) {
	registry := prometheus.NewRegistry()
	_, err := pgxadapter.NewPoolCollector(&pgxadapter.PoolOptions{
		Registerer: registry, Namespace: "billing", Name: "main", Stat: func() pgxadapter.Stat { return nil },
	})
	require.NoError(t, err)

	families, err := registry.Gather()

	require.NoError(t, err)
	require.Empty(t, families)
}

func TestNewPoolCollectorRejectsInvalidOptions(t *testing.T) {
	stat := func() pgxadapter.Stat { return &fakeStat{} }
	for name, options := range map[string]*pgxadapter.PoolOptions{
		"nil options":   nil,
		"no registerer": {Namespace: "billing", Name: "main", Stat: stat},
		"no namespace":  {Registerer: prometheus.NewRegistry(), Name: "main", Stat: stat},
		"no name":       {Registerer: prometheus.NewRegistry(), Namespace: "billing", Stat: stat},
		"no stat":       {Registerer: prometheus.NewRegistry(), Namespace: "billing", Name: "main"},
	} {
		t.Run(name, func(t *testing.T) {
			collector, err := pgxadapter.NewPoolCollector(options)

			require.Error(t, err)
			require.Nil(t, collector)
		})
	}
}

func TestTwoPoolsOfOneServiceRegisterSideBySide(t *testing.T) {
	registry := prometheus.NewRegistry()
	for name, size := range map[string]int32{"main": 10, "replica": 4} {
		_, err := pgxadapter.NewPoolCollector(&pgxadapter.PoolOptions{
			Registerer: registry, Namespace: "billing", Name: name, Stat: func() pgxadapter.Stat { return &fakeStat{max: size} },
		})
		require.NoError(t, err)
	}

	require.NoError(t, testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP billing_db_pool_max_connections Maximum size of the pool.
# TYPE billing_db_pool_max_connections gauge
billing_db_pool_max_connections{pool="main"} 10
billing_db_pool_max_connections{pool="replica"} 4
`), "billing_db_pool_max_connections"))
}

func TestThePoolNameTwiceUnderOneNamespaceFailsToRegister(t *testing.T) {
	registry := prometheus.NewRegistry()
	options := &pgxadapter.PoolOptions{Registerer: registry, Namespace: "billing", Name: "main", Stat: func() pgxadapter.Stat { return &fakeStat{} }}
	_, err := pgxadapter.NewPoolCollector(options)
	require.NoError(t, err)

	_, err = pgxadapter.NewPoolCollector(options)

	var alreadyRegistered prometheus.AlreadyRegisteredError
	require.True(t, errors.As(err, &alreadyRegistered), "error %v", err)
}
