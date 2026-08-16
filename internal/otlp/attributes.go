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
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
)

// OTLP attributes are a typed tree; the store keeps them as Map(String,
// String), for the same reason a log line's structured fields are strings: an
// attribute has no schema the platform can rely on, and the query language
// compares numerically wherever it is asked to. What matters is that a value
// that was present never reads as absent, so every type has a rendering and
// none of them is the empty string.

const (
	// maxAttributes bounds one span's attributes. A span carrying hundreds is
	// a dump rather than a span, and every key it introduces becomes a part of
	// the column the whole batch writes.
	maxAttributes = 128
	// maxValueBytes bounds one attribute's value. Stack traces arrive this
	// way, and a truncated one is still the useful part.
	maxValueBytes = 8 << 10
)

// truncationMarker says a value was cut rather than leaving it looking whole.
const truncationMarker = "…[truncated]"

// attributesOf flattens OTLP attributes into the store's map.
func attributesOf(attributes []*commonv1.KeyValue) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	out := make(map[string]string, min(len(attributes), maxAttributes))
	for _, attribute := range attributes {
		if len(out) >= maxAttributes {
			break
		}
		key := strings.TrimSpace(attribute.GetKey())
		if key == "" {
			continue
		}
		out[key] = truncate(valueOf(attribute.GetValue()))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// valueOf renders one attribute value.
//
// Composite values keep their JSON rather than being flattened into more keys:
// an array attribute is one attribute in every tool that shows it, and
// exploding it into `tags.0`, `tags.1` would make a query over it depend on
// where in the list the value happened to land.
func valueOf(value *commonv1.AnyValue) string {
	switch typed := value.GetValue().(type) {
	case *commonv1.AnyValue_StringValue:
		return typed.StringValue
	case *commonv1.AnyValue_BoolValue:
		return strconv.FormatBool(typed.BoolValue)
	case *commonv1.AnyValue_IntValue:
		return strconv.FormatInt(typed.IntValue, 10)
	case *commonv1.AnyValue_DoubleValue:
		// 'f' with -1 precision keeps 0.5 as "0.5" rather than as
		// "0.50000000", and keeps a large value out of exponent notation,
		// where a numeric comparison in the query language would not find it.
		return strconv.FormatFloat(typed.DoubleValue, 'f', -1, 64)
	case *commonv1.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(typed.BytesValue)
	case *commonv1.AnyValue_ArrayValue:
		items := make([]string, 0, len(typed.ArrayValue.GetValues()))
		for _, item := range typed.ArrayValue.GetValues() {
			items = append(items, valueOf(item))
		}
		return encode(items)
	case *commonv1.AnyValue_KvlistValue:
		return encode(attributesOf(typed.KvlistValue.GetValues()))
	}
	return ""
}

func encode(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func truncate(value string) string {
	if len(value) <= maxValueBytes {
		return value
	}
	// Cut on a rune boundary: a half-written multi-byte character would make
	// the whole row unreadable as JSON on the way into the store.
	cut := maxValueBytes
	for cut > 0 && !utf8Start(value[cut]) {
		cut--
	}
	return value[:cut] + truncationMarker
}

// utf8Start reports whether a byte begins a UTF-8 sequence: an ASCII byte, or
// a leading byte, but never a continuation.
func utf8Start(b byte) bool { return b&0xc0 != 0x80 }
