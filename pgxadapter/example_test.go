package pgxadapter_test

import (
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/govengers/telemetry/pgxadapter"
)

func ExampleNewPoolCollector() {
	registry := prometheus.NewRegistry() // in main: telemetry.NewRegistry(), also served by telemetry.NewMetricsServer
	pool := &fakeStat{max: 10}           // in main: the *pgxpool.Pool
	_, err := pgxadapter.NewPoolCollector(&pgxadapter.PoolOptions{
		Registerer: registry,
		Namespace:  "billing",
		Name:       "main",
		Stat:       func() pgxadapter.Stat { return pool }, // in main: return pool.Stat()
	})
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(testutil.CollectAndCount(registry, "billing_db_pool_connections"))
	// Output: 3
}

func ExampleNewTracer() {
	poolConfig, err := pgxpool.ParseConfig("postgres://billing@db.internal/billing")
	if err != nil {
		fmt.Println(err)
		return
	}
	// With traces: multitracer.New(otelpgx.NewTracer(), pgxadapter.NewTracer())
	poolConfig.ConnConfig.Tracer = multitracer.New(pgxadapter.NewTracer())

	// pool, err := pgxpool.NewWithConfig(ctx, poolConfig): every statement run in a context with a
	// wide event (wideevent.FromContext) adds to its db.queries, db.duration_ms and db.slowest_ms
	_, isBatchTracer := poolConfig.ConnConfig.Tracer.(pgx.BatchTracer)
	fmt.Println(isBatchTracer)
	// Output: true
}
