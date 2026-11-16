package pgxadapter_test

import (
	"fmt"

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
