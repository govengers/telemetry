package ginadapter_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/govengers/telemetry/ginadapter"
	"github.com/govengers/telemetry/health"
)

func TestRegisterHealthRoutesServesBothProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	probes, err := health.New(&health.Options{Checks: map[string]health.Check{"postgres": func(context.Context) error { return nil }}})
	require.NoError(t, err)
	engine := gin.New()

	ginadapter.RegisterHealthRoutes(engine, probes)

	require.Equal(t, http.StatusOK, serve(t, engine, http.MethodGet, health.LivenessRoute))
	require.Equal(t, http.StatusOK, serve(t, engine, http.MethodGet, health.ReadinessRoute))
	probes.SetDraining()
	require.Equal(t, http.StatusOK, serve(t, engine, http.MethodGet, health.LivenessRoute))
	require.Equal(t, http.StatusServiceUnavailable, serve(t, engine, http.MethodGet, health.ReadinessRoute))
}
