package ginadapter

import (
	"github.com/gin-gonic/gin"

	"github.com/govengers/telemetry/health"
)

// RegisterHealthRoutes registers the probes at the engine's root: GET health.LivenessRoute and
// GET health.ReadinessRoute. Register them before, and never behind, the authentication
// middleware (LAY-004), and pass both routes to MetricsOptions.SkipRoutes, the wide-event
// middleware's SkipPaths and the otelgin filter, so probes make no metrics, events or spans.
func RegisterHealthRoutes(engine *gin.Engine, probes *health.Health) {
	engine.GET(health.LivenessRoute, gin.WrapH(probes.LivenessHandler()))
	engine.GET(health.ReadinessRoute, gin.WrapH(probes.ReadinessHandler()))
}
