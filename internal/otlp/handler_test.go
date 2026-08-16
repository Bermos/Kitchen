/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package otlp

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// fakeStore stands in for the telemetry store and records what was written.
type fakeStore struct {
	server *httptest.Server
	writes []string
}

func newFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	store := &fakeStore{}
	store.server = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		store.writes = append(store.writes, string(body))
	}))
	t.Cleanup(store.server.Close)
	return store
}

// receiver builds one wired to the fake store, with no Kitchen object to read:
// what resolve() would have set is set directly.
func (s *fakeStore) receiver(t *testing.T) *Receiver {
	t.Helper()
	endpoint, err := url.Parse(s.server.URL)
	if err != nil {
		t.Fatalf("parsing the test server URL: %v", err)
	}
	return &Receiver{
		store: clickhouse.New(clickhouse.Config{
			Host:     endpoint.Hostname(),
			HTTPPort: endpoint.Port(),
			Database: "kitchen",
			Username: "kitchen",
		}),
		enabled: true,
	}
}

func oneSpan() *collectortrace.ExportTraceServiceRequest {
	return &collectortrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{{
			Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{
				attribute(AttrServiceName, stringValue(testService)),
			}},
			ScopeSpans: []*tracev1.ScopeSpans{{
				Spans: []*tracev1.Span{{
					TraceId:           []byte{0x9d, 0x8d, 0x0f},
					SpanId:            []byte{0x01},
					Name:              "GET /checkout",
					StartTimeUnixNano: 1_786_874_400_000_000_000,
					EndTimeUnixNano:   1_786_874_400_420_000_000,
				}},
			}},
		}},
	}
}

// Protobuf is what every SDK sends by default.
func TestAProtobufExportIsAccepted(t *testing.T) {
	store := newFakeStore(t)
	receiver := store.receiver(t)

	body, err := proto.Marshal(oneSpan())
	if err != nil {
		t.Fatalf("marshalling the export: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, TracesPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("the answer should be in the encoding it was asked in, got %q", got)
	}
	response := &collectortrace.ExportTraceServiceResponse{}
	if err := proto.Unmarshal(recorder.Body.Bytes(), response); err != nil {
		t.Fatalf("the answer is not an OTLP response: %v", err)
	}
	if response.GetPartialSuccess().GetRejectedSpans() != 0 {
		t.Fatalf("nothing should have been rejected: %v", response.GetPartialSuccess())
	}

	// A single span is under the batch threshold, so nothing has been written
	// yet — the flush is what writes.
	receiver.flush(t.Context())
	if len(store.writes) != 1 || !strings.Contains(store.writes[0], "GET /checkout") {
		t.Fatalf("the span should have reached the store, got %v", store.writes)
	}
}

// JSON is what a person reproducing a problem with curl will send.
func TestAJSONExportIsAccepted(t *testing.T) {
	store := newFakeStore(t)
	receiver := store.receiver(t)

	body, err := protojson.Marshal(oneSpan())
	if err != nil {
		t.Fatalf("marshalling the export: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, TracesPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("the answer should be in the encoding it was asked in, got %q", got)
	}
	receiver.flush(t.Context())
	if len(store.writes) != 1 {
		t.Fatalf("the span should have reached the store, got %v", store.writes)
	}
}

// An installation without a store, or with traces switched off, refuses the
// export rather than accepting it and dropping it: an application shipping
// spans nowhere should be able to find out.
func TestAnExportWithNowhereToGoIsRefused(t *testing.T) {
	for name, receiver := range map[string]*Receiver{
		"traces off": {enabled: false},
		"no store":   {enabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, TracesPath, strings.NewReader(""))
			req.Header.Set("Content-Type", "application/x-protobuf")
			recorder := httptest.NewRecorder()
			receiver.Handler().ServeHTTP(recorder, req)

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("want 503, got %d", recorder.Code)
			}
		})
	}
}

func TestAnUnreadableExportIsTheCallersError(t *testing.T) {
	store := newFakeStore(t)
	receiver := store.receiver(t)

	req := httptest.NewRequest(http.MethodPost, TracesPath, strings.NewReader("this is not protobuf"))
	req.Header.Set("Content-Type", "application/x-protobuf")
	recorder := httptest.NewRecorder()
	receiver.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", recorder.Code)
	}
}

// A store that is not keeping up costs the operator a bounded amount of
// memory, and the export says how much of it did not fit — OTLP has a field
// for exactly that, and reporting a full success would be a lie.
func TestAFullBufferIsReportedAsPartialSuccess(t *testing.T) {
	store := newFakeStore(t)
	receiver := store.receiver(t)
	receiver.buffer = make([]clickhouse.Span, bufferLimit-1)

	rejected := receiver.accept([]clickhouse.Span{
		{Name: "the last one that fits"},
		{Name: "one too many"},
		{Name: "and another"},
	})
	if rejected != 2 {
		t.Fatalf("want two rejected spans, got %d", rejected)
	}
	// A buffer at its limit is past the batch threshold, so accepting the
	// span that filled it is also what empties it.
	if len(receiver.buffer) != 0 {
		t.Fatalf("a full buffer should have been flushed, %d spans left", len(receiver.buffer))
	}
	if len(store.writes) != 1 || !strings.Contains(store.writes[0], "the last one that fits") {
		t.Fatalf("the spans that fit should have reached the store, got %d writes", len(store.writes))
	}
}
