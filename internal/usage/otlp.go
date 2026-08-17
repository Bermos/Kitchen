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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/version"
)

// The metrics this package exports. The read path asks for them by these exact
// names, so the name is an interface between two packages — which is why these
// alias the store's constants rather than repeating the strings. A metric
// nothing reads is a metric that silently stopped working, and two spellings
// of one name is how that happens.
const (
	// metricRestarts is the lifetime counter as the API server reports it,
	// and metricRestartsDelta the change in it since the previous sweep. Both
	// are exported: the counter answers "how bad is this container's life",
	// the delta answers "what happened in this window", and neither can be
	// derived from the other once the series has been bucketed.
	metricRestarts      = clickhouse.MetricContainerRestarts
	metricRestartsDelta = clickhouse.MetricContainerRestartsDelta
	// metricOOMKilled is 1 for the sweep that noticed an OOM kill, so that
	// summing it over a window counts kills rather than sampling intervals.
	metricOOMKilled = clickhouse.MetricContainerOOMKilled
	// The limits, so a chart of usage from kubeletstats has a ceiling to draw
	// against without the read path going back to the API server for it.
	metricCPULimit    = clickhouse.MetricContainerCPULimit
	metricMemoryLimit = clickhouse.MetricContainerMemoryLimit
	// metricReplicas is how many pods the environment is running.
	metricReplicas = clickhouse.MetricEnvironmentReplicas
)

// Units, in UCUM as OTLP wants them: an annotation in braces for a count of
// something, and the real unit where there is one.
const (
	unitRestarts = "{restart}"
	unitKills    = "{kill}"
	unitCores    = "{cpu}"
	unitBytes    = "By"
	unitPods     = "{pod}"
)

// Resource attribute keys every exported sample carries.
//
// These are not this package's to choose. They are the keys the telemetry
// schema materializes its project, environment, namespace, pod, container and
// node columns out of, and the node collector stamps the same ones onto the
// kubelet's numbers — which is what puts both halves of an environment's
// metrics on the same rows of the same tables. Changing one here changes
// nothing but where this half lands.
//
// kitchen.build is deliberately absent: everything sampled here carries an
// environment label and is therefore runtime, and ClickHouse reads a missing
// key out of a map as the empty string the column wants anyway.
const (
	attrProject     = "kitchen.project"
	attrEnvironment = "deployment.environment.name"
	attrSource      = "kitchen.source"
	attrNamespace   = "k8s.namespace.name"
	attrPod         = "k8s.pod.name"
	attrContainer   = "k8s.container.name"
	attrNode        = "k8s.node.name"
)

// scope names the operator as the origin of these metrics where they sit
// beside the collector's own, which is the only way to tell afterwards which
// process produced a series.
func scope() instrumentation.Scope {
	return instrumentation.Scope{
		Name:    "github.com/Bermos/Kitchen/internal/usage",
		Version: version.Version,
	}
}

// Exporter is where a sweep goes: the OTLP metric exporter's own interface,
// narrowed to what this package calls, so a test can stand in for the node
// collector without one running.
type Exporter interface {
	Export(ctx context.Context, metrics *metricdata.ResourceMetrics) error
	Shutdown(ctx context.Context) error
}

// newExporter builds the OTLP/HTTP metric client for the endpoint as it is
// written on the Kitchen object.
//
// That endpoint is a base URL — the same string applications are handed in
// OTEL_EXPORTER_OTLP_ENDPOINT — and the exporter appends OTLP's own
// /v1/metrics path to it, exactly as an SDK inside an application would. Its
// scheme decides whether the connection is plaintext, which for an in-cluster
// Service name it is.
func newExporter(ctx context.Context, endpoint string) (Exporter, error) {
	return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(endpoint))
}

// ResourceMetrics shapes a sweep into what the exporter sends: one batch per
// container, and one per environment for the replica count.
//
// The split is not a choice. The schema materializes project, environment,
// pod and container out of the *resource* attributes, because that is where
// the collector puts them for every other signal and where they can sit in an
// ordering key — so a container's identity has to be its resource rather than
// its data point's attributes. OTLP's Go exporter carries one resource per
// request, which makes a sweep one small request per container, keep-alive'd
// to the node-local agent. Batching several resources into one body means
// writing the protobuf by hand and giving up the exporter's retries with it.
func (s Sweep) ResourceMetrics() []*metricdata.ResourceMetrics {
	batches := make([]*metricdata.ResourceMetrics, 0, len(s.Containers)+len(s.Environments))
	for _, sample := range s.Containers {
		batches = append(batches, batch(resource.NewSchemaless(
			attribute.String(attrProject, sample.Project),
			attribute.String(attrEnvironment, sample.Environment),
			attribute.String(attrSource, clickhouse.SourceRuntime),
			attribute.String(attrNamespace, sample.Namespace),
			attribute.String(attrPod, sample.Pod),
			attribute.String(attrContainer, sample.Container),
			attribute.String(attrNode, sample.Node),
		), s.containerMetrics(sample)))
	}
	for _, sample := range s.Environments {
		batches = append(batches, batch(resource.NewSchemaless(
			attribute.String(attrProject, sample.Project),
			attribute.String(attrEnvironment, sample.Environment),
			attribute.String(attrSource, clickhouse.SourceRuntime),
			attribute.String(attrNamespace, sample.Namespace),
		), s.environmentMetrics(sample)))
	}
	return batches
}

// containerMetrics is one container's five series.
func (s Sweep) containerMetrics(sample ContainerSample) []metricdata.Metrics {
	killed := int64(0)
	if sample.OOMKilled {
		killed = 1
	}
	return []metricdata.Metrics{
		{
			Name:        metricRestarts,
			Description: "Times the container has restarted since its pod started",
			Unit:        unitRestarts,
			Data: metricdata.Sum[int64]{
				Temporality: metricdata.CumulativeTemporality,
				IsMonotonic: true,
				// A cumulative point counts from the pod's start, not from
				// the operator's: the counter belongs to the container and
				// survives every restart of whatever is watching it.
				DataPoints: []metricdata.DataPoint[int64]{{
					StartTime: sample.Started,
					Time:      s.At,
					Value:     sample.Restarts,
				}},
			},
		},
		{
			Name:        metricRestartsDelta,
			Description: "Restarts since the previous sample",
			Unit:        unitRestarts,
			Data:        s.deltaSum(sample.Restarted),
		},
		{
			Name:        metricOOMKilled,
			Description: "Restarts since the previous sample the kernel's OOM killer caused",
			Unit:        unitKills,
			Data:        s.deltaSum(killed),
		},
		{
			Name:        metricCPULimit,
			Description: "CPU the container is limited to, zero where the release set no limit",
			Unit:        unitCores,
			Data:        gauge(s, sample.CPULimitCores),
		},
		{
			Name:        metricMemoryLimit,
			Description: "Memory the container is limited to, zero where the release set no limit",
			Unit:        unitBytes,
			Data:        gauge(s, sample.MemoryLimitBytes),
		},
	}
}

// environmentMetrics is the one series that belongs to an environment rather
// than to any container of it.
func (s Sweep) environmentMetrics(sample EnvironmentSample) []metricdata.Metrics {
	return []metricdata.Metrics{{
		Name:        metricReplicas,
		Description: "Pods the environment is running",
		Unit:        unitPods,
		Data:        gauge(s, sample.Pods),
	}}
}

// batch wraps one resource's metrics as the exporter takes them.
func batch(from *resource.Resource, metrics []metricdata.Metrics) *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{
		Resource: from,
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Scope:   scope(),
			Metrics: metrics,
		}},
	}
}

// deltaSum is a count of things that happened between the previous sweep and
// this one. Delta rather than cumulative because that is what the sampler
// actually knows: it can say what changed since it last looked, and cannot
// say what a counter it has been maintaining would read.
func (s Sweep) deltaSum(value int64) metricdata.Sum[int64] {
	return metricdata.Sum[int64]{
		Temporality: metricdata.DeltaTemporality,
		IsMonotonic: true,
		DataPoints: []metricdata.DataPoint[int64]{{
			StartTime: s.Since,
			Time:      s.At,
			Value:     value,
		}},
	}
}

// gauge is a value that was simply true at the moment it was read. It carries
// the sweep window as its start time rather than leaving it unset, so that
// nothing in the store has to explain a row stamped 1970.
func gauge[N int64 | float64](s Sweep, value N) metricdata.Gauge[N] {
	return metricdata.Gauge[N]{
		DataPoints: []metricdata.DataPoint[N]{{
			StartTime: s.Since,
			Time:      s.At,
			Value:     value,
		}},
	}
}
