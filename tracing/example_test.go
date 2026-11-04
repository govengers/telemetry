package tracing_test

import (
	"context"
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
	wideevent "github.com/govengers/wide-event-logger"
	wideeventginadapter "github.com/govengers/wide-event-logger/ginadapter"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/govengers/telemetry/tracing"
)

// In main: build the provider, install it and the W3C propagator, and shut it down last in the
// drain sequence. Without OTEL_EXPORTER_OTLP_ENDPOINT, as here, the provider is a no-op.
func ExampleNew() {
	ctx := context.Background()
	traces, err := tracing.New(ctx, &tracing.Options{ServiceName: "billing", ServiceVersion: "1.4.0"})
	if err != nil {
		fmt.Println(err)
		return
	}
	otel.SetTracerProvider(traces.TracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// ... serve until SIGTERM, drain the servers, then flush the spans:
	shutdownContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := traces.Shutdown(shutdownContext); err != nil {
		fmt.Println(err)
	}
	fmt.Println(traces.Enabled())
	// Output: false
}

// otelgin goes before the wide-event middleware, so the request's span is in the context when
// the PreEnricher records its IDs.
func ExampleRecordTraceID() {
	gin.SetMode(gin.ReleaseMode) // in main, so gin prints no debug lines
	wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{
		Emitter: wideevent.NewJSONEmitter(nil),
		PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
			tracing.RecordTraceID(ginContext.Request.Context(), we)
		},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	engine := gin.New()
	engine.Use(otelgin.Middleware("billing"), wideEvents)
	fmt.Println(len(engine.Handlers))
	// Output: 2
}
