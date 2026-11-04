// Package otlptest is a fake OTLP/HTTP trace receiver for the tests: it decodes every export
// request and keeps its spans.
package otlptest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Receiver is an httptest server on /v1/traces.
type Receiver struct {
	*httptest.Server

	mutex         sync.Mutex
	resourceSpans []*tracev1.ResourceSpans
}

// NewReceiver starts a receiver; the caller closes it.
func NewReceiver() *Receiver {
	receiver := &Receiver{}
	routes := http.NewServeMux()
	routes.HandleFunc("POST /v1/traces", receiver.receive)
	receiver.Server = httptest.NewServer(routes)
	return receiver
}

func (r *Receiver) receive(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	export := &collectortrace.ExportTraceServiceRequest{}
	if err := proto.Unmarshal(body, export); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	r.mutex.Lock()
	r.resourceSpans = append(r.resourceSpans, export.GetResourceSpans()...)
	r.mutex.Unlock()
	response, err := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = writer.Write(response) // a failed write fails the exporter, which the test sees
}

// ResourceSpans returns what the receiver has received so far.
func (r *Receiver) ResourceSpans() []*tracev1.ResourceSpans {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]*tracev1.ResourceSpans(nil), r.resourceSpans...)
}

// Spans returns every received span.
func (r *Receiver) Spans() []*tracev1.Span {
	var spans []*tracev1.Span
	for _, resourceSpans := range r.ResourceSpans() {
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			spans = append(spans, scopeSpans.GetSpans()...)
		}
	}
	return spans
}

// ResourceAttribute returns the string value of key in the first received resource.
func (r *Receiver) ResourceAttribute(key string) string {
	for _, resourceSpans := range r.ResourceSpans() {
		for _, attribute := range resourceSpans.GetResource().GetAttributes() {
			if attribute.GetKey() == key {
				return attribute.GetValue().GetStringValue()
			}
		}
	}
	return ""
}
