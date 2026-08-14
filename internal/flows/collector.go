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

// Package flows follows Cilium's Hubble Relay and ships flow observations
// into the telemetry store — the data behind the dashboard's traffic view.
//
// Cilium is the platform's CNI and Hubble Relay its cluster-wide flow API, so
// nothing new runs on the nodes: the operator is a gRPC client of what is
// already there. The one-store rule from docs/SCOPE.md holds — flows land in
// the same ClickHouse the logs do, under the same retention.
package flows

import (
	"context"
	"strings"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	// configPollInterval is how often an idle collector re-reads the Kitchen
	// object waiting for a relay address (or a store) to appear, and how
	// often a streaming one checks whether the configuration moved away.
	configPollInterval = 30 * time.Second
	// reconnectDelay spaces attempts against a relay that is refusing or
	// dropping the stream.
	reconnectDelay = 10 * time.Second
	// flushInterval and flushBatch bound how long an observation sits in
	// memory and how large one insert grows.
	flushInterval = 5 * time.Second
	flushBatch    = 500
)

// Collector is a manager Runnable. It idles until the Kitchen singleton names
// a Hubble Relay address and a telemetry store, then follows the flow stream
// and batches observations into ClickHouse.
type Collector struct {
	Client client.Client
}

// NeedLeaderElection makes the collector a singleton: every replica shipping
// the same relay's flows would double-count every edge.
func (c *Collector) NeedLeaderElection() bool { return true }

func (c *Collector) log() logr.Logger { return logf.Log.WithName("flows") }

// config is one resolved reading of the Kitchen object.
type config struct {
	relayAddress string
	store        clickhouse.Config
	hasStore     bool
}

// Start implements manager.Runnable. It never returns an error before the
// context ends: flow collection is an observability capability, and a relay
// or store being down must not take the operator down with it.
func (c *Collector) Start(ctx context.Context) error {
	for {
		cfg, err := c.resolve(ctx)
		if err == nil && cfg.relayAddress != "" && cfg.hasStore {
			c.follow(ctx, cfg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(configPollInterval):
		}
	}
}

// resolve reads the relay address and the store connection off the Kitchen
// singleton, the same way the API and the reconcilers do.
func (c *Collector) resolve(ctx context.Context) (config, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return config{}, err
	}
	cfg := config{relayAddress: strings.TrimSpace(kitchen.Spec.Observability.Hubble.RelayAddress)}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return cfg, nil
	}
	secret := &corev1.Secret{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: controller.PlatformNamespace, Name: ref.Name}, secret); err != nil {
		return cfg, err
	}
	store, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return cfg, err
	}
	cfg.store, cfg.hasStore = store, true
	return cfg, nil
}

// follow holds one stream against the relay, shipping batches until the
// stream breaks, the configuration moves away, or the context ends.
func (c *Collector) follow(ctx context.Context, cfg config) {
	// In-cluster Hubble Relay serves plaintext gRPC by default; a TLS relay
	// would need certificates the platform has no story for yet.
	conn, err := grpc.NewClient(cfg.relayAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		c.log().V(1).Info("hubble relay unreachable", "address", cfg.relayAddress, "reason", err.Error())
		return
	}
	defer func() { _ = conn.Close() }()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := observerpb.NewObserverClient(conn).GetFlows(streamCtx, &observerpb.GetFlowsRequest{Follow: true})
	if err != nil {
		c.log().V(1).Info("hubble stream refused", "address", cfg.relayAddress, "reason", err.Error())
		return
	}
	c.log().Info("following hubble flows", "address", cfg.relayAddress)

	store := clickhouse.New(cfg.store)
	batch := make([]clickhouse.Flow, 0, flushBatch)
	lastFlush := time.Now()
	lastConfigCheck := time.Now()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := store.InsertFlows(ctx, batch); err != nil {
			// Dropped observations, not a broken collector: the next batch
			// tries again, and the traffic view simply has a gap.
			c.log().V(1).Info("flow batch dropped", "flows", len(batch), "reason", err.Error())
		}
		batch = batch[:0]
		lastFlush = time.Now()
	}
	defer flush()

	for {
		response, err := stream.Recv()
		if err != nil {
			if ctx.Err() == nil {
				c.log().V(1).Info("hubble stream ended", "reason", err.Error())
				select {
				case <-ctx.Done():
				case <-time.After(reconnectDelay):
				}
			}
			return
		}
		if flow := response.GetFlow(); flow != nil {
			if observation, keep := observe(flow); keep {
				batch = append(batch, observation)
			}
		}
		if len(batch) >= flushBatch || time.Since(lastFlush) > flushInterval {
			flush()
		}
		if time.Since(lastConfigCheck) > configPollInterval {
			lastConfigCheck = time.Now()
			if current, err := c.resolve(ctx); err == nil &&
				(current.relayAddress != cfg.relayAddress || !current.hasStore) {
				c.log().Info("flow configuration changed, reconnecting")
				return
			}
		}
	}
}

// observe turns one Hubble flow into a store row, or nothing. Three kinds of
// flow carry the signal the traffic view draws, everything else (per-packet
// noise, replies, agent chatter) is dropped here rather than in the store:
//
//   - HTTP responses: one request served, with status and latency. The flow
//     travels server→client, so the edge is recorded the way the request ran.
//   - TCP SYNs: one connection opened on an edge with no L7 visibility.
//   - Drops: whatever Cilium refused, whichever protocol it was.
func observe(flow *flowpb.Flow) (clickhouse.Flow, bool) {
	timestamp := time.Now()
	if t := flow.GetTime(); t != nil {
		timestamp = t.AsTime()
	}

	source := endpointName(flow.GetSource(), flow.GetSourceNames())
	destination := endpointName(flow.GetDestination(), flow.GetDestinationNames())

	if http := flow.GetL7().GetHttp(); http != nil {
		if flow.GetL7().GetType() != flowpb.L7FlowType_RESPONSE {
			return clickhouse.Flow{}, false
		}
		return clickhouse.Flow{
			Timestamp:            timestamp,
			Source:               destination.name,
			SourceNamespace:      destination.namespace,
			Destination:          source.name,
			DestinationNamespace: source.namespace,
			Protocol:             "HTTP",
			Verdict:              flow.GetVerdict().String(),
			HTTPStatus:           uint16(http.GetCode()),
			LatencyMs:            float64(flow.GetL7().GetLatencyNs()) / 1e6,
		}, true
	}

	dropped := flow.GetVerdict() == flowpb.Verdict_DROPPED
	syn := false
	protocol := ""
	switch {
	case flow.GetL4().GetTCP() != nil:
		protocol = "TCP"
		flags := flow.GetL4().GetTCP().GetFlags()
		syn = flags.GetSYN() && !flags.GetACK()
	case flow.GetL4().GetUDP() != nil:
		protocol = "UDP"
	case flow.GetL4().GetICMPv4() != nil, flow.GetL4().GetICMPv6() != nil:
		protocol = "ICMP"
	}
	if !dropped && !syn {
		return clickhouse.Flow{}, false
	}
	if flow.GetIsReply().GetValue() {
		return clickhouse.Flow{}, false
	}
	return clickhouse.Flow{
		Timestamp:            timestamp,
		Source:               source.name,
		SourceNamespace:      source.namespace,
		Destination:          destination.name,
		DestinationNamespace: destination.namespace,
		Protocol:             protocol,
		Verdict:              flow.GetVerdict().String(),
	}, true
}

type endpoint struct {
	name      string
	namespace string
}

// endpointName names a flow endpoint at service-map granularity: the owning
// workload when Hubble knows it, the pod as a fallback, a DNS name for the
// outside world, or "world" when nothing at all is known.
func endpointName(ep *flowpb.Endpoint, dnsNames []string) endpoint {
	if ep != nil {
		if workloads := ep.GetWorkloads(); len(workloads) > 0 && workloads[0].GetName() != "" {
			return endpoint{name: workloads[0].GetName(), namespace: ep.GetNamespace()}
		}
		if ep.GetPodName() != "" {
			return endpoint{name: ep.GetPodName(), namespace: ep.GetNamespace()}
		}
	}
	if len(dnsNames) > 0 {
		return endpoint{name: dnsNames[0]}
	}
	return endpoint{name: "world"}
}
