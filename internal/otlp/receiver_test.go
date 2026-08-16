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
	"strings"
	"testing"
	"time"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// testService is the emitting service every fixture here claims to be.
const testService = "shop"

func stringValue(value string) *commonv1.AnyValue {
	return &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: value}}
}

func attribute(key string, value *commonv1.AnyValue) *commonv1.KeyValue {
	return &commonv1.KeyValue{Key: key, Value: value}
}

func TestAnExportBecomesStoreRows(t *testing.T) {
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	export := &collectortrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{{
			Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{
				attribute(AttrServiceName, stringValue(testService)),
				attribute(AttrProject, stringValue(testService)),
				attribute(AttrEnvironment, stringValue("production")),
			}},
			ScopeSpans: []*tracev1.ScopeSpans{{
				Spans: []*tracev1.Span{{
					TraceId:           []byte{0x9d, 0x8d, 0x0f},
					SpanId:            []byte{0x01, 0x02},
					ParentSpanId:      make([]byte, 8), // a root span's parent is the zero id
					Name:              "GET /checkout",
					Kind:              tracev1.Span_SPAN_KIND_SERVER,
					StartTimeUnixNano: uint64(start.UnixNano()),
					EndTimeUnixNano:   uint64(start.Add(420 * time.Millisecond).UnixNano()),
					Status: &tracev1.Status{
						Code:    tracev1.Status_STATUS_CODE_ERROR,
						Message: "boom",
					},
					Attributes: []*commonv1.KeyValue{
						attribute("http.response.status_code",
							&commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 500}}),
						attribute("http.route", stringValue("/checkout")),
					},
				}},
			}},
		}},
	}

	spans := SpansOf(export)
	if len(spans) != 1 {
		t.Fatalf("want one span, got %d", len(spans))
	}
	span := spans[0]

	if span.TraceID != "9d8d0f" || span.SpanID != "0102" {
		t.Fatalf("ids are lower-case hex: %+v", span)
	}
	// The zero parent is what makes a root span findable in the store, and an
	// id of sixteen zeroes would not read as one.
	if span.ParentSpanID != "" {
		t.Fatalf("a root span's parent should be empty, got %q", span.ParentSpanID)
	}
	if span.Kind != "SERVER" || span.StatusCode != clickhouse.StatusError {
		t.Fatalf("the protobuf enum names should be trimmed to the protocol's words: %+v", span)
	}
	if span.DurationMs != 420 {
		t.Fatalf("want 420ms, got %v", span.DurationMs)
	}
	if span.HTTPStatus != 500 {
		t.Fatalf("the status should be lifted into its own column, got %d", span.HTTPStatus)
	}
	if span.Service != testService || span.Project != testService || span.Environment != "production" {
		t.Fatalf("the resource should say where the span ran: %+v", span)
	}
	// The resource travels on every span: a span is read on its own, out of a
	// log line or a waterfall, and a join to find its service would be a join
	// on every read.
	if span.Resource[AttrServiceName] != testService {
		t.Fatalf("the resource attributes should ride along: %+v", span.Resource)
	}
	if span.Attributes["http.route"] != "/checkout" {
		t.Fatalf("the span's own attributes should survive: %+v", span.Attributes)
	}
}

// The older semantic convention is still what most deployed instrumentation
// writes, so both spellings resolve.
func TestEitherSpellingOfTheHTTPStatusIsFound(t *testing.T) {
	for key, want := range map[string]uint16{
		"http.response.status_code": 503,
		"http.status_code":          404,
	} {
		got := httpStatusOf(map[string]string{key: strings.TrimSpace(itoa(want))})
		if got != want {
			t.Fatalf("%s: want %d, got %d", key, want, got)
		}
	}
	if got := httpStatusOf(map[string]string{"http.status_code": "not a number"}); got != 0 {
		t.Fatalf("an unparseable status is no status, got %d", got)
	}
}

// A clock that steps backwards mid-span would otherwise sort that span before
// every real one in the waterfall.
func TestABackwardsSpanHasNoNegativeDuration(t *testing.T) {
	start := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	export := &collectortrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracev1.ResourceSpans{{
			ScopeSpans: []*tracev1.ScopeSpans{{
				Spans: []*tracev1.Span{{
					Name:              "impossible",
					StartTimeUnixNano: uint64(start.UnixNano()),
					EndTimeUnixNano:   uint64(start.Add(-time.Second).UnixNano()),
				}},
			}},
		}},
	}
	spans := SpansOf(export)
	if len(spans) != 1 || spans[0].DurationMs != 0 {
		t.Fatalf("want one span of zero duration, got %+v", spans)
	}
	// A span with no status at all is UNSET, which is what OTLP calls "the
	// application did not say" — not an error.
	if spans[0].StatusCode != clickhouse.StatusUnset {
		t.Fatalf("want an unset status, got %q", spans[0].StatusCode)
	}
}

// Attributes have no schema, so every type gets a rendering and none of them
// is the empty string — a value that was present must never read as absent.
func TestEveryAttributeTypeHasAValue(t *testing.T) {
	attributes := attributesOf([]*commonv1.KeyValue{
		attribute("string", stringValue("x")),
		attribute("bool", &commonv1.AnyValue{Value: &commonv1.AnyValue_BoolValue{BoolValue: true}}),
		attribute("int", &commonv1.AnyValue{Value: &commonv1.AnyValue_IntValue{IntValue: 42}}),
		attribute("double", &commonv1.AnyValue{Value: &commonv1.AnyValue_DoubleValue{DoubleValue: 0.5}}),
		attribute("bytes", &commonv1.AnyValue{Value: &commonv1.AnyValue_BytesValue{BytesValue: []byte{0x01}}}),
		attribute("array", &commonv1.AnyValue{Value: &commonv1.AnyValue_ArrayValue{
			ArrayValue: &commonv1.ArrayValue{Values: []*commonv1.AnyValue{
				stringValue("a"), stringValue("b"),
			}},
		}}),
		attribute("map", &commonv1.AnyValue{Value: &commonv1.AnyValue_KvlistValue{
			KvlistValue: &commonv1.KeyValueList{Values: []*commonv1.KeyValue{
				attribute("inner", stringValue("y")),
			}},
		}}),
	})

	for key, want := range map[string]string{
		"string": "x",
		"bool":   "true",
		"int":    "42",
		// Not "0.50000000", and not exponent notation: a numeric comparison
		// in the log query language would not find either.
		"double": "0.5",
		"bytes":  "AQ==",
		"array":  `["a","b"]`,
		"map":    `{"inner":"y"}`,
	} {
		if got := attributes[key]; got != want {
			t.Fatalf("%s: want %q, got %q", key, want, got)
		}
	}
}

// A span carrying a dump is a span, not a licence to widen the column for
// every row in the batch.
func TestAttributesAreBounded(t *testing.T) {
	many := make([]*commonv1.KeyValue, 0, maxAttributes*2)
	for i := 0; i < maxAttributes*2; i++ {
		many = append(many, attribute("key"+itoa(uint16(i)), stringValue("v")))
	}
	if got := len(attributesOf(many)); got != maxAttributes {
		t.Fatalf("want at most %d attributes, got %d", maxAttributes, got)
	}

	long := attributesOf([]*commonv1.KeyValue{
		attribute("stack", stringValue(strings.Repeat("x", maxValueBytes*2))),
	})
	value := long["stack"]
	if len(value) > maxValueBytes+len(truncationMarker) {
		t.Fatalf("a long value should have been cut, got %d bytes", len(value))
	}
	// A cut value says so rather than looking whole.
	if !strings.HasSuffix(value, truncationMarker) {
		t.Fatal("a truncated value should be marked as one")
	}
}

// Truncation cuts on a rune boundary: half a character would make the row
// unreadable as JSON on the way into the store.
func TestTruncationKeepsValidUTF8(t *testing.T) {
	value := truncate(strings.Repeat("é", maxValueBytes))
	if !strings.HasSuffix(value, truncationMarker) {
		t.Fatal("the value should have been cut")
	}
	body := strings.TrimSuffix(value, truncationMarker)
	if strings.ContainsRune(body, '�') || !utf8Valid(body) {
		t.Fatal("the cut landed inside a character")
	}
}

func utf8Valid(value string) bool {
	for _, r := range value {
		if r == '�' {
			return false
		}
	}
	return true
}

func itoa(value uint16) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
