package ginadapter_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/govengers/telemetry"
	"github.com/govengers/telemetry/ginadapter"
)

func ExampleNewMetricsMiddleware() {
	registry := telemetry.NewRegistry() // built in main, also served by telemetry.NewMetricsServer
	requestMetrics, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{
		Registerer: registry,
		Namespace:  "billing",
		SkipRoutes: []string{"/api/health"},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	engine := gin.New()
	engine.Use(requestMetrics)
	engine.GET("/api/v1/invoices/:id", func(ginContext *gin.Context) { ginContext.Status(http.StatusOK) })

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/invoices/42", nil))

	fmt.Println(testutil.CollectAndCount(registry, "billing_http_requests_total"))
	// Output: 1
}
