package health_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/govengers/telemetry/health"
)

func ExampleNew() {
	ping := func(context.Context) error { return nil } // in main: pool.Ping
	probes, err := health.New(&health.Options{Checks: map[string]health.Check{"postgres": ping}})
	if err != nil {
		fmt.Println(err)
		return
	}
	routes := http.NewServeMux()
	routes.Handle("GET "+health.LivenessRoute, probes.LivenessHandler())
	routes.Handle("GET "+health.ReadinessRoute, probes.ReadinessHandler())

	ready := httptest.NewRecorder()
	routes.ServeHTTP(ready, httptest.NewRequestWithContext(context.Background(), http.MethodGet, health.ReadinessRoute, nil))
	fmt.Print(ready.Code, " ", ready.Body.String())

	probes.SetDraining() // first step of the drain sequence
	draining := httptest.NewRecorder()
	routes.ServeHTTP(draining, httptest.NewRequestWithContext(context.Background(), http.MethodGet, health.ReadinessRoute, nil))
	fmt.Print(draining.Code, " ", draining.Body.String())
	// Output:
	// 200 {"status":"ready","checks":{"postgres":"ok"}}
	// 503 {"status":"draining"}
}
