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

package usage

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// stubExporter keeps what it was handed instead of sending it.
type stubExporter struct {
	mu       sync.Mutex
	batches  []*metricdata.ResourceMetrics
	fail     error
	shutdown bool
}

func (e *stubExporter) Export(_ context.Context, metrics *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail != nil {
		return e.fail
	}
	e.batches = append(e.batches, metrics)
	return nil
}

func (e *stubExporter) Shutdown(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdown = true
	return nil
}

// The endpoint on the Kitchen object is a base URL — the same string
// applications are handed — and OTLP's own path is the exporter's to append.
// Getting this wrong is silent: the collector answers 404 and the samples are
// simply never there.
func TestTheEndpointIsABaseURLTheExporterCompletes(t *testing.T) {
	requests := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		select {
		case requests <- request:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exporter, err := newExporter(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("newExporter: %v", err)
	}
	defer func() {
		if err := exporter.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()

	sweep := Sweep{
		At:    sampledAt,
		Since: sampledAt,
		Containers: []ContainerSample{{
			Project:     appProject,
			Environment: appEnvironment,
			Namespace:   appNamespace,
			Pod:         appPodName,
			Container:   appContainer,
			Node:        appNode,
			Started:     sampledAt,
		}},
	}
	for _, batch := range sweep.ResourceMetrics() {
		if err := exporter.Export(context.Background(), batch); err != nil {
			t.Fatalf("Export: %v", err)
		}
	}

	request := <-requests
	if request.Method != http.MethodPost {
		t.Fatalf("OTLP is a POST, got %s", request.Method)
	}
	if request.URL.Path != "/v1/metrics" {
		t.Fatalf("want OTLP's own metrics path appended to the endpoint, got %q", request.URL.Path)
	}
	if got := request.Header.Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("want the protobuf encoding every collector accepts, got %q", got)
	}
}

// A sweep is one batch per container plus one per environment, because the
// resource is the join and OTLP carries one resource per request.
func TestASweepIsExportedPerResource(t *testing.T) {
	exporter := &stubExporter{}
	collector := collector(appPod(appPodName), appPod("production-abc-2"))

	collector.sweepOnce(context.Background(), exporter)

	if len(exporter.batches) != 3 {
		t.Fatalf("want a batch per container and one for the environment, got %d", len(exporter.batches))
	}

	// Every container batch names the pod it describes by uid. Without it the
	// collector's k8sattributes processor falls back to associating the record
	// with whoever opened the connection — the operator — and stamps its
	// metadata onto a sample about a different pod entirely.
	for _, batch := range exporter.batches {
		attributes := batch.Resource.Set()
		if _, ok := attributes.Value(attrContainer); !ok {
			continue // the environment batch describes no pod
		}
		uid, ok := attributes.Value(attrPodUID)
		if !ok || uid.AsString() == "" {
			t.Errorf("a container batch went out without %s: %v", attrPodUID, attributes.Encoded(attribute.DefaultEncoder()))
		}
	}
}

// A collector that cannot reach the node collector is not a broken collector:
// the export is a gap in the series and the next sweep tries again.
func TestAFailedExportIsNotFatal(t *testing.T) {
	exporter := &stubExporter{fail: errors.New("connection refused")}
	collector := collector(appPod(appPodName))

	collector.sweepOnce(context.Background(), exporter)

	// The sweep still happened, so the next one has a baseline to difference
	// against rather than starting over and losing a restart.
	if len(collector.seen) != 1 {
		t.Fatalf("a dropped export should not cost the restart baseline: %+v", collector.seen)
	}
}
