# telemetry

[![CI](https://github.com/govengers/telemetry/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/govengers/telemetry/actions/workflows/ci.yml)
[![unit tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/telemetry/badges/unit-tests.json)](https://github.com/govengers/telemetry/actions/workflows/ci.yml)
[![e2e tests](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/telemetry/badges/e2e-tests.json)](test/e2e)
[![coverage](https://img.shields.io/endpoint?url=https://raw.githubusercontent.com/govengers/telemetry/badges/coverage.json)](https://github.com/govengers/telemetry/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/govengers/telemetry.svg)](https://pkg.go.dev/github.com/govengers/telemetry)
[![License](https://img.shields.io/github/license/govengers/telemetry)](LICENSE)

Metrics bootstrap and tracing setup for govengers services. Module `github.com/govengers/telemetry`, Go 1.26, Apache 2.0.

One tool owns each signal:

| Signal | Tool | Where |
|---|---|---|
| Logs | wide events ([wide-event-logger](https://github.com/govengers/wide-event-logger)), JSON on stdout | not here |
| Metrics | Prometheus `client_golang`, the service's own registry | `telemetry`, `telemetry/ginadapter` |
| Traces | OpenTelemetry, traces only | `telemetry/tracing` |

No OpenTelemetry metrics or logs SDK, no bridge from OpenTelemetry to Prometheus. The signals meet on the wide event: `trace.id` and `trace.span_id` lead from a log line to its trace.

```sh
go get github.com/govengers/telemetry
```

## Packages

| Package | Imports | What |
|---|---|---|
| `telemetry` | `client_golang` | `NewRegistry` (Go, process and build-info collectors), `NewMetricsServer` (GET `/metrics` on `:9090`, every timeout set), `MethodLabel` |
| `telemetry/ginadapter` | Gin, `client_golang` | `NewMetricsMiddleware`: rate, errors and duration of every request by route template |
| `telemetry/tracing` | OpenTelemetry API, SDK, OTLP/HTTP exporter | `New` (tracer provider from `OTEL_*`, no-op without an endpoint), `RecordTraceID` (trace ID on the wide event) |

A service that only wants metrics imports `telemetry` and pulls in neither Gin nor the OpenTelemetry SDK.

## Metrics

```go
// main
registry := telemetry.NewRegistry() // every component gets it as its prometheus.Registerer
requestMetrics, err := ginadapter.NewMetricsMiddleware(&ginadapter.MetricsOptions{
    Registerer: registry,
    Namespace:  "billing",
    SkipRoutes: []string{"/api/health", "/api/ready"},
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
// Gin: otelgin before the wide-event middleware, so the span is in the context when the event begins
wideEvents, err := wideeventginadapter.NewMiddleware(&wideeventginadapter.Options{
    Emitter: emitter,
    PreEnricher: func(we *wideevent.Event, ginContext *gin.Context) {
        tracing.RecordTraceID(ginContext.Request.Context(), we)
    },
})
engine.Use(
    otelgin.Middleware("billing", otelgin.WithGinFilter(func(ginContext *gin.Context) bool {
        return ginContext.FullPath() != "/api/health" // probes make no traces
    })),
    wideEvents,
    requestMetrics,
)

// outbound HTTP: the transport starts a client span and sends traceparent
client := &http.Client{Timeout: 10 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport)}

// pgx: parameterized SQL, never the parameters (otelpgx leaves them out by default)
poolConfig.ConnConfig.Tracer = otelpgx.NewTracer()

// Redis: without the command text, which holds the keys and values
err = redisotel.InstrumentTracing(redisClient, redisotel.WithDBStatement(false))
```

`otelgin` names server spans `GET /api/v1/invoices/:id` after the route template. Do not install an OpenTelemetry `MeterProvider`: `otelgin`, `otelhttp` and `otelpgx` then record no OpenTelemetry metrics, and Prometheus stays the one metrics pipeline. For Redis pool metrics use `redisprometheus.NewCollector` from `github.com/redis/go-redis/extra/redisprometheus/v9` on the service's registry, not `redisotel.InstrumentMetrics`.

With the no-op provider and the propagator installed, an incoming `traceparent` still reaches the wide event: the request's events carry the caller's trace ID even when this service exports nothing.

### Job queues

`github.com/govengers/jobs/oteladapter` carries the trace context from the enqueuing request into the job and opens one span per attempt; it writes `trace.id` and `trace.span_id` onto the job's wide event the same way.

## Docs

- [docs/wiki/Home.md](docs/wiki/Home.md): wiring order, the drain sequence, what goes where, gotchas.
- [llms.txt](llms.txt): the short version for agents.
- Rules: [govengers/standards](https://github.com/govengers/standards) MET (metrics), TRC (tracing), LOG (wide events).

## License

Apache 2.0, see [LICENSE](LICENSE).
