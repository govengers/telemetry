package telemetry_test

import (
	"fmt"

	"github.com/govengers/telemetry"
)

// main builds one registry, hands it to every component as their prometheus.Registerer and
// serves it on the internal metrics listener, which it runs and drains itself.
func ExampleNewMetricsServer() {
	registry := telemetry.NewRegistry()
	metricsServer, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: registry})
	if err != nil {
		fmt.Println(err)
		return
	}
	// go metricsServer.ListenAndServe(), and metricsServer.Shutdown(ctx) in the drain sequence
	fmt.Println(metricsServer.Addr)
	// Output: :9090
}

func ExampleMethodLabel() {
	fmt.Println(telemetry.MethodLabel("GET"), telemetry.MethodLabel("BREW"))
	// Output: GET other
}
