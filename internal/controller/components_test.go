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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

var _ = Describe("Component survey", func() {
	ctx := context.Background()

	var reconciler *KitchenReconciler
	var kitchen *kitchenv1alpha1.Kitchen
	var created []client.Object

	// setCond writes into the kitchen under test, exactly as Reconcile's does.
	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		kitchen.Status.Conditions = append(kitchen.Status.Conditions, metav1.Condition{
			Type: condType, Status: status, Reason: reason, Message: message,
			LastTransitionTime: metav1.Now(),
		})
	}

	condition := func() *metav1.Condition {
		for i := range kitchen.Status.Conditions {
			if kitchen.Status.Conditions[i].Type == condComponentsHealthy {
				return &kitchen.Status.Conditions[i]
			}
		}
		return nil
	}

	componentNamed := func(name string) *kitchenv1alpha1.ComponentStatus {
		for i := range kitchen.Status.Components {
			if kitchen.Status.Components[i].Name == name {
				return &kitchen.Status.Components[i]
			}
		}
		return nil
	}

	podTemplate := func(labels map[string]string) corev1.PodTemplateSpec {
		return corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "c", Image: "busybox",
			}}},
		}
	}

	// track registers an object for teardown and creates it.
	track := func(obj client.Object) {
		ExpectWithOffset(1, k8sClient.Create(ctx, obj)).To(Succeed())
		created = append(created, obj)
	}

	platform := func(component string) map[string]string {
		labels := map[string]string{labelPartOfKey: labelPartOfValue, "sel": component}
		if component != "" {
			labels[labelComponentKind] = component
		}
		return labels
	}

	BeforeEach(func() {
		// APIReader is the direct client here, which is what the event
		// lookup needs: field selectors are not served by a cache.
		reconciler = &KitchenReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
		}
		kitchen = &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		}
		created = nil
		Expect(reconciler.ensurePlatformNamespace(ctx)).To(Succeed())
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("reports nothing to worry about when no workload carries the label", func() {
		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeTrue())
		Expect(kitchen.Status.Components).To(BeEmpty())
		Expect(condition().Status).To(Equal(metav1.ConditionTrue))
		Expect(condition().Reason).To(Equal("NoComponents"))
	})

	It("counts a fully available deployment as healthy", func() {
		labels := platform("auth")
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-auth", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(int32(2)),
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "auth"}},
				Template: podTemplate(map[string]string{"sel": "auth"}),
			},
		}
		track(deploy)
		// The API server rejects an available count that outruns the ready
		// and total ones, so all three have to agree.
		deploy.Status.Replicas = 2
		deploy.Status.ReadyReplicas = 2
		deploy.Status.AvailableReplicas = 2
		Expect(k8sClient.Status().Update(ctx, deploy)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeTrue())

		auth := componentNamed("auth")
		Expect(auth).NotTo(BeNil())
		Expect(auth.Kind).To(Equal("Deployment"))
		Expect(auth.Healthy).To(BeTrue())
		Expect(auth.Available).To(Equal(int32(2)))
		Expect(auth.Desired).To(Equal(int32(2)))
		Expect(auth.Message).To(BeEmpty())
		Expect(condition().Message).To(Equal("1/1 healthy"))
	})

	// The regression this whole survey exists for. A DaemonSet whose pods are
	// refused at admission has no pods at all, so nothing anywhere is in a bad
	// state — there is only a count that never reaches its target.
	It("catches a daemonset that wants pods and has none", func() {
		labels := platform("logs")
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-logs", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "logs"}},
				Template: podTemplate(map[string]string{"sel": "logs"}),
			},
		}
		track(ds)
		ds.Status.DesiredNumberScheduled = 1
		ds.Status.NumberAvailable = 0
		Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())

		logs := componentNamed("logs")
		Expect(logs).NotTo(BeNil())
		Expect(logs.Kind).To(Equal("DaemonSet"))
		Expect(logs.Healthy).To(BeFalse())
		Expect(logs.Message).To(HavePrefix("0 of 1 pods available"))
		Expect(condition().Status).To(Equal(metav1.ConditionFalse))
		Expect(condition().Reason).To(Equal("ComponentsUnhealthy"))
		Expect(condition().Message).To(ContainSubstring("waiting on logs"))
	})

	// The no-pod case: with nothing to ask, the workload's own events are still
	// where the reason is.
	It("explains an unhealthy component with its latest warning event", func() {
		labels := platform("logs")
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-logs-evt", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "logs"}},
				Template: podTemplate(map[string]string{"sel": "logs"}),
			},
		}
		track(ds)
		ds.Status.DesiredNumberScheduled = 1
		Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

		track(&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-logs-evt.1", Namespace: PlatformNamespace},
			InvolvedObject: corev1.ObjectReference{
				Kind: "DaemonSet", Namespace: PlatformNamespace,
				Name: ds.Name, UID: ds.UID, APIVersion: "apps/v1",
			},
			Reason:        "FailedCreate",
			Message:       `violates PodSecurity "baseline:latest": hostPath volumes`,
			Type:          corev1.EventTypeWarning,
			LastTimestamp: metav1.Now(),
		})

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())
		Expect(componentNamed("logs").Message).To(ContainSubstring("violates PodSecurity"))
	})

	// The failure the survey under-reported: the collector was created, ran for
	// eight seconds, was OOM-killed, and did it again. `0 of 1 pods available`
	// was the whole report; OOMKilled, exit 137 and the restart count were on
	// the pod all along, and nothing was reading them.
	It("explains a pod that is created and then killed", func() {
		labels := platform("logs")
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-logs-oom", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "logs-oom"}},
				Template: podTemplate(map[string]string{"sel": "logs-oom"}),
			},
		}
		track(ds)
		ds.Status.DesiredNumberScheduled = 1
		Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "survey-logs-oom-abcde", Namespace: PlatformNamespace,
				Labels: map[string]string{"sel": "logs-oom"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vector", Image: "busybox"}}},
		}
		track(pod)
		pod.Status = corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "vector",
				RestartCount: 214,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff",
				}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason:     "OOMKilled",
					ExitCode:   137,
					FinishedAt: metav1.Now(),
				}},
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())

		message := componentNamed("logs").Message
		Expect(message).To(HavePrefix("0 of 1 pods available: "))
		Expect(message).To(ContainSubstring("OOMKilled (exit 137)"))
		Expect(message).To(ContainSubstring("CrashLoopBackOff"))
		// A blip and a loop read very differently, and the count is what tells
		// them apart.
		Expect(message).To(ContainSubstring("214 restarts"))
	})

	It("reports why a pod could not be scheduled", func() {
		labels := platform("clickhouse")
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-ch", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.StatefulSetSpec{
				ServiceName: "survey-ch",
				Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "ch"}},
				Template:    podTemplate(map[string]string{"sel": "ch"}),
			},
		}
		track(sts)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "survey-ch-0", Namespace: PlatformNamespace,
				Labels: map[string]string{"sel": "ch"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "clickhouse", Image: "busybox"}}},
		}
		track(pod)
		pod.Status = corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: `pod has unbound immediate PersistentVolumeClaims`,
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())
		Expect(componentNamed("clickhouse").Message).To(
			ContainSubstring("Unschedulable: pod has unbound immediate PersistentVolumeClaims"))
	})

	It("reports a container that never started", func() {
		labels := platform("auth")
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-auth-pull", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "auth-pull"}},
				Template: podTemplate(map[string]string{"sel": "auth-pull"}),
			},
		}
		track(deploy)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "survey-auth-pull-xyz", Namespace: PlatformNamespace,
				Labels: map[string]string{"sel": "auth-pull"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "auth", Image: "busybox"}}},
		}
		track(pod)
		pod.Status = corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "auth",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: `Back-off pulling image "ghcr.io/bermos/kitchen-auth:nope"`,
				}},
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())
		Expect(componentNamed("auth").Message).To(ContainSubstring("ImagePullBackOff: Back-off pulling image"))
	})

	// A pod that is starting is not a pod that is broken. Reporting
	// ContainerCreating as the reason for a shortfall would make every ordinary
	// rollout read as a fault, so the workload's own events answer instead.
	It("does not mistake a starting pod for a failing one", func() {
		labels := platform("logs")
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-logs-start", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "logs-start"}},
				Template: podTemplate(map[string]string{"sel": "logs-start"}),
			},
		}
		track(ds)
		ds.Status.DesiredNumberScheduled = 1
		Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "survey-logs-start-abcde", Namespace: PlatformNamespace,
				Labels: map[string]string{"sel": "logs-start"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vector", Image: "busybox"}}},
		}
		track(pod)
		pod.Status = corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "vector",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
			}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())
		Expect(componentNamed("logs").Message).To(Equal("0 of 1 pods available"))
	})

	It("still reports counts when it can read neither pods nor events", func() {
		reconciler.APIReader = nil

		labels := platform("logs")
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-logs-noread", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "logs"}},
				Template: podTemplate(map[string]string{"sel": "logs"}),
			},
		}
		track(ds)
		ds.Status.DesiredNumberScheduled = 3
		ds.Status.NumberAvailable = 1
		Expect(k8sClient.Status().Update(ctx, ds)).To(Succeed())

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())
		Expect(componentNamed("logs").Message).To(Equal("1 of 3 pods available"))
	})

	It("ignores workloads that are not part of the platform", func() {
		deploy := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: "survey-stranger", Namespace: PlatformNamespace,
				Labels: map[string]string{"sel": "stranger"},
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "stranger"}},
				Template: podTemplate(map[string]string{"sel": "stranger"}),
			},
		}
		track(deploy)

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeTrue())
		Expect(componentNamed("survey-stranger")).To(BeNil())
	})

	It("falls back to the object name when a workload sets no component label", func() {
		labels := platform("")
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-nameless", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.StatefulSetSpec{
				ServiceName: "survey-nameless",
				Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"sel": ""}},
				Template:    podTemplate(map[string]string{"sel": ""}),
			},
		}
		track(sts)

		Expect(reconciler.surveyComponents(ctx, kitchen, setCond)).To(BeFalse())

		nameless := componentNamed("survey-nameless")
		Expect(nameless).NotTo(BeNil())
		Expect(nameless.Kind).To(Equal("StatefulSet"))
		// One replica is the API's default, and none of it is running.
		Expect(nameless.Desired).To(Equal(int32(1)))
		Expect(nameless.Available).To(Equal(int32(0)))
	})

	It("reports components in name order so the list does not churn", func() {
		for _, name := range []string{"zulu", "alpha", "mike"} {
			labels := platform(name)
			track(&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: "survey-" + name, Namespace: PlatformNamespace, Labels: labels,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": name}},
					Template: podTemplate(map[string]string{"sel": name}),
				},
			})
		}

		reconciler.surveyComponents(ctx, kitchen, setCond)

		names := make([]string, 0, len(kitchen.Status.Components))
		for _, c := range kitchen.Status.Components {
			names = append(names, c.Name)
		}
		Expect(names).To(Equal([]string{"alpha", "mike", "zulu"}))
	})

	// status.components is keyed on name, so a duplicate does not read oddly —
	// it makes the whole status update fail, conditions included.
	It("keeps names unique when two workloads claim the same component", func() {
		singleton := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, singleton))).To(Succeed())
		defer func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, singleton))).To(Succeed())
		}()

		for _, name := range []string{"survey-twin-a", "survey-twin-b"} {
			labels := platform("twin")
			track(&appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: labels},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "twin"}},
					Template: podTemplate(map[string]string{"sel": "twin"}),
				},
			})
		}

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, singleton)).To(Succeed())
		kitchen = singleton
		reconciler.surveyComponents(ctx, kitchen, setCond)

		names := make([]string, 0, 2)
		for _, c := range kitchen.Status.Components {
			names = append(names, c.Name)
		}
		Expect(names).To(ConsistOf("twin", "survey-twin-b"))

		// The real assertion: the API server accepts it.
		Expect(k8sClient.Status().Update(ctx, singleton)).To(Succeed())
	})

	It("keeps the singleton's status writable with components on it", func() {
		singleton := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, singleton))).To(Succeed())
		defer func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, singleton))).To(Succeed())
		}()

		labels := platform("auth")
		track(&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "survey-persist", Namespace: PlatformNamespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"sel": "auth"}},
				Template: podTemplate(map[string]string{"sel": "auth"}),
			},
		})

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, singleton)).To(Succeed())
		kitchen = singleton
		reconciler.surveyComponents(ctx, kitchen, setCond)
		Expect(k8sClient.Status().Update(ctx, singleton)).To(Succeed())

		stored := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, stored)).To(Succeed())
		Expect(stored.Status.Components).To(HaveLen(1))
		Expect(stored.Status.Components[0].Name).To(Equal("auth"))
	})
})
