package tracing

import (
	"context"
	"net/url"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// RedactedValue replaces every query value in url.full and url.query, the same word the
// wide-event middleware writes into request.query.
const RedactedValue = "redacted"

// The attributes whose query values redactingExporter replaces.
const (
	attributeURLFull  attribute.Key = "url.full"
	attributeURLQuery attribute.Key = "url.query"
)

// redactingExporter replaces the query values of url.full and url.query before the spans
// leave the process, keeping the parameter names: otelhttp records the outbound URL with its
// query, which can carry tokens and personal data (an e-mail address in a user search, an
// OAuth code), and span attributes follow the same rule as wide-event fields. It redacts at
// export because instrumentations set these attributes at any time during a span.
type redactingExporter struct {
	sdktrace.SpanExporter
}

// ExportSpans hands the spans on with their URL attributes redacted.
func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	redacted := make([]sdktrace.ReadOnlySpan, len(spans))
	for index, span := range spans {
		redacted[index] = redactSpan(span)
	}
	return e.SpanExporter.ExportSpans(ctx, redacted)
}

// redactedSpan is a span with replaced attributes; everything else is the original's.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
	attributes []attribute.KeyValue
}

// Attributes replaces the original's; every other method is the original span's.
func (s redactedSpan) Attributes() []attribute.KeyValue {
	return s.attributes
}

// redactSpan returns span itself when none of its attributes needs redacting.
func redactSpan(span sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	original := span.Attributes()
	var attributes []attribute.KeyValue // a copy, made on the first change
	for index, recorded := range original {
		value, changed := redactAttribute(recorded)
		if !changed {
			continue
		}
		if attributes == nil {
			attributes = append([]attribute.KeyValue(nil), original...)
		}
		attributes[index] = value
	}
	if attributes == nil {
		return span
	}
	return redactedSpan{ReadOnlySpan: span, attributes: attributes}
}

func redactAttribute(recorded attribute.KeyValue) (attribute.KeyValue, bool) {
	switch recorded.Key {
	case attributeURLFull:
		redacted, changed := redactURL(recorded.Value.AsString())
		return attributeURLFull.String(redacted), changed
	case attributeURLQuery:
		redacted, changed := redactQuery(recorded.Value.AsString())
		return attributeURLQuery.String(redacted), changed
	default:
		return recorded, false
	}
}

// redactURL replaces the query values of rawURL; a URL that does not parse is replaced whole,
// since nothing says which part of it is safe.
func redactURL(rawURL string) (string, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return RedactedValue, true
	}
	if parsed.RawQuery == "" {
		return rawURL, false
	}
	parsed.RawQuery, _ = redactQuery(parsed.RawQuery)
	return parsed.String(), true
}

// redactQuery replaces every value of rawQuery and keeps the names.
func redactQuery(rawQuery string) (string, bool) {
	if rawQuery == "" {
		return rawQuery, false
	}
	query, _ := url.ParseQuery(rawQuery) // keeps the valid pairs of a malformed query
	redacted := make(url.Values, len(query))
	for name := range query {
		redacted[name] = []string{RedactedValue}
	}
	return redacted.Encode(), true
}
