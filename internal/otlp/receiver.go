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

// Package otlp implements the platform's trace receiver: the OTLP/HTTP
// endpoint instrumented applications export spans to, and the translation from
// a span on the wire to a row in the telemetry store.
//
// Traces are the one telemetry Kitchen cannot collect on an application's
// behalf. Logs are on the node, flows are in the CNI, resource usage is in the
// kubelet — but only the application knows that this request was a checkout
// and that it spent 380 of its 420 milliseconds waiting for a database. The
// L7 data Hubble already ships could be reshaped into something trace-like,
// and it would answer none of that; it would only make the trace view look
// populated.
//
// So the platform does the half it can do: the receiver is always running, and
// every environment is handed OTLP's standard environment variables pointing
// at it, so instrumenting an application is adding its language's SDK and
// nothing else. There is no collector to deploy and no endpoint to configure.
//
// The endpoint is in-cluster and unauthenticated, like every OTLP collector
// deployment: it is not published on the Gateway, and what it accepts is spans
// from workloads that are already inside the cluster.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	// TracesPath is OTLP/HTTP's own path for spans. It is fixed by the
	// protocol: an SDK given a base endpoint appends it.
	TracesPath = "/v1/traces"

	// DefaultPort is OTLP/HTTP's registered port, so that an application
	// configured with nothing but a hostname still finds the receiver.
	DefaultPort = 4318

	// maxBodySize bounds one export. The OTLP SDKs batch to a few hundred
	// spans, which is orders of magnitude below this.
	maxBodySize = 8 << 20

	// How the writer paces itself: spans wait at most flushInterval or until
	// flushBatch of them have arrived, and bufferLimit is where a store that
	// is not accepting them stops costing the operator memory.
	flushInterval = 5 * time.Second
	flushBatch    = 500
	bufferLimit   = 20000

	// configPollInterval is how often the receiver re-reads where the store
	// is, so that configuring one does not need a restart.
	configPollInterval = 30 * time.Second
)

// Receiver serves OTLP/HTTP and batches spans into the telemetry store. It
// runs as a manager Runnable on every replica: exporting is a data path, not a
// decision, and every replica behind the Service should accept it.
type Receiver struct {
	Client client.Client
	// BindAddr for the HTTP server, e.g. ":4318".
	BindAddr string

	mu     sync.Mutex
	buffer []clickhouse.Span
	// dropped counts spans refused since the last time it was reported, so a
	// store that cannot keep up says so once a flush rather than once a span.
	dropped int

	// store is where accepted spans go, re-resolved on a timer. Nil means
	// this installation has no telemetry store, and exports are refused
	// rather than accepted and discarded.
	storeMu sync.RWMutex
	store   *clickhouse.Client
	enabled bool
}

// Start implements manager.Runnable.
func (r *Receiver) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              r.BindAddr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	go r.follow(ctx)

	r.log().Info("starting otlp receiver", "addr", r.BindAddr, "path", TracesPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica
// accepts exports. Unlike the flow and usage collectors this cannot
// double-count — each span is exported to exactly one replica.
func (r *Receiver) NeedLeaderElection() bool { return false }

func (r *Receiver) log() logr.Logger { return logf.Log.WithName("otlp") }

// Handler builds the routed handler. It is exported so the receiver can be
// exercised without binding a port.
func (r *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+TracesPath, r.receiveTraces)
	// A GET on the path is what a person checks the endpoint with, and a
	// health probe is what the platform checks it with.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// follow keeps the store connection and the on/off switch in step with the
// Kitchen object, and flushes what has been received.
func (r *Receiver) follow(ctx context.Context) {
	r.resolve(ctx)

	config := time.NewTicker(configPollInterval)
	defer config.Stop()
	flush := time.NewTicker(flushInterval)
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			// Whatever is held has a moment to land: an operator restarting
			// should not lose the last five seconds of every trace.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			r.flush(shutdownCtx)
			cancel()
			return
		case <-config.C:
			r.resolve(ctx)
		case <-flush.C:
			r.flush(ctx)
		}
	}
}

// resolve reads the receiver's configuration off the Kitchen singleton.
func (r *Receiver) resolve(ctx context.Context) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		r.log().V(1).Info("cannot read the trace configuration", "reason", err.Error())
		return
	}

	var store *clickhouse.Client
	if ref := kitchen.Spec.Observability.ClickHouse.SecretRef; ref != nil {
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: controller.PlatformNamespace, Name: ref.Name}
		if err := r.Client.Get(ctx, key, secret); err != nil {
			r.log().V(1).Info("cannot read the telemetry secret", "reason", err.Error())
			return
		}
		cfg, err := clickhouse.ConfigFromSecret(secret)
		if err != nil {
			r.log().V(1).Info("unusable telemetry secret", "reason", err.Error())
			return
		}
		store = clickhouse.New(cfg)
	}

	r.storeMu.Lock()
	r.store, r.enabled = store, kitchen.Spec.Observability.Traces.Enabled
	r.storeMu.Unlock()
}

// destination is where spans currently go, and whether they are wanted.
func (r *Receiver) destination() (*clickhouse.Client, bool) {
	r.storeMu.RLock()
	defer r.storeMu.RUnlock()
	return r.store, r.enabled
}

// receiveTraces accepts one OTLP export.
//
// Both encodings the protocol defines are accepted: protobuf, which every SDK
// uses by default, and JSON, which is what a `curl` reproducing a problem will
// send. The answer is an empty ExportTraceServiceResponse in the same encoding,
// which is OTLP's "all of it was accepted".
func (r *Receiver) receiveTraces(w http.ResponseWriter, req *http.Request) {
	store, enabled := r.destination()
	switch {
	case !enabled:
		// A refusal the exporter will retry from, rather than a silent
		// success: an application shipping spans nowhere should be able to
		// find out that it is.
		http.Error(w, "this installation does not collect traces", http.StatusServiceUnavailable)
		return
	case store == nil:
		http.Error(w, "this installation has no telemetry store to keep traces in", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize+1))
	if err != nil {
		http.Error(w, "unreadable request body", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodySize {
		http.Error(w, "export too large; send smaller batches", http.StatusRequestEntityTooLarge)
		return
	}

	export := &collectortrace.ExportTraceServiceRequest{}
	contentType := req.Header.Get("Content-Type")
	isJSON := strings.Contains(contentType, "application/json")
	if isJSON {
		// Unknown fields are ignored on purpose: OTLP's own compatibility
		// promise is that a newer exporter may send a field this build has
		// never heard of, and dropping the whole batch over one would be the
		// wrong reading of it.
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, export)
	} else {
		err = proto.Unmarshal(body, export)
	}
	if err != nil {
		http.Error(w, fmt.Sprintf("unreadable OTLP export: %v", err), http.StatusBadRequest)
		return
	}

	spans := SpansOf(export)
	rejected := r.accept(spans)

	// OTLP's partial-success field is how a receiver says "I kept some of
	// that", which is exactly what a full buffer means.
	response := &collectortrace.ExportTraceServiceResponse{}
	if rejected > 0 {
		response.PartialSuccess = &collectortrace.ExportTracePartialSuccess{
			RejectedSpans: int64(rejected),
			ErrorMessage:  "the telemetry store is not keeping up; some spans were dropped",
		}
	}

	var encoded []byte
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		encoded, err = protojson.Marshal(response)
	} else {
		w.Header().Set("Content-Type", "application/x-protobuf")
		encoded, err = proto.Marshal(response)
	}
	if err != nil {
		http.Error(w, "cannot encode the response", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// accept buffers spans and reports how many did not fit. It flushes eagerly
// once a batch has accumulated, so a busy platform does not wait out the
// ticker for every batch.
func (r *Receiver) accept(spans []clickhouse.Span) int {
	r.mu.Lock()
	room := bufferLimit - len(r.buffer)
	rejected := 0
	if len(spans) > room {
		rejected = len(spans) - room
		r.dropped += rejected
		spans = spans[:max(room, 0)]
	}
	r.buffer = append(r.buffer, spans...)
	full := len(r.buffer) >= flushBatch
	r.mu.Unlock()

	if full {
		// On the request's own goroutine, and deliberately: an exporter that
		// is outrunning the store should feel it in its export latency rather
		// than in a silently growing buffer. The write is not the request's
		// context, which ends with the response — the store's own client
		// timeout is what bounds it.
		r.flush(context.Background())
	}
	return rejected
}

// flush writes what has been received. A failed write puts nothing back: the
// spans are gone, which is a gap in the trace view, and the alternative is a
// buffer that grows until the operator is killed for it.
func (r *Receiver) flush(ctx context.Context) {
	store, _ := r.destination()

	r.mu.Lock()
	batch, dropped := r.buffer, r.dropped
	r.buffer, r.dropped = nil, 0
	r.mu.Unlock()

	if dropped > 0 {
		r.log().V(1).Info("spans dropped", "spans", dropped, "reason", "receive buffer full")
	}
	if len(batch) == 0 || store == nil {
		return
	}
	if err := store.InsertSpans(ctx, batch); err != nil {
		r.log().V(1).Info("span batch dropped", "spans", len(batch), "reason", err.Error())
	}
}

// Attribute keys the platform reads out of a span or its resource. The
// `kitchen.*` pair is what the Environment reconciler puts into every
// application's OTEL_RESOURCE_ATTRIBUTES, which is how a trace knows which
// project and environment it ran in without the application being told.
const (
	AttrServiceName = "service.name"
	AttrProject     = "kitchen.project"
	AttrEnvironment = "kitchen.environment"
)

// httpStatusKeys are the spellings of an HTTP response status across semantic
// convention versions, newest first. It is lifted into a column of its own so
// that "show me the 500s" is a comparison rather than a map lookup over every
// span in the window.
var httpStatusKeys = []string{"http.response.status_code", "http.status_code"}

// SpansOf flattens an OTLP export into store rows.
//
// Resource attributes are carried onto every span rather than kept in a table
// of their own: a span is read on its own — out of a log line, out of a
// waterfall — and a join to find out which service emitted it would be a join
// on every read of every trace.
func SpansOf(export *collectortrace.ExportTraceServiceRequest) []clickhouse.Span {
	spans := []clickhouse.Span{}
	for _, resourceSpans := range export.GetResourceSpans() {
		resource := attributesOf(resourceSpans.GetResource().GetAttributes())
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			for _, span := range scopeSpans.GetSpans() {
				attributes := attributesOf(span.GetAttributes())
				start := time.Unix(0, int64(span.GetStartTimeUnixNano())) // #nosec G115 -- nanoseconds since the epoch fit an int64 until the year 2262
				end := time.Unix(0, int64(span.GetEndTimeUnixNano()))     // #nosec G115 -- as above

				duration := end.Sub(start)
				if duration < 0 {
					// A clock that went backwards mid-span. Zero is wrong;
					// a negative duration is wrong *and* sorts before every
					// real span in a waterfall.
					duration = 0
				}

				spans = append(spans, clickhouse.Span{
					Timestamp:     start.UTC(),
					TraceID:       hexOf(span.GetTraceId()),
					SpanID:        hexOf(span.GetSpanId()),
					ParentSpanID:  hexOf(span.GetParentSpanId()),
					Name:          span.GetName(),
					Kind:          kindOf(span.GetKind().String()),
					Service:       resource[AttrServiceName],
					Project:       resource[AttrProject],
					Environment:   resource[AttrEnvironment],
					DurationMs:    float64(duration.Nanoseconds()) / 1e6,
					StatusCode:    statusOf(span.GetStatus().GetCode().String()),
					StatusMessage: span.GetStatus().GetMessage(),
					HTTPStatus:    httpStatusOf(attributes),
					Attributes:    attributes,
					Resource:      resource,
				})
			}
		}
	}
	return spans
}

// hexOf renders a trace or span id the way every trace tool spells it: lower
// case hex, and empty for the zero id a root span's parent is.
func hexOf(id []byte) string {
	empty := true
	for _, b := range id {
		if b != 0 {
			empty = false
			break
		}
	}
	if empty {
		return ""
	}
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(id)*2)
	for _, b := range id {
		out = append(out, digits[b>>4], digits[b&0x0f])
	}
	return string(out)
}

// kindOf and statusOf trim the protobuf enum names down to the words the
// protocol documents — SPAN_KIND_SERVER is SERVER, STATUS_CODE_ERROR is ERROR
// — because those are what a person filtering the trace list will type.
func kindOf(name string) string {
	name = strings.TrimPrefix(name, "SPAN_KIND_")
	if name == "" || name == "UNSPECIFIED" {
		return ""
	}
	return name
}

func statusOf(name string) string {
	name = strings.TrimPrefix(name, "STATUS_CODE_")
	if name == "" {
		return clickhouse.StatusUnset
	}
	return name
}

func httpStatusOf(attributes map[string]string) uint16 {
	for _, key := range httpStatusKeys {
		value, ok := attributes[key]
		if !ok {
			continue
		}
		status, err := strconv.ParseUint(strings.TrimSpace(value), 10, 16)
		if err != nil {
			continue
		}
		return uint16(status)
	}
	return 0
}
