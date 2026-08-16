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

// Package usage samples what the platform's workloads are actually using —
// CPU, memory, restarts, replicas — and ships the samples into the telemetry
// store. It is what turns the environment page from an instant into a history.
//
// # Why the operator samples rather than a collector scraping
//
// The obvious pipeline is a Prometheus scrape of every kubelet's cAdvisor
// endpoint, and Kitchen already runs a Vector on every node that could do it.
// It does not, for one reason: a cAdvisor series is keyed by namespace, pod and
// container, and the join from there back to the project and environment that
// own the pod lives in the pod's *labels*, which a scrape does not carry. A
// collector that cannot make that join produces numbers the dashboard cannot
// attribute, and the usual fix — kube-state-metrics, joined at query time — is
// a second workload whose entire job is to turn API objects into metrics for a
// system that can only read metrics. The operator is not that system: it reads
// API objects for a living.
//
// So the operator asks the kubelet for usage (one Summary API call per node
// that runs application pods, through the API server's node proxy) and takes
// everything else — which pods exist, what they are limited to, how often they
// have restarted, whether the last exit was an OOM kill — from the pods it can
// already list. Nothing new runs on the nodes and there is nothing to keep in
// step with anything.
//
// # Why the differencing happens here
//
// Kubernetes reports restarts as a lifetime counter. Bucketed in the store,
// a counter loses every transition that lands on a bucket boundary — the
// restart that happened at 10:20:00 sits inside the 10:20 bucket as a flat 1,
// and the bucket before it as a flat 0, so no bucket contains the change. The
// collector knows what it saw last time, so it writes the delta as well as the
// counter and the store keeps events it can simply add up.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	// configPollInterval is how often an idle collector re-reads the Kitchen
	// object waiting for a telemetry store (or for sampling to be switched
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

// Sampler answers what one node's containers are using. It is an interface so
// the collector can be exercised without a kubelet.
type Sampler interface {
	NodeUsage(ctx context.Context, node string) (map[ContainerKey]Usage, error)
}

// ContainerKey identifies one container of one pod, which is the granularity
// both the kubelet and the store work at.
type ContainerKey struct {
	Namespace string
	Pod       string
	Container string
}

// Usage is what one container is using right now.
type Usage struct {
	CPUCores    float64
	MemoryBytes uint64
}

// Collector is a manager Runnable. It idles until the Kitchen singleton names
// a telemetry store and leaves sampling switched on, then writes one batch of
// samples per interval.
type Collector struct {
	// Client reads the Kitchen singleton and the store's secret, both of
	// which the manager caches.
	Client client.Client
	// Reader reads pods and nodes. It is the uncached reader for the same
	// reason the API's introspection endpoints use one: caching every pod in
	// the cluster to sample them on a timer would mean an informer over all
	// of them, warm forever.
	Reader client.Reader
	// Sampler is where usage comes from. Nil means the kubelet, reached
	// through the API server's node proxy.
	Sampler Sampler

	// seen is the previous sample's restart counter per container, which is
	// what makes a restart an event. It is rebuilt whole every cycle, so a
	// container that has gone away takes its entry with it.
	seen map[ContainerKey]uint32
}

// NeedLeaderElection makes the collector a singleton: every replica sampling
// the same containers would write the same rows several times over, and the
// series would read as several times the usage.
func (c *Collector) NeedLeaderElection() bool { return true }

func (c *Collector) log() logr.Logger { return logf.Log.WithName("usage") }

// config is one resolved reading of the Kitchen object.
type config struct {
	enabled  bool
	interval time.Duration
	store    clickhouse.Config
	hasStore bool
}

// Start implements manager.Runnable. Like the flow collector it never returns
// an error before the context ends: sampling is an observability capability,
// and a kubelet or a store being down must not take the operator down.
func (c *Collector) Start(ctx context.Context) error {
	for {
		cfg, err := c.resolve(ctx)
		switch {
		case err != nil:
			c.log().V(1).Info("cannot read the sampling configuration", "reason", err.Error())
		case cfg.enabled && cfg.hasStore:
			c.run(ctx, cfg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(configPollInterval):
		}
	}
}

// resolve reads the sampler's configuration and the store connection off the
// Kitchen singleton, the same way the API and the reconcilers do.
func (c *Collector) resolve(ctx context.Context) (config, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return config{}, err
	}
	metrics := kitchen.Spec.Observability.Metrics
	cfg := config{enabled: metrics.Enabled, interval: DefaultInterval}
	if metrics.IntervalSeconds > 0 {
		cfg.interval = time.Duration(metrics.IntervalSeconds) * time.Second
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return cfg, nil
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: controller.PlatformNamespace, Name: ref.Name}
	if err := c.Client.Get(ctx, key, secret); err != nil {
		return cfg, err
	}
	store, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return cfg, err
	}
	cfg.store, cfg.hasStore = store, true
	return cfg, nil
}

// run samples on the interval until the configuration moves away or the
// context ends.
func (c *Collector) run(ctx context.Context, cfg config) {
	store := clickhouse.New(cfg.store)
	c.log().Info("sampling workload resources", "interval", cfg.interval.String())

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	// The first sample establishes the restart baseline and is written like
	// any other; only its deltas are zero, because nothing was seen before it.
	c.sampleOnce(ctx, store)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := c.resolve(ctx)
			if err == nil && (!current.enabled || !current.hasStore ||
				current.interval != cfg.interval || current.store != cfg.store) {
				c.log().Info("sampling configuration changed, restarting")
				return
			}
			c.sampleOnce(ctx, store)
		}
	}
}

// sampleOnce takes one reading of every application container and writes it.
func (c *Collector) sampleOnce(ctx context.Context, store *clickhouse.Client) {
	samples, err := c.Sample(ctx, time.Now().UTC())
	if err != nil {
		c.log().V(1).Info("sampling failed", "reason", err.Error())
		return
	}
	if err := store.InsertResourceSamples(ctx, samples); err != nil {
		// A dropped batch is a gap in the series, not a broken collector:
		// the next one tries again.
		c.log().V(1).Info("resource batch dropped", "samples", len(samples), "reason", err.Error())
	}
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get

// Sample reads one sample per container of every application pod.
//
// It is driven by the pods, not by the kubelet's answer: a container that is
// crash-looping or waiting on an image has no usage to report, and leaving it
// out would make the replica count — which is how many pods reported — say
// zero for an environment that very much exists and is the reason someone is
// looking at the page.
//
// A node whose kubelet cannot be reached contributes pods without usage rather
// than failing the sweep, so one sick node does not blank the whole platform's
// series.
func (c *Collector) Sample(ctx context.Context, at time.Time) ([]clickhouse.ResourceSample, error) {
	pods := &corev1.PodList{}
	if err := c.Reader.List(ctx, pods, client.HasLabels{controller.LabelEnvironment}); err != nil {
		return nil, err
	}

	usage := map[ContainerKey]Usage{}
	sampler := c.sampler()
	for _, node := range nodesOf(pods.Items) {
		perNode, err := sampler.NodeUsage(ctx, node)
		if err != nil {
			c.log().V(1).Info("no usage from node", "node", node, "reason", err.Error())
			continue
		}
		for key, value := range perNode {
			usage[key] = value
		}
	}

	seen := make(map[ContainerKey]uint32, len(c.seen))
	samples := make([]clickhouse.ResourceSample, 0, len(usage))
	for i := range pods.Items {
		pod := &pods.Items[i]
		limits := containerLimits(pod)
		for j := range pod.Status.ContainerStatuses {
			status := &pod.Status.ContainerStatuses[j]
			key := ContainerKey{Namespace: pod.Namespace, Pod: pod.Name, Container: status.Name}

			restarts := uint32(status.RestartCount) // #nosec G115 -- a restart count is never negative
			restarted := uint16(0)
			if previous, ok := c.seen[key]; ok && restarts > previous {
				restarted = uint16(min(restarts-previous, 0xffff))
			}
			seen[key] = restarts

			sample := clickhouse.ResourceSample{
				Timestamp:        at,
				Project:          pod.Labels[controller.LabelProject],
				Environment:      pod.Labels[controller.LabelEnvironment],
				Namespace:        pod.Namespace,
				Pod:              pod.Name,
				Container:        status.Name,
				Node:             pod.Spec.NodeName,
				CPUCores:         usage[key].CPUCores,
				MemoryBytes:      usage[key].MemoryBytes,
				CPULimitCores:    limits[status.Name].CPUCores,
				MemoryLimitBytes: limits[status.Name].MemoryBytes,
				Restarts:         restarts,
				Restarted:        restarted,
				OOMKilled:        restarted > 0 && oomKilled(status, at),
			}
			samples = append(samples, sample)
		}
	}
	c.seen = seen
	return samples, nil
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

// containerLimits is what each container of a pod is allowed, by name. A
// container with no limit reports zero, which is the truth and is what the
// dashboard draws as "no limit" rather than as a limit of nothing.
func containerLimits(pod *corev1.Pod) map[string]Usage {
	limits := make(map[string]Usage, len(pod.Spec.Containers))
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		limit := Usage{}
		if cpu, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
			limit.CPUCores = float64(cpu.MilliValue()) / 1000
		}
		if memory, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
			if value, ok := memory.AsInt64(); ok && value > 0 {
				limit.MemoryBytes = uint64(value)
			}
		}
		limits[container.Name] = limit
	}
	return limits
}

// nodesOf is the distinct nodes a set of pods runs on, so the sweep asks each
// kubelet once and asks no kubelet that runs nothing of interest.
func nodesOf(pods []corev1.Pod) []string {
	seen := map[string]struct{}{}
	nodes := []string{}
	for i := range pods {
		node := pods[i].Spec.NodeName
		if node == "" {
			continue
		}
		if _, ok := seen[node]; ok {
			continue
		}
		seen[node] = struct{}{}
		nodes = append(nodes, node)
	}
	return nodes
}

func (c *Collector) sampler() Sampler {
	if c.Sampler != nil {
		return c.Sampler
	}
	return noSampler{}
}

// noSampler stands in for a collector built without one. Pods are still
// counted and their restarts still recorded — the replica and restart series
// are worth having on their own — and the usage columns stay zero.
type noSampler struct{}

func (noSampler) NodeUsage(context.Context, string) (map[ContainerKey]Usage, error) {
	return nil, nil
}

// KubeletSampler reads the kubelet's Summary API through the API server's node
// proxy, which is the one path that needs no network route to the nodes and no
// second set of credentials.
type KubeletSampler struct {
	REST rest.Interface
}

// NewKubeletSampler builds a sampler from the manager's own REST
// configuration.
func NewKubeletSampler(cfg *rest.Config) (*KubeletSampler, error) {
	// The core API group is served at /api/v1, which is where nodes and their
	// proxy subresource live. Nothing here is decoded through the serializer
	// — the kubelet's answer is read raw — but a REST client insists on one.
	config := rest.CopyConfig(cfg)
	config.APIPath = "/api"
	config.GroupVersion = &corev1.SchemeGroupVersion
	config.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	client, err := rest.RESTClientFor(config)
	if err != nil {
		return nil, err
	}
	return &KubeletSampler{REST: client}, nil
}

// summary is the slice of the kubelet's /stats/summary answer that matters.
// It is decoded into types of this package's own rather than k8s.io/kubelet's:
// the fields used are four, and the alternative is a dependency on a component
// whose Go module moves with every Kubernetes release.
type summary struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		Containers []struct {
			Name string `json:"name"`
			CPU  *struct {
				UsageNanoCores uint64 `json:"usageNanoCores"`
			} `json:"cpu"`
			Memory *struct {
				WorkingSetBytes uint64 `json:"workingSetBytes"`
			} `json:"memory"`
		} `json:"containers"`
	} `json:"pods"`
}

// NodeUsage asks one kubelet what its containers are using.
func (s *KubeletSampler) NodeUsage(ctx context.Context, node string) (map[ContainerKey]Usage, error) {
	if strings.TrimSpace(node) == "" {
		return nil, fmt.Errorf("a node name is required")
	}
	raw, err := s.REST.Get().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("stats", "summary").
		// Everything else in the answer — filesystem stats, per-volume
		// numbers, the node's own network counters — is several times the
		// size of what is used here.
		Param("only_cpu_and_memory", "true").
		DoRaw(ctx)
	if err != nil {
		return nil, err
	}

	answer := summary{}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("unreadable summary from node %s: %w", node, err)
	}

	usage := map[ContainerKey]Usage{}
	for _, pod := range answer.Pods {
		for _, container := range pod.Containers {
			value := Usage{}
			if container.CPU != nil {
				value.CPUCores = float64(container.CPU.UsageNanoCores) / 1e9
			}
			if container.Memory != nil {
				value.MemoryBytes = container.Memory.WorkingSetBytes
			}
			usage[ContainerKey{
				Namespace: pod.PodRef.Namespace,
				Pod:       pod.PodRef.Name,
				Container: container.Name,
			}] = value
		}
	}
	return usage, nil
}
