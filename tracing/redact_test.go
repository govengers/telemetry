package tracing

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRedactingExporterReplacesQueryValues(t *testing.T) {
	tests := map[string]struct {
		attribute attribute.KeyValue
		want      string
	}{
		"url.full with query":    {attribute: attributeURLFull.String("https://idp.example.com/admin/users?email=alice%40example.com&max=1"), want: "https://idp.example.com/admin/users?email=redacted&max=redacted"},
		"url.full without query": {attribute: attributeURLFull.String("https://idp.example.com/token"), want: "https://idp.example.com/token"},
		"url.full unparsable":    {attribute: attributeURLFull.String("http://[::1"), want: RedactedValue},
		"url.query":              {attribute: attributeURLQuery.String("code=secret&state=abc"), want: "code=redacted&state=redacted"},
		"url.query empty":        {attribute: attributeURLQuery.String(""), want: ""},
		"other attribute":        {attribute: attribute.String("http.route", "/users?x=1"), want: "/users?x=1"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(redactingExporter{exporter}))

			_, span := provider.Tracer("test").Start(t.Context(), "call")
			span.SetAttributes(attribute.String("http.request.method", "GET"), test.attribute) // after the start, like otelhttp
			span.End()

			exported := exporter.GetSpans()
			require.Len(t, exported, 1)
			attributes := map[attribute.Key]string{}
			for _, recorded := range exported[0].Attributes {
				attributes[recorded.Key] = recorded.Value.AsString()
			}
			require.Equal(t, map[attribute.Key]string{"http.request.method": "GET", test.attribute.Key: test.want}, attributes)
		})
	}
}

func TestRedactSpanKeepsASpanWithoutURLAttributes(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder), sdktrace.WithSyncer(redactingExporter{exporter}))
	_, span := provider.Tracer("test").Start(t.Context(), "work")
	span.End()

	ended := recorder.Ended()

	require.Len(t, ended, 1)
	require.Equal(t, ended[0], redactSpan(ended[0]))
}
