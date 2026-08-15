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
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
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

	view := statusView{
		Cluster: s.clusterStatus(ctx, kitchen),
		Tunnel:  tunnelStatus(kitchen),
		Gateway: gatewayStatus(kitchen),
	}
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
	view.Nodes = len(nodes.Items)
	for i := range nodes.Items {
		if nodeReady(&nodes.Items[i]) {
			view.ReadyNodes++
		}
	}
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

// buildQueue counts what the build controller's concurrency gate is currently
// weighing: builds running against the limit, and builds waiting for a slot.
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
	for i := range builds.Items {
		switch builds.Items[i].Status.Phase {
		case kitchenv1alpha1.BuildRunning:
			view.Running++
		case kitchenv1alpha1.BuildQueued:
			view.Queued++
		}
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
