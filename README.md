# telemetry

[![CI](https://github.com/govengers/telemetry/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/govengers/telemetry/actions/workflows/ci.yml)
[![unit tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/telemetry/badges/unit-tests.json)](https://github.com/govengers/telemetry/actions/workflows/ci.yml)
[![e2e tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/telemetry/badges/e2e-tests.json)](test/e2e)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/telemetry/badges/coverage.json)](https://github.com/govengers/telemetry/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/govengers/telemetry.svg)](https://pkg.go.dev/github.com/govengers/telemetry)
[![License](https://img.shields.io/github/license/govengers/telemetry)](LICENSE)

Metrics bootstrap, health probes and tracing setup for govengers services. Module `github.com/govengers/telemetry`, Go 1.26, Apache 2.0.

One tool owns each signal:

| Signal | Tool | Where |
|---|---|---|
| Logs | wide events ([wide-event-logger](https://github.com/govengers/wide-event-logger)), JSON on stdout | not here |
| Metrics | Prometheus `client_golang`, the service's own registry | `telemetry`, `telemetry/ginadapter` |
| Traces | OpenTelemetry, traces only | `telemetry/tracing` |

No OpenTelemetry metrics or logs SDK, no bridge from OpenTelemetry to Prometheus. The signals meet on the wide event: `trace.id` and `trace.span_id` lead from a log line to its trace.

```sh
go get github.com/govengers/telemetry
go get github.com/govengers/telemetry/pgxadapter # a module of its own, for pgx services
```

## Packages

| Package | Imports | What |
|---|---|---|
| `telemetry` | `client_golang` | `NewRegistry` (Go, process and build-info collectors), `NewMetricsServer` (GET `/metrics` on `:9090`, every timeout set), `MethodLabel` |
| `telemetry/ginadapter` | Gin, `client_golang` | `NewMetricsMiddleware`: rate, errors and duration of every request by route template; `RegisterHealthRoutes` |
| `telemetry/health` | standard library | `New`: liveness and readiness `http.Handler`s, readiness checks with timeout and short cache, `SetDraining` for the drain |
| `telemetry/pgxadapter` (own module) | pgx, `client_golang`, wide-event-logger | `NewTracer`: the database work of each unit of work on its wide event; `NewPoolCollector`: pgx pool statistics as metrics, read at scrape time |
| `telemetry/tracing` | OpenTelemetry API, SDK, OTLP/HTTP exporter | `New` (tracer provider from `OTEL_*`, no-op without an endpoint), `RecordTraceID` (trace ID on the wide event) |

A service that only wants metrics imports `telemetry` and pulls in neither Gin nor the OpenTelemetry SDK. `pgxadapter` is the module `github.com/govengers/telemetry/pgxadapter`, so the telemetry module does not require pgx.

## Metrics

```go
// main
registry := telemetry.NewRegistry() // every component gets it as its prometheus.Registerer
requestMetrics, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{
    Registerer: registry,
    Namespace:  "billing",
    SkipRoutes: []string{health.LivenessRoute, health.ReadinessRoute},
})
if err != nil {
    return err
}
metricsServer, err := telemetry.NewMetricsServer(&telemetry.MetricsServerOptions{Registry: registry})
if err != nil {
    return err
}
// run metricsServer.ListenAndServe next to the API server; metricsServer.Shutdown(ctx) in the drain
```

The middleware exports:

```text
billing_http_requests_total{route,method,status}
billing_http_request_duration_seconds{route,method}
billing_http_requests_in_flight
```

`route` is the route template (`/api/v1/invoices/:id`), or `unmatched` when no route matched, never the raw path. `method` is one of the nine standard methods or `other`. `status` is the status code; a handler that panics counts as `500` and the panic goes on to the recovery middleware. `SkipRoutes` are route templates: probes, or a socket route whose request lives as long as the socket. `Buckets` defaults to `prometheus.DefBuckets`.

The metrics server serves only `GET /metrics`, in the OpenMetrics format when Prometheus asks for it, on every interface (Prometheus scrapes from inside the cluster; the Ingress never routes the port). `telemetry.MethodLabel` bounds the method label of outbound-call metrics the same way.

### Pool metrics

```go
// main, after the pool is built
_, err = pgxadapter.NewPoolCollector(&pgxadapter.PoolOptions{
    Registerer: registry,
    Namespace:  "billing",
    Name:       "main", // the pool label; a second pool registers its own collector with its own Name
    Stat:       func() pgxadapter.Stat { return pool.Stat() },
})
if err != nil {
    return err
}
```

The collector calls `Stat` once per scrape, so acquires pay nothing for the metrics. Every metric carries the constant label `pool` (`pool="main"`):

```text
billing_db_pool_connections{pool,state}                 acquired, idle, constructing (their sum is the pool's size)
billing_db_pool_max_connections
billing_db_pool_acquires_total
billing_db_pool_acquire_duration_seconds_total
billing_db_pool_acquire_waits_total                     acquires that waited because the pool was empty
billing_db_pool_acquire_wait_duration_seconds_total
billing_db_pool_canceled_acquires_total
```

`Stat` is an interface with the methods of `*pgxpool.Stat` the collector reads, so the collector is tested without a database. Saturation is `connections{state="acquired"} / ignoring(state) max_connections` per pool; a rising `rate(acquire_waits_total)` means requests queue for a connection.

## Database work on the wide event

```go
// main, before the pool is built
poolConfig.ConnConfig.Tracer = multitracer.New(otelpgx.NewTracer(), pgxadapter.NewTracer()) // github.com/jackc/pgx/v5/multitracer
pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
```

Every statement run with a context that carries a wide event (`wideevent.FromContext`) adds to the event's totals, so the event of a request or a job answers "how much database work, how slow, did it fail" without a line in the repositories (standards LOG-004):

| Field | Value |
|---|---|
| `db.queries` | statements run: each `Exec`, `Query`, `QueryRow`, each statement of a batch, each `CopyFrom`; `begin` and `commit` count too |
| `db.duration_ms` | the sum of their times, in milliseconds |
| `db.slowest_ms` | the longest single call; a batch is timed as one call |
| `db.failure` | the class of the first failed statement: `canceled`, `timeout`, `connection`, `server`, `client` |
| `db.sqlstate` | the SQLSTATE of that failure when the server rejected it (`23505` for a unique violation) |

No SQL text, arguments or error text: a server error's text carries the values of the failed row. A statement's time ends when its rows are closed, so it includes reading them; waiting for a pool connection is not part of it (the pool metrics show that). Statements that run concurrently add up. Statements outside a unit of work (migrations, startup) record nothing, and those that end after the event was emitted are dropped. The keys are constants (`pgxadapter.KeyQueries`, ...); the tracer owns them, a repository writes its own `db.*` fields next to them (`db.operation`, `db.tx`).

## Health probes

```go
// main, after the pool and Redis
probes, err := health.New(&health.Options{
    Checks: map[string]health.Check{
        "postgres": pool.Ping,
        "redis":    func(ctx context.Context) error { return redisClient.Ping(ctx).Err() },
    },
    OnCheckFailure: func(name string, err error) { logger.Warn("readiness check failed", "check", name, "error", err.Error()) },
})
if err != nil {
    return err
}
ginadapter.RegisterHealthRoutes(engine, probes) // at the root, outside authentication
// net/http: routes.Handle("GET "+health.ReadinessRoute, probes.ReadinessHandler())
```

| Route | Answers |
|---|---|
| `GET /api/health` (`health.LivenessRoute`) | always `200 ok`: the process serves HTTP. It checks no dependency, so a database outage does not restart every pod |
| `GET /api/ready` (`health.ReadinessRoute`) | `200 {"status":"ready","checks":{"postgres":"ok"}}`; `503` with status `unavailable` and the check's `failed` or `timeout` when one fails; `503 {"status":"draining"}` after `SetDraining` |

The checks run concurrently under one `Timeout` (default 800 ms, below the Kubernetes default `timeoutSeconds: 1`; a larger `Timeout` needs a larger `timeoutSeconds`, or the kubelet times out before the 503 body arrives). A check that ignores its context still counts as `timeout` when the timeout passes, and until its run returns, later evaluations report it as `timeout` without starting it again; a panicking check counts as `failed`. Concurrent probes share one evaluation and a result answers further probes for `CacheFor` (default 1 s), so a flood of requests to the unauthenticated route does not ping the database once each. The body names the checks but never carries their error text; `OnCheckFailure` gets the error; two evaluations can call it at the same time, so it must be safe for concurrent use. `SetDraining` makes readiness fail without running the checks and leaves liveness at 200; see the [drain sequence](docs/wiki/Home.md#drain-sequence).

## Tracing

Tracing pays off when a request crosses a service boundary: the service calls another service over the network, or is called by one. Inside one binary the wide event's `db.*`, `cache.*` and `http.*` fields already cover a request.

```go
// main, before the components that trace
traces, err := tracing.New(ctx, &tracing.Options{ServiceName: "billing", ServiceVersion: version})
if err != nil {
    return err
}
otel.SetTracerProvider(traces.TracerProvider())
otel.SetTextMapPropagator(propagation.TraceContext{})

// last in the drain sequence, after the servers stopped
shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
defer cancel()
if err := traces.Shutdown(shutdownContext); err != nil {
    logger.Error("trace flush failed", "error", err.Error())
}
```

`New` sets no global state. The two `otel.Set*` lines are main's choice; the alternative is to pass `traces.TracerProvider()` and `propagation.TraceContext{}` to every instrumentation through its options (`otelgin.WithTracerProvider`, `otelhttp.WithTracerProvider`, ...), which the [end-to-end test](test/e2e/telemetry_test.go) does.

### Configuration

Tracing is configured through the standard OpenTelemetry environment variables, not the service's own configuration:

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | the collector; **neither set means tracing is off**: `New` returns a no-op provider and no error |
| `OTEL_SDK_DISABLED=true`, `OTEL_TRACES_EXPORTER=none` | tracing off |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `..._TRACES_PROTOCOL` | only `http/protobuf` (the default); `grpc` is an error at startup |
| `OTEL_EXPORTER_OTLP_HEADERS`, `..._TIMEOUT`, `..._INSECURE`, `..._CERTIFICATE` | read by the OTLP/HTTP exporter |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | override and extend the resource (`service.name`, `service.version`, `deployment.environment.name`, ...) |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | the SDK default is `parentbased_always_on`; keep it and tail-sample in the collector |

The SDK keeps every span and the collector decides which traces to keep (tail sampling that keeps errors and slow traces), like the wide-event sampler keeps every failure.

Query values in `url.full` and `url.query` are replaced by `redacted` before the spans leave the process: `otelhttp` records outbound URLs with their query, which can carry tokens or an e-mail address.

### Instrumentation

Use the OpenTelemetry contrib instrumentations directly; the library does not wrap them.

```go
// Gin: otelgin first, before the recovery and the wide-event middleware, so the span is in the
// context when the event begins and sees the 500 of a panic
wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{
    Emitter: emitter,
    PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
        tracing.RecordTraceID(ginContext.Request.Context(), we)
    },
})
engine.Use(
    otelgin.Middleware("billing", otelgin.WithGinFilter(func(ginContext *gin.Context) bool {
        route := ginContext.FullPath() // probes make no traces
        return route != health.LivenessRoute && route != health.ReadinessRoute
    })),
    recovery, // inside the span: otelgin does not recover, so outside it a panic's span would show no error
    wideEvents,
    requestMetrics,
)

// outbound HTTP: the transport starts a client span and sends traceparent
client := &http.Client{Timeout: 10 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}

// pgx: parameterized SQL, never the parameters (otelpgx leaves them out by default), next to the wide-event tracer
poolConfig.ConnConfig.Tracer = multitracer.New(otelpgx.NewTracer(), pgxadapter.NewTracer())

// Redis: without the command text, which holds the keys and values
err = redisotel.InstrumentTracing(redisClient, redisotel.WithDBStatement(false))
```

`otelgin` names server spans `GET /api/v1/invoices/:id` after the route template. Do not install an OpenTelemetry `MeterProvider`: `otelgin`, `otelhttp` and `otelpgx` then record no OpenTelemetry metrics, and Prometheus stays the one metrics pipeline. For Redis pool metrics use `redisprometheus.NewCollector` from `github.com/redis/go-redis/extra/redisprometheus/v9` on the service's registry, not `redisotel.InstrumentMetrics`.

With the no-op provider and the propagator installed, an incoming `traceparent` still reaches the wide event: the request's events carry the caller's trace ID even when this service exports nothing.

### Job queues

`github.com/govengers/jobs/oteladapter` carries the trace context from the enqueuing request into the job and opens one span per attempt; it writes `trace.id` and `trace.span_id` onto the job's wide event the same way.

## Docs

- [docs/wiki/Home.md](docs/wiki/Home.md): wiring order, the drain sequence, what goes where, gotchas.
- [docs/design.md](docs/design.md): design decisions.
- [llms.txt](llms.txt): the short version for agents.
- Rules: [govengers/standards](https://github.com/govengers/standards) MET (metrics), TRC (tracing), LOG (wide events), LAY-003 and LAY-004 (drain, probes).

## License

Apache 2.0, see [LICENSE](LICENSE).
