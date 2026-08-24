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

package controller

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Clock sync (issue #140): whether the timestamps this platform stamps mean
// the same thing on every machine that stamps them.
//
// It is here because an incident report is a correlation, and a correlation is
// three timestamps from three machines: a log line the collector read off a
// node, a request row the operator wrote, an audit record appended from
// whichever replica served the request. Clocks that disagree by more than the
// gaps being reasoned about do not make any of those wrong; they make the
// *order* wrong, silently, and there is nothing in the data that shows it.
// Retention and immutability are both worthless against a log whose ordering
// cannot be trusted, which is why this ships with them rather than after.
//
// # What is actually measured, and what it cannot see
//
// Each node's kubelet renews a Lease in `kube-node-lease`, stamping
// `spec.renewTime` from **the node's own clock**. The operator compares that
// stamp with its own clock. That is the measurement, and it has three
// properties worth stating rather than burying:
//
//   - **It is one-sided in its precision.** A renewal is up to the kubelet's
//     renewal period old by the time anyone reads it — ten seconds by default
//     — so a node whose clock is *behind* is indistinguishable from a node
//     whose renewal is merely stale, up to that period. A node whose clock is
//     *ahead* stamps a time in the future, which nothing but a wrong clock
//     produces. The threshold is therefore applied to the two directions
//     differently: a future stamp counts in full, a past one is discounted by
//     the renewal grace below.
//   - **The reference is the operator's own clock**, which comes from the node
//     the operator happens to be running on. So what this measures is
//     *disagreement within the cluster*, not agreement with UTC. A cluster
//     whose every node is uniformly ten minutes fast reads as perfectly
//     synchronised here.
//   - **Nothing is asked of the outside world.** Measuring against UTC would
//     mean reaching an NTP server or an HTTP `Date` header from the operator
//     pod, and a platform that owns its cluster has no business deciding on
//     its operator's behalf which time source the institution trusts. The
//     honest answer is the one this gives: the cluster's clocks agree with
//     each other to within X, and whether they agree with the world is the
//     job of whatever runs ntpd on those nodes.
//
// docs/COMPLIANCE.md §14.5 says the same thing at length, because a
// measurement whose limits are only in a code comment is a measurement
// somebody will over-read.

const (
	// clockComponentName is what the survey calls the check. It is a name
	// rather than a workload, and the survey reports it under kind `Node`.
	clockComponentName = "clock-sync"

	// clockRenewalGrace is how much staleness a lease renewal is forgiven in
	// the past direction. The kubelet's default renewal period is 10s and
	// its lease duration 40s; a node that has simply not renewed yet is not
	// a node with a wrong clock, and reporting it as one would make every
	// cluster fail this check every few minutes.
	//
	// It is compiled in rather than configured because it is a property of
	// the kubelet, not of the installation: an operator who wants a
	// different sensitivity moves the threshold, which is the knob that
	// means what they mean.
	clockRenewalGrace = 45 * time.Second

	// clockMethod names the measurement on the status, so a number is never
	// read without the caveat that belongs to it.
	clockMethod = "kubelet node lease renewTime, compared with the operator's own clock"
)

// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// nodeClock is one node's measured offset from the operator's clock.
type nodeClock struct {
	node string
	// offset is positive when the node's stamp is in the future — its clock
	// is ahead of the operator's — and negative when it is behind.
	offset time.Duration
	// drift is the offset the threshold is applied to: the same number, with
	// the renewal grace forgiven in the past direction only.
	drift time.Duration
}

// surveyClockSync measures every node's clock against the operator's and
// returns the component the survey reports, plus the measurement behind it.
//
// It returns no component at all when the check is off or when there is
// nothing to measure, because a component that says "not measured" in a list
// of workloads is worse than an absent one: the survey's contract is that
// anything in it with Healthy false is broken.
func (r *KitchenReconciler) surveyClockSync(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	now time.Time,
) (*kitchenv1alpha1.ComponentStatus, *kitchenv1alpha1.ClockSyncStatus) {
	spec := &kitchen.Spec.Observability.ClockSync
	if !spec.ClockSyncEnabled() {
		return nil, nil
	}
	threshold := time.Duration(spec.DriftThreshold()) * time.Second

	measured, err := r.measureNodeClocks(ctx, now)
	if err != nil {
		// Unmeasurable is not unhealthy. The operator may be running without
		// permission to read leases on a cluster that has been narrowed, and
		// a platform that reported that as a broken clock would be crying
		// wolf about somebody else's RBAC.
		return nil, &kitchenv1alpha1.ClockSyncStatus{
			Checked:         ptr.To(metav1.NewTime(now)),
			Method:          clockMethod,
			MaxDriftSeconds: spec.DriftThreshold(),
			Message:         "node clocks could not be measured: " + err.Error(),
		}
	}
	if len(measured) == 0 {
		return nil, &kitchenv1alpha1.ClockSyncStatus{
			Checked:         ptr.To(metav1.NewTime(now)),
			Method:          clockMethod,
			MaxDriftSeconds: spec.DriftThreshold(),
			Message: "no node has a kubelet lease to read a clock from, so time sync is unverified " +
				"on this cluster",
		}
	}

	var worst nodeClock
	drifted := 0
	for _, node := range measured {
		if abs(node.drift) > abs(worst.drift) {
			worst = node
		}
		if abs(node.drift) > threshold {
			drifted++
		}
	}

	status := &kitchenv1alpha1.ClockSyncStatus{
		Checked:          ptr.To(metav1.NewTime(now)),
		Method:           clockMethod,
		Nodes:            int32(len(measured)), //nolint:gosec // a node count is not a security boundary
		Drifted:          int32(drifted),       //nolint:gosec // ditto
		MaxDriftSeconds:  spec.DriftThreshold(),
		WorstNode:        worst.node,
		WorstDriftMillis: worst.offset.Milliseconds(),
	}

	component := &kitchenv1alpha1.ComponentStatus{
		Name:      clockComponentName,
		Kind:      "Node",
		Desired:   status.Nodes,
		Available: status.Nodes - status.Drifted,
		Healthy:   drifted == 0,
	}
	if drifted > 0 {
		status.Message = clockDriftMessage(worst, threshold, drifted, len(measured))
		component.Message = truncateMessage(status.Message)
	}
	return component, status
}

// clockDriftMessage is what an operator reads and acts on. It names the worst
// node, the size and direction of its offset, the threshold it broke, and the
// thing to go and look at — because "clock drift detected" is a message that
// sends somebody to a search engine rather than to a machine.
func clockDriftMessage(worst nodeClock, threshold time.Duration, drifted, nodes int) string {
	direction := "ahead of"
	if worst.offset < 0 {
		direction = "behind"
	}
	return fmt.Sprintf(
		"%d of %d nodes are beyond the %s drift threshold; %s is %s %s the operator's clock. "+
			"Every correlation across these nodes — a log line, a request, an audit record — is unsafe "+
			"to order while that holds. Check time synchronisation (chrony, systemd-timesyncd, ntpd) "+
			"on the node, or raise spec.observability.clockSync.maxDriftSeconds if this cluster "+
			"genuinely runs that loosely",
		drifted, nodes, threshold, worst.node, roundDuration(worst.offset), direction)
}

// measureNodeClocks reads every node's lease and turns it into an offset.
//
// A node with no lease is skipped rather than counted as drifted: the lease is
// the kubelet's, so a node that has none is a node the check has no opinion
// about. That is also the honest answer for a control plane node whose kubelet
// is not registered.
func (r *KitchenReconciler) measureNodeClocks(ctx context.Context, now time.Time) ([]nodeClock, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes); err != nil {
		return nil, err
	}

	leases := &coordinationv1.LeaseList{}
	if err := r.List(ctx, leases, client.InNamespace(NodeLeaseNamespace)); err != nil {
		return nil, err
	}
	renewed := make(map[string]time.Time, len(leases.Items))
	for i := range leases.Items {
		lease := &leases.Items[i]
		if lease.Spec.RenewTime == nil {
			continue
		}
		renewed[lease.Name] = lease.Spec.RenewTime.Time
	}

	measured := make([]nodeClock, 0, len(nodes.Items))
	for i := range nodes.Items {
		name := nodes.Items[i].Name
		stamp, ok := renewed[name]
		if !ok {
			continue
		}
		offset := stamp.Sub(now)
		measured = append(measured, nodeClock{node: name, offset: offset, drift: driftOf(offset)})
	}
	return measured, nil
}

// NodeLeaseNamespace is where the kubelet renews its heartbeat. It is
// Kubernetes' own constant and not configurable.
//
// It is exported because the manager's cache is narrowed to it: the clock
// check is the only thing here that reads a Lease, and an unrestricted
// informer would hold every leader-election lease in the cluster to answer a
// question about the nodes.
const NodeLeaseNamespace = "kube-node-lease"

// driftOf applies the renewal grace, in the past direction only.
//
// A stamp in the future can only come from a clock that is ahead, so it counts
// in full. A stamp in the past is a clock that is behind *or* a renewal that
// has not happened yet, and the two are indistinguishable from here — so the
// first renewal period and a bit is forgiven, and what is left is drift.
func driftOf(offset time.Duration) time.Duration {
	if offset >= 0 {
		return offset
	}
	if -offset <= clockRenewalGrace {
		return 0
	}
	return offset + clockRenewalGrace
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// roundDuration keeps a message readable: a millisecond of precision below a
// second, and whole seconds above it.
func roundDuration(d time.Duration) time.Duration {
	d = abs(d)
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}
