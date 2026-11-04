# govengers/telemetry

The metrics bootstrap and the tracing setup of a govengers service. The README has the API; this page has the order things go in and what is easy to get wrong.

## What goes where

| Question | Answer |
|---|---|
| How many requests, how many failed, how slow? | Prometheus: `ginadapter.NewMetricsMiddleware`, alerts and dashboards |
| What happened in this one request? | the wide event (`wide-event-logger`), one per unit of work |
| Which service made this request slow? | the trace (`tracing`), when the request crossed a service boundary |
| From a log line to its trace? | `trace.id` on the wide event, written by `tracing.RecordTraceID` |

## Bootstrap order in `main`

1. Configuration.
2. `tracing.New`, then `otel.SetTracerProvider` and `otel.SetTextMapPropagator(propagation.TraceContext{})`. A startup failure of `New` (an unsupported protocol) stops the boot before anything listens.
3. `telemetry.NewRegistry`, passed to every component with metrics.
4. Clients, pool, Redis, with their instrumentation.
5. The Gin engine: recovery, `otelgin.Middleware`, the wide-event middleware with the `RecordTraceID` pre-enricher, `ginadapter.NewMetricsMiddleware`, the routes.
6. `telemetry.NewMetricsServer` next to the API server.

## Drain sequence

1. Readiness answers 503, wait for the load balancer.
2. Shut down the API server, then the metrics server (each with a deadline).
3. Close the job worker, the pool and Redis.
4. `traces.Shutdown` with its own deadline, last, so the spans of the drained requests are flushed.

## Gotchas

- **Middleware order.** `otelgin` must run before the wide-event middleware. The other way round, the pre-enricher sees no span and the event has no `trace.id`.
- **Route templates.** Labels and span names use `ginContext.FullPath()`. A route registered with a trailing slash or matched through `RedirectTrailingSlash` is answered before any middleware.
- **Health probes.** Skip them in all three: `SkipRoutes` (metrics), `SkipPaths` (wide events), `otelgin.WithGinFilter` (traces).
- **Redis statements.** `redisotel` records the whole command, keys and values, unless `redisotel.WithDBStatement(false)`.
- **No OpenTelemetry meter provider.** The contrib instrumentations record OpenTelemetry metrics through the global meter provider; leaving it unset keeps them off. Prometheus is the metrics pipeline.
- **Module family.** Bump `go.opentelemetry.io/otel`, `otel/sdk`, `otel/trace` and the OTLP exporters to one minor together, and the `contrib` instrumentations to the release that matches it. The `otel/log` modules are on their own v0 line; this library does not use them.
- **No collector, no traces, no cost.** Without `OTEL_EXPORTER_OTLP_ENDPOINT` the provider is a no-op; an incoming `traceparent` still reaches the wide event through the propagator.
