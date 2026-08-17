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

// Package usage samples what only the API server knows about the platform's
// workloads — restarts, OOM kills, resource limits, replica counts — and
// exports it to the node collector over OTLP.
//
// # Why the operator still samples, now that there is a collector
//
// The node collector's kubeletstats receiver reads CPU and memory per pod and
// container, and k8sattributes stamps every series it produces with the
// project and environment off the pod's labels. That join — from a series
// keyed by namespace, pod and container back to the application that owns it —
// is the one a bare cAdvisor scrape cannot make, and it is why this package
// used to call every kubelet itself. It no longer does; that half moved.
//
// The other half has no receiver at all. How often a container has restarted,
// whether the kernel took it for its memory, what it is limited to and how
// many pods an environment is actually running are facts about API objects,
// not about a running process. The conventional answer is kube-state-metrics:
// a second workload whose entire job is to turn API objects into metrics for a
// system that can only read metrics. The operator is not that system — it
// reads API objects for a living — so it keeps that half and hands the result
// to the collector like any other workload.
//
// # Why it exports rather than inserts
//
// The collector owns every write into the OTel tables, and a second writer
// would be a second thing to keep in step with the schema. Exporting instead
// makes the operator an ordinary OTLP client of the same endpoint every
// instrumented application is handed: the samples arrive carrying the resource
// attributes the collector stamps on the kubelet's own numbers, land in the
// same tables under the same retention, and the read path joins the two
// halves on project and environment without caring which side wrote which.
//
// # Why the differencing happens here
//
// Kubernetes reports restarts as a lifetime counter. Bucketed for a chart, a
// counter loses every transition that lands on a bucket boundary — the restart
// that happened at 10:20:00 sits inside the 10:20 bucket as a flat 1, and in
// the bucket before it as a flat 0, so no bucket contains the change. This
// sampler knows what it saw last time, so it exports the change since then as
// a delta beside the counter, and a question about any window is a sum.
package usage

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	// configPollInterval is how often an idle collector re-reads the Kitchen
	// object waiting for an OTLP endpoint (or for sampling to be switched
	// on) to appear.
	configPollInterval = 30 * time.Second
	// DefaultInterval matches the CRD's default, for a Kitchen object written
	// before the field existed.
	DefaultInterval = 30 * time.Second
	// oomWindow is how far back a termination counts as "the reason for the
	// restart this sample just noticed". The restart itself is what triggers
	// the look; this only guards against attributing an ancient OOM kill to a
	// restart that had some other cause.
	oomWindow = 10 * time.Minute
)

// oomKilledReason is the reason Kubernetes gives a container the kernel's OOM
// killer took.
const oomKilledReason = "OOMKilled"

// ContainerKey identifies one container of one pod, which is the granularity
// the API server reports restarts at and the collector reports usage at.
type ContainerKey struct {
	Namespace string
	Pod       string
	Container string
}

// environmentKey is what makes two pods the same environment. Namespace rides
// along because it is a resource attribute of the replica count, not because
// an environment is ever spread across two.
type environmentKey struct {
	project     string
	environment string
	namespace   string
}

// ContainerSample is one reading of one container: what the API server knows
// about it that no receiver on the node does.
type ContainerSample struct {
	Project     string
	Environment string
	Namespace   string
	Pod         string
	Container   string
	Node        string

	// Started is when the pod started, which is when the restart counter
	// below started from zero. A cumulative sum has to say where it counts
	// from, or a consumer cannot tell a fresh pod from a counter that jumped.
	Started time.Time

	// CPULimitCores and MemoryLimitBytes are zero where the release set no
	// limit, which is the truth and is what the dashboard draws as "no limit"
	// rather than as a limit of nothing.
	CPULimitCores    float64
	MemoryLimitBytes int64

	// Restarts is the lifetime counter, Restarted the change in it since the
	// previous sweep, and OOMKilled whether that change was the kernel.
	Restarts  int64
	Restarted int64
	OOMKilled bool
}

// EnvironmentSample is how many pods one environment is running. It counts
// pods rather than ready pods: a crash-looping replica is one the environment
// is running, and is usually the reason someone is looking at the page.
type EnvironmentSample struct {
	Project     string
	Environment string
	Namespace   string
	Pods        int64
}

// Sweep is one pass over the platform's application pods.
type Sweep struct {
	// At is when the pass was taken and Since when the one before it was;
	// together they are the window the delta metrics cover.
	At    time.Time
	Since time.Time

	Containers   []ContainerSample
	Environments []EnvironmentSample
}

// Collector is a manager Runnable. It idles until the Kitchen singleton names
// an OTLP endpoint and leaves sampling switched on, then exports one sweep per
// interval.
type Collector struct {
	// Client reads the Kitchen singleton, which the manager caches.
	Client client.Client
	// Reader reads pods. It is the uncached reader for the same reason the
	// API's introspection endpoints use one: caching every pod in the cluster
	// to sample them on a timer would mean an informer over all of them, warm
	// forever.
	Reader client.Reader
	// Exporter overrides where sweeps go. Empty means the endpoint on the
	// Kitchen object, which is the only thing production uses; a test sets it
	// to keep the sweep in memory.
	Exporter Exporter

	// seen is the previous sweep's restart counter per container, which is
	// what makes a restart an event. It is rebuilt whole every cycle, so a
	// container that has gone away takes its entry with it.
	seen map[ContainerKey]uint32
	// last is when the previous sweep was taken, and so the start of the
	// window this one's deltas cover.
	last time.Time
}

// NeedLeaderElection makes the collector a singleton: this is a cluster-wide
// sampler, and every replica exporting the same containers would show up as
// several times the restarts.
func (c *Collector) NeedLeaderElection() bool { return true }

func (c *Collector) log() logr.Logger { return logf.Log.WithName("usage") }

// config is one resolved reading of the Kitchen object.
type config struct {
	enabled  bool
	interval time.Duration
	endpoint string
}

// Start implements manager.Runnable. Like the flow collector it never returns
// an error before the context ends: sampling is an observability capability,
// and a collector being down must not take the operator down.
func (c *Collector) Start(ctx context.Context) error {
	for {
		cfg, err := c.resolve(ctx)
		switch {
		case err != nil:
			c.log().V(1).Info("cannot read the sampling configuration", "reason", err.Error())
		case cfg.enabled && cfg.endpoint != "":
			c.run(ctx, cfg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(configPollInterval):
		}
	}
}

// resolve reads the sampler's configuration off the Kitchen singleton, the
// same way the API and the reconcilers do.
//
// The endpoint is the traces one because there is only one OTLP address on the
// platform: the node collector's Service, which applications are handed and
// the operator exports to as another client of it.
func (c *Collector) resolve(ctx context.Context) (config, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return config{}, err
	}
	metrics := kitchen.Spec.Observability.Metrics
	cfg := config{
		enabled:  metrics.Enabled,
		interval: DefaultInterval,
		endpoint: strings.TrimSpace(kitchen.Spec.Observability.Traces.Endpoint),
	}
	if metrics.IntervalSeconds > 0 {
		cfg.interval = time.Duration(metrics.IntervalSeconds) * time.Second
	}
	return cfg, nil
}

// run samples on the interval until the configuration moves away or the
// context ends.
func (c *Collector) run(ctx context.Context, cfg config) {
	exporter := c.Exporter
	if exporter == nil {
		built, err := newExporter(ctx, cfg.endpoint)
		if err != nil {
			c.log().V(1).Info("cannot export samples", "endpoint", cfg.endpoint, "reason", err.Error())
			return
		}
		exporter = built
		// The exporter holds a connection pool to the node collector, so a
		// configuration change that returns from here closes it rather than
		// leaving one behind per reconfiguration.
		defer func() {
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := exporter.Shutdown(shutdown); err != nil {
				c.log().V(1).Info("the metric exporter did not shut down cleanly", "reason", err.Error())
			}
		}()
	}
	c.log().Info("sampling workload resources", "interval", cfg.interval.String(), "endpoint", cfg.endpoint)

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	// The first sweep establishes the restart baseline and is exported like
	// any other; only its deltas are zero, because nothing was seen before it.
	c.sweepOnce(ctx, exporter)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := c.resolve(ctx)
			if err == nil && (!current.enabled ||
				current.interval != cfg.interval || current.endpoint != cfg.endpoint) {
				c.log().Info("sampling configuration changed, restarting")
				return
			}
			c.sweepOnce(ctx, exporter)
		}
	}
}

// sweepOnce takes one reading of every application container and exports it.
func (c *Collector) sweepOnce(ctx context.Context, exporter Exporter) {
	sweep, err := c.Sample(ctx, time.Now().UTC())
	if err != nil {
		c.log().V(1).Info("sampling failed", "reason", err.Error())
		return
	}
	for _, batch := range sweep.ResourceMetrics() {
		if err := exporter.Export(ctx, batch); err != nil {
			// A dropped export is a gap in the series, not a broken
			// collector: the next sweep tries again. It is logged once per
			// resource rather than once per sweep because an endpoint that
			// refuses one refuses all of them, and the first line says why.
			c.log().V(1).Info("sample export dropped", "reason", err.Error())
			return
		}
	}
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Sample reads one sample per container of every application pod, and the
// replica count per environment those pods add up to.
//
// It is driven by the pods rather than by anything the node reports: a
// container that is crash-looping or waiting on an image is still a replica
// the environment is running, and leaving it out would make the replica count
// say zero for an environment that very much exists.
func (c *Collector) Sample(ctx context.Context, at time.Time) (Sweep, error) {
	pods := &corev1.PodList{}
	if err := c.Reader.List(ctx, pods, client.HasLabels{controller.LabelEnvironment}); err != nil {
		return Sweep{}, err
	}

	since := c.last
	if since.IsZero() {
		// The first sweep has nothing behind it, so its delta window is empty
		// — and so, necessarily, are its deltas.
		since = at
	}
	sweep := Sweep{At: at, Since: since}

	seen := make(map[ContainerKey]uint32, len(c.seen))
	replicas := map[environmentKey]int64{}
	// Environments in the order they were first seen, because a map's is not
	// an order and two sweeps over the same cluster should export the same one.
	var order []environmentKey
	for i := range pods.Items {
		pod := &pods.Items[i]
		limits := containerLimits(pod)
		environment := environmentKey{
			project:     pod.Labels[controller.LabelProject],
			environment: pod.Labels[controller.LabelEnvironment],
			namespace:   pod.Namespace,
		}
		if _, ok := replicas[environment]; !ok {
			order = append(order, environment)
		}
		replicas[environment]++

		started := at
		if pod.Status.StartTime != nil {
			started = pod.Status.StartTime.Time
		}

		for j := range pod.Status.ContainerStatuses {
			status := &pod.Status.ContainerStatuses[j]
			key := ContainerKey{Namespace: pod.Namespace, Pod: pod.Name, Container: status.Name}

			restarts := uint32(status.RestartCount) // #nosec G115 -- a restart count is never negative
			restarted := int64(0)
			if previous, ok := c.seen[key]; ok && restarts > previous {
				restarted = int64(restarts - previous)
			}
			seen[key] = restarts

			sweep.Containers = append(sweep.Containers, ContainerSample{
				Project:          environment.project,
				Environment:      environment.environment,
				Namespace:        pod.Namespace,
				Pod:              pod.Name,
				Container:        status.Name,
				Node:             pod.Spec.NodeName,
				Started:          started,
				CPULimitCores:    limits[status.Name].cpuCores,
				MemoryLimitBytes: limits[status.Name].memoryBytes,
				Restarts:         int64(restarts),
				Restarted:        restarted,
				OOMKilled:        restarted > 0 && oomKilled(status, at),
			})
		}
	}
	for i := range sweep.Environments {
		sweep.Environments[i].Pods = int64(replicas[environmentKey{
			project:     sweep.Environments[i].Project,
			environment: sweep.Environments[i].Environment,
			namespace:   sweep.Environments[i].Namespace,
		}])
	}

	c.seen, c.last = seen, at
	return sweep, nil
}

// oomKilled reports whether the restart this sample noticed was the kernel
// reclaiming the container's memory.
func oomKilled(status *corev1.ContainerStatus, at time.Time) bool {
	terminated := status.LastTerminationState.Terminated
	if terminated == nil || terminated.Reason != oomKilledReason {
		return false
	}
	// A container that OOMed once keeps that last state until it terminates
	// again, so the reason alone would mark every later restart an OOM kill.
	return !terminated.FinishedAt.IsZero() && at.Sub(terminated.FinishedAt.Time) < oomWindow
}

// limit is what one container is allowed.
type limit struct {
	cpuCores    float64
	memoryBytes int64
}

// containerLimits is what each container of a pod is allowed, by name.
func containerLimits(pod *corev1.Pod) map[string]limit {
	limits := make(map[string]limit, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		allowed := limit{}
		if cpu, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
			allowed.cpuCores = float64(cpu.MilliValue()) / 1000
		}
		if memory, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
			if value, ok := memory.AsInt64(); ok && value > 0 {
				allowed.memoryBytes = value
			}
		}
		limits[container.Name] = allowed
	}
	return limits
}
