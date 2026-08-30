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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What the platform asks a container before it sends anyone to it (#236).
//
// Without a probe the kubelet marks a pod Ready the moment its process
// starts, so a rolling update swaps a serving pod for one that is still
// applying a migration — and `status.phase: Live`, which is read off the
// Deployment's availability and so off `readyReplicas`, meant no more than
// "the process started". These three probes are what make that number mean
// something.

// containerProbes builds the startup, readiness and liveness probes for one
// container.
//
// port is the container's own port, or zero for a workload that publishes
// none — a worker. A workload with neither a port nor a declared health check
// has nothing that could be probed and gets no probes at all.
//
// The three are deliberately not the same probe three times:
//
//   - **Startup** carries the generous threshold. Slow startup is a
//     legitimate state, and while a startup probe is failing the other two
//     are not consulted, so the loose number lives here alone.
//   - **Readiness** is what keeps a rollout honest: it is the one that
//     decides whether the Service sends anybody to this pod.
//   - **Liveness** is written only for an HTTP check, and that is a decision.
//     A TCP connect cannot tell a wedged application from a working one — the
//     socket still accepts — so a TCP liveness probe would restart nothing a
//     dying process was not already going to end, while adding a way for a
//     healthy container to be killed. Declaring a path is what buys the
//     restart, because a path is where the application says what wedged
//     means for it.
func containerProbes(health *kitchenv1alpha1.HealthSpec, port int32) (startup, readiness, liveness *corev1.Probe) {
	probePort := health.ProbePort(port)
	if probePort <= 0 {
		return nil, nil, nil
	}

	handler := corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(probePort)},
	}
	path := health.HTTPPath()
	if path != "" {
		handler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path,
			Port: intstr.FromInt32(probePort),
		}}
	}

	probe := func(failures int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler:     handler,
			PeriodSeconds:    health.Period(),
			TimeoutSeconds:   health.Timeout(),
			FailureThreshold: failures,
			SuccessThreshold: 1,
		}
	}

	startup = probe(health.StartupFailures())
	readiness = probe(health.Failures())
	if path != "" {
		liveness = probe(health.Failures())
	}
	return startup, readiness, liveness
}

// applyProbes puts the three probes on a container. It is one call rather
// than three assignments because a container that should have none has to
// have all three cleared: a Deployment found with probes on it and a health
// check since removed would otherwise keep them for ever.
func applyProbes(container *corev1.Container, health *kitchenv1alpha1.HealthSpec, port int32) {
	container.StartupProbe, container.ReadinessProbe, container.LivenessProbe = containerProbes(health, port)
}
