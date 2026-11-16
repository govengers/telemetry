# govengers/telemetry

The metrics bootstrap, the health probes and the tracing setup of a govengers service. The README has the API; this page has the order things go in and what is easy to get wrong.

## What goes where

| Question | Answer |
|---|---|
| How many requests, how many failed, how slow? | Prometheus: `ginadapter.NewMetricsMiddleware`, alerts and dashboards |
| What happened in this one request? | the wide event (`wide-event-logger`), one per unit of work |
| Which service made this request slow? | the trace (`tracing`), when the request crossed a service boundary |
| From a log line to its trace? | `trace.id` on the wide event, written by `tracing.RecordTraceID` |
| Should the kubelet restart the pod? | liveness, `health.LivenessHandler`: 200 while the process serves HTTP |
| Should the load balancer send it traffic? | readiness, `health.ReadinessHandler`: the checks, and 503 once draining |
| How much database work did this request do, and did it fail? | `pgxadapter.NewTracer`: `db.queries`, `db.duration_ms`, `db.slowest_ms`, `db.failure`, `db.sqlstate` on the wide event |
| Is the database pool the bottleneck? | `pgxadapter.NewPoolCollector`: `<ns>_db_pool_*{pool}` |

## Bootstrap order in `main`

1. Configuration.
2. `tracing.New`, then `otel.SetTracerProvider` and `otel.SetTextMapPropagator(propagation.TraceContext{})`. A startup failure of `New` (an unsupported protocol) stops the boot before anything listens.
3. `telemetry.NewRegistry`, passed to every component with metrics.
4. Clients, pool, Redis, with their instrumentation: `poolConfig.ConnConfig.Tracer = multitracer.New(otelpgx.NewTracer(), pgxadapter.NewTracer())` before the pool is built, `pgxadapter.NewPoolCollector` on the registry right after it, one per pool with its own `Name`.
5. `health.New` with a check per dependency the pod cannot serve without (`pool.Ping`, the Redis ping).
6. The Gin engine: `otelgin.Middleware`, the recovery, the wide-event middleware with the `RecordTraceID` pre-enricher, `ginadapter.NewMetricsMiddleware`, then `ginadapter.RegisterHealthRoutes` at the root before the authentication middleware, then the routes.
7. `telemetry.NewMetricsServer` next to the API server.

## Drain sequence

On SIGTERM (standards LAY-003):

1. `probes.SetDraining()`: readiness answers 503 from now on, liveness stays 200.
2. Keep serving for a pause (about 5 s, configurable) so endpoints and load balancers see the pod leave and stop routing to it. Withdrawing readiness and closing the listener in the same instant changes nothing: requests already on their way get connection errors.
3. Shut down the API server, then the metrics server (each with a deadline). Close WebSockets yourself, `Shutdown` does not.
4. Stop the job worker and wait for it, then close the pool and Redis.
5. `traces.Shutdown` with its own deadline, last, so the spans of the drained requests are flushed.

The pause, the server deadlines and the trace flush add up to less than the pod's `terminationGracePeriodSeconds` (default 30 s): for example 5 s + 20 s + 3 s.

```go
<-ctx.Done() // SIGTERM
probes.SetDraining()
time.Sleep(configuration.DrainPause) // keep serving while the pod leaves the endpoints
shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
defer cancel()
err := errors.Join(apiServer.Shutdown(shutdownContext), metricsServer.Shutdown(shutdownContext))
```

## Gotchas

- **Middleware order.** `otelgin` runs first. After the wide-event middleware, the pre-enricher sees no span and the event has no `trace.id`. Inside the recovery, a panicking request's span ends before the recovery writes the 500, so the trace shows no error: `otelgin` does not recover.
- **Route templates.** Labels and span names use `ginContext.FullPath()`. A route registered with a trailing slash or matched through `RedirectTrailingSlash` is answered before any middleware.
- **Health probes.** Skip `health.LivenessRoute` and `health.ReadinessRoute` in all three: `SkipRoutes` (metrics), `SkipPaths` (wide events), `otelgin.WithGinFilter` (traces). A readiness check then runs without a parent span; an instrumented client may still start a root span for the ping (the Redis `PING` does unless `redisotel.WithCommandFilter` drops it).
- **What readiness checks.** Only what the pod cannot serve any request without: its database, its Redis. Not a downstream service: when that one is down every replica turns unready at once and the whole service goes dark instead of degrading. `pool.Ping` waits for a free connection, so an exhausted pool times the check out; when every replica's pool is exhausted at once, all of them turn unready together. Keep `Timeout` well above the normal ping time.
- **Probe timeout.** The probe's `timeoutSeconds` must stay above `Timeout` (default 800 ms against the Kubernetes default of 1 s). With `Timeout: 2 * time.Second` set `timeoutSeconds: 3`, or a hung database shows up in the kubelet's events as a client timeout instead of the 503 body naming the check.
- **Liveness checks nothing.** A liveness probe that pings the database restarts every pod during a database outage, and the restarts make the recovery slower.
- **Readiness body.** It is unauthenticated, so it names the checks and never carries their errors (hostnames, addresses). Record the error through `OnCheckFailure`, which runs once per evaluation, not once per probe, and can run in two evaluations at the same time.
- **pgxadapter is a module of its own.** `go get github.com/govengers/telemetry/pgxadapter`; it is tagged `pgxadapter/vX.Y.Z` and requires pgx v5.12.0 or newer.
- **The database fields need the event in the statement's context.** The Gin middleware and the jobs middleware put it there; a repository that runs its statements with `context.Background()` or a context it built itself records nothing. Work detached with `context.WithoutCancel` keeps the event, and its statements that end after the event was emitted are dropped.
- **One tracer, several pools.** The fields add up every pool's statements of a unit of work. The pool label separates only the pool metrics.
- **db.duration_ms is not wall time.** It is the sum of the statements' times: statements run concurrently add up, and a query's time includes reading its rows until `Close`.
- **Redis statements.** `redisotel` records the whole command, keys and values, unless `redisotel.WithDBStatement(false)`.
- **No OpenTelemetry meter provider.** The contrib instrumentations record OpenTelemetry metrics through the global meter provider; leaving it unset keeps them off. Prometheus is the metrics pipeline.
- **Module family.** Bump `go.opentelemetry.io/otel`, `otel/sdk`, `otel/trace` and the OTLP exporters to one minor together, and the `contrib` instrumentations to the release that matches it. The `otel/log` modules are on their own v0 line; this library does not use them.
- **No collector, no traces, no cost.** Without `OTEL_EXPORTER_OTLP_ENDPOINT` the provider is a no-op; an incoming `traceparent` still reaches the wide event through the propagator.
