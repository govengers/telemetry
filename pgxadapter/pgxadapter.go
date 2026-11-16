// Package pgxadapter instruments a pgx connection pool (github.com/jackc/pgx/v5): Tracer
// records the database work of each unit of work on its wide event, and NewPoolCollector exports
// the pool's statistics as Prometheus metrics, read at scrape time (standards MET-006).
//
// The package is a module of its own, github.com/govengers/telemetry/pgxadapter, so the
// telemetry module stays free of pgx. In main:
//
//	tracer := pgxadapter.NewTracer() // one for every pool
//	poolConfig.ConnConfig.Tracer = multitracer.New(otelpgx.NewTracer(), tracer)
//	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
//	...
//	pgxadapter.NewPoolCollector(&pgxadapter.PoolOptions{
//	    Registerer: registry,
//	    Namespace:  "billing",
//	    Name:       "main",
//	    Stat:       func() pgxadapter.Stat { return pool.Stat() },
//	})
package pgxadapter

import (
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	subsystem = "db_pool"
	poolLabel = "pool"
)

// Stat is the part of *pgxpool.Stat the collector reads.
type Stat interface {
	AcquireCount() int64
	AcquireDuration() time.Duration
	AcquiredConns() int32
	CanceledAcquireCount() int64
	ConstructingConns() int32
	EmptyAcquireCount() int64
	EmptyAcquireWaitTime() time.Duration
	IdleConns() int32
	MaxConns() int32
}

// PoolOptions configures NewPoolCollector.
type PoolOptions struct {
	Registerer prometheus.Registerer // required; the service's registry, never the default one
	Namespace  string                // required: the service name, e.g. "billing" for billing_db_pool_connections
	// Name names the pool in the constant pool label of every metric; required, e.g. "main".
	// Each pool of a service registers its own collector under the same Namespace and its
	// own Name.
	Name string
	// Stat returns a snapshot of the pool's statistics; required. It is called once per
	// scrape: func() pgxadapter.Stat { return pool.Stat() }.
	Stat func() Stat
}

// PoolCollector is the Prometheus collector of one pool. It is safe for concurrent use.
type PoolCollector struct {
	stat                func() Stat
	connections         *prometheus.Desc
	maxConnections      *prometheus.Desc
	acquires            *prometheus.Desc
	acquireDuration     *prometheus.Desc
	acquireWaits        *prometheus.Desc
	acquireWaitDuration *prometheus.Desc
	canceledAcquires    *prometheus.Desc
}

// Values of the state label of <namespace>_db_pool_connections; their sum is the pool's size.
const (
	StateAcquired     = "acquired"
	StateIdle         = "idle"
	StateConstructing = "constructing"
)

// NewPoolCollector registers a collector on options.Registerer that exports, at every scrape,
// each metric with the constant label pool="<options.Name>":
//
//	<namespace>_db_pool_connections{pool,state}          gauge: acquired, idle, constructing
//	<namespace>_db_pool_max_connections                  gauge: the pool's MaxConns
//	<namespace>_db_pool_acquires_total                   successful acquires
//	<namespace>_db_pool_acquire_duration_seconds_total   time spent in successful acquires
//	<namespace>_db_pool_acquire_waits_total              acquires that waited because the pool was empty
//	<namespace>_db_pool_acquire_wait_duration_seconds_total  time those acquires waited
//	<namespace>_db_pool_canceled_acquires_total          acquires canceled by their context
//
// The registration error is returned, wrapping the registry's (prometheus.AlreadyRegisteredError
// for a second collector with the same Namespace and Name).
func NewPoolCollector(options *PoolOptions) (*PoolCollector, error) {
	if options == nil || options.Registerer == nil {
		return nil, errors.New("telemetry pgxadapter: Registerer is required")
	}
	if options.Namespace == "" {
		return nil, errors.New("telemetry pgxadapter: Namespace is required")
	}
	if options.Name == "" {
		return nil, errors.New("telemetry pgxadapter: Name is required")
	}
	if options.Stat == nil {
		return nil, errors.New("telemetry pgxadapter: Stat is required")
	}
	constantLabels := prometheus.Labels{poolLabel: options.Name}
	describe := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(options.Namespace, subsystem, name), help, labels, constantLabels)
	}
	collector := &PoolCollector{
		stat:                options.Stat,
		connections:         describe("connections", "Connections in the pool by state.", "state"),
		maxConnections:      describe("max_connections", "Maximum size of the pool."),
		acquires:            describe("acquires_total", "Successful acquires from the pool."),
		acquireDuration:     describe("acquire_duration_seconds_total", "Time spent in successful acquires from the pool."),
		acquireWaits:        describe("acquire_waits_total", "Successful acquires that waited because the pool was empty."),
		acquireWaitDuration: describe("acquire_wait_duration_seconds_total", "Time successful acquires waited because the pool was empty."),
		canceledAcquires:    describe("canceled_acquires_total", "Acquires canceled by their context."),
	}
	if err := options.Registerer.Register(collector); err != nil {
		return nil, fmt.Errorf("telemetry pgxadapter: register the metrics of the %s pool %q: %w", options.Namespace, options.Name, err)
	}
	return collector, nil
}

// Describe implements prometheus.Collector.
func (c *PoolCollector) Describe(descriptions chan<- *prometheus.Desc) {
	for _, description := range []*prometheus.Desc{
		c.connections, c.maxConnections, c.acquires, c.acquireDuration, c.acquireWaits, c.acquireWaitDuration, c.canceledAcquires,
	} {
		descriptions <- description
	}
}

// Collect implements prometheus.Collector: it takes one snapshot of the pool and sends every
// metric from it.
func (c *PoolCollector) Collect(metrics chan<- prometheus.Metric) {
	stat := c.stat()
	if stat == nil {
		return
	}
	gauge := func(description *prometheus.Desc, value float64, labels ...string) {
		metrics <- prometheus.MustNewConstMetric(description, prometheus.GaugeValue, value, labels...)
	}
	counter := func(description *prometheus.Desc, value float64) {
		metrics <- prometheus.MustNewConstMetric(description, prometheus.CounterValue, value)
	}
	gauge(c.connections, float64(stat.AcquiredConns()), StateAcquired)
	gauge(c.connections, float64(stat.IdleConns()), StateIdle)
	gauge(c.connections, float64(stat.ConstructingConns()), StateConstructing)
	gauge(c.maxConnections, float64(stat.MaxConns()))
	counter(c.acquires, float64(stat.AcquireCount()))
	counter(c.acquireDuration, stat.AcquireDuration().Seconds())
	counter(c.acquireWaits, float64(stat.EmptyAcquireCount()))
	counter(c.acquireWaitDuration, stat.EmptyAcquireWaitTime().Seconds())
	counter(c.canceledAcquires, float64(stat.CanceledAcquireCount()))
}
