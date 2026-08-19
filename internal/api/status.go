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

package api

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/controller"
)

// /status is the platform as it is running: the cluster it owns, whether the
// tunnel is up, how full the build queue is, and the component survey the
// Kitchen reconciler keeps. /settings answers the neighbouring question — the
// platform as it is configured — and the two are separate because the status
// bar polls this one every half minute and never writes to it.

const (
	// condGatewayProgrammed and condTunnelConnected are the Kitchen
	// conditions this view reads. They are spelled here rather than imported
	// because they are part of the platform's published status vocabulary
	// (docs/CRDS.md), not an implementation detail of the reconciler.
	condGatewayProgrammed = "GatewayProgrammed"
	condTunnelConnected   = "TunnelConnected"
)

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

func (s *Server) getStatus(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	// Everyone gets the platform's name and the build queue. The tunnel, the
	// gateway, the component survey and the node counts are the operator's,
	// and a member's answer leaves them out rather than emptying them — see
	// statusView.
	view := statusView{Cluster: clusterStatusView{Name: clusterName(kitchen)}}
	if platformRoleFrom(ctx).AtLeast(access.PlatformOperator) {
		view.Cluster = s.clusterStatus(ctx, kitchen)
		tunnel := tunnelStatus(kitchen)
		view.Tunnel = &tunnel
		gateway := gatewayStatus(kitchen)
		view.Gateway = &gateway
		for _, component := range kitchen.Status.Components {
			view.Components = append(view.Components, componentStatusView{
				Name:      component.Name,
				Kind:      component.Kind,
				Healthy:   component.Healthy,
				Available: component.Available,
				Desired:   component.Desired,
				Message:   component.Message,
			})
		}
	}

	builds, err := s.buildQueue(ctx, kitchen)
	if err != nil {
		s.writeError(w, err)
		return
	}
	view.Builds = builds

	writeJSON(w, http.StatusOK, view)
}

// clusterStatus counts the nodes the platform runs on.
//
// A failed count is reported inside the view rather than as an error: the
// operator's ClusterRole only grew the node permission with this endpoint, so
// an installation whose chart has not been upgraded yet would otherwise lose
// the whole status bar over the one line it cannot fill in.
func (s *Server) clusterStatus(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) clusterStatusView {
	view := clusterStatusView{Name: clusterName(kitchen)}

	nodes := &corev1.NodeList{}
	if err := s.reader().List(ctx, nodes); err != nil {
		view.Message = err.Error()
		return view
	}
	total, ready := len(nodes.Items), 0
	for i := range nodes.Items {
		if nodeReady(&nodes.Items[i]) {
			ready++
		}
	}
	view.Nodes, view.ReadyNodes = &total, &ready
	return view
}

// clusterName is what the cluster is called, falling back to the first label
// of the base domain — an installation that never named its cluster still has
// one word that means "this platform" to the people using it.
func clusterName(kitchen *kitchenv1alpha1.Kitchen) string {
	if name := strings.TrimSpace(kitchen.Spec.ClusterName); name != "" {
		return name
	}
	label, _, _ := strings.Cut(kitchen.Spec.BaseDomain, ".")
	return label
}

func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func tunnelStatus(kitchen *kitchenv1alpha1.Kitchen) tunnelStatusView {
	enabled := kitchen.Spec.Ingress.Cloudflared.Enabled
	return tunnelStatusView{
		Enabled:   enabled,
		Connected: enabled && conditionIs(kitchen.Status.Conditions, condTunnelConnected, metav1.ConditionTrue),
		Message:   conditionMessage(kitchen.Status.Conditions, condTunnelConnected),
	}
}

func gatewayStatus(kitchen *kitchenv1alpha1.Kitchen) gatewayStatusView {
	return gatewayStatusView{
		Address:    kitchen.Status.GatewayAddress,
		Programmed: conditionIs(kitchen.Status.Conditions, condGatewayProgrammed, metav1.ConditionTrue),
		Message:    conditionMessage(kitchen.Status.Conditions, condGatewayProgrammed),
	}
}

// buildQueue reports what the build controller's concurrency gate is currently
// weighing: builds running against the limit, and builds waiting for a slot —
// with how long each has been waiting.
//
// The wait is the part worth reading. A queue's length says how busy the
// platform is; only the wait says whether it is moving, and a build that has
// been queued for half an hour against a gate with free capacity is a stuck
// controller rather than a busy one.
func (s *Server) buildQueue(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) (buildQueueView, error) {
	capacity := kitchen.Spec.Builds.Concurrency
	if capacity <= 0 {
		capacity = controller.DefaultBuildConcurrency
	}
	view := buildQueueView{Capacity: capacity}

	builds := &kitchenv1alpha1.BuildList{}
	if err := s.Client.List(ctx, builds, client.InNamespace(s.Namespace)); err != nil {
		return buildQueueView{}, err
	}
	now := time.Now()
	for i := range builds.Items {
		build := &builds.Items[i]
		switch build.Status.Phase {
		case kitchenv1alpha1.BuildRunning:
			view.Running++
		case kitchenv1alpha1.BuildQueued:
			view.Queued++
			// A build is queued from the moment it exists: admission is what
			// creates it, and the gate is the only thing between that and
			// running.
			queued := build.CreationTimestamp.Time
			wait := int64(now.Sub(queued).Seconds())
			if wait < 0 {
				wait = 0
			}
			view.Waiting = append(view.Waiting, queuedBuildView{
				Name:        build.Name,
				Project:     build.Spec.ProjectRef.Name,
				QueuedAt:    queued.UTC().Format(time.RFC3339),
				WaitSeconds: wait,
			})
		}
	}
	sort.Slice(view.Waiting, func(i, j int) bool {
		return view.Waiting[i].WaitSeconds > view.Waiting[j].WaitSeconds
	})
	if len(view.Waiting) > 0 {
		view.OldestWaitSeconds = view.Waiting[0].WaitSeconds
	}
	return view, nil
}

// conditionIs answers whether a condition of that type is set to that status.
// A condition nobody has written yet is neither.
func conditionIs(conditions []metav1.Condition, condType string, status metav1.ConditionStatus) bool {
	for _, condition := range conditions {
		if condition.Type == condType {
			return condition.Status == status
		}
	}
	return false
}

// conditionMessage is the message on a condition, empty when it is unset.
func conditionMessage(conditions []metav1.Condition, condType string) string {
	for _, condition := range conditions {
		if condition.Type == condType {
			return condition.Message
		}
	}
	return ""
}
