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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/provider"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// A project is one deployable unit, and a unit can be several workloads
// (#271).
//
// What these cases hold up is the four claims the issue is written against: a
// commit produces one release spanning all of them, a preview brings up the
// whole set wired to itself, a workload is reachable from inside without
// being published, and a rollback restores the exact set that release
// declared.

var _ = Describe("A unit of several workloads", func() {
	const (
		projectName = "unitshop"
		namespace   = "default"
		webImage    = "registry.example.com/kitchen/unitshop@sha256:1111111111111111"
		apiImage    = "registry.example.com/kitchen/unitshop-api@sha256:2222222222222222"
		prodName    = "unitshop-production"
		previewName = "unitshop-pr-7"
	)
	appNS := "kitchen-" + projectName

	ctx := context.Background()

	var reconciler *EnvironmentReconciler
	var releases int

	apiService := func() kitchenv1alpha1.ProcessSpec {
		return kitchenv1alpha1.ProcessSpec{
			Name: "api",
			Type: kitchenv1alpha1.ProcessService,
			Port: 8080,
			Build: &kitchenv1alpha1.ProcessBuildSpec{
				Strategy:       kitchenv1alpha1.BuildStrategyDockerfile,
				DockerfilePath: detect.DefaultDockerfile,
				RootDirectory:  "services/api",
			},
		}
	}

	// release writes a new Release and answers its name. A Release spec is
	// immutable, so a case that wants another set of workloads makes another
	// release — which is also what a rollback target is.
	release := func(processes []kitchenv1alpha1.ProcessSpec, workloads []kitchenv1alpha1.WorkloadImage) string {
		releases++
		name := "unitshop-rel-" + strings.Repeat("0", 5) + string(rune('0'+releases))
		ExpectWithOffset(1, k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "unitshop-bld-1"},
				Image:      webImage,
				Workloads:  workloads,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 3000},
					Processes: processes,
				},
			},
		})).To(Succeed())
		return name
	}

	environment := func(name string, envType kitchenv1alpha1.EnvironmentType, releaseName string) *kitchenv1alpha1.Environment {
		spec := kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
			Type:       envType,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
		}
		if envType == kitchenv1alpha1.EnvironmentPreview {
			spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 7, Branch: "feat/api"}
		}
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       spec,
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		key := types.NamespacedName{Name: name, Namespace: namespace}
		for range 2 {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		return env
	}

	// moveTo is a rollback: the environment is pointed at another release and
	// reconciled again.
	moveTo := func(name, releaseName string) *kitchenv1alpha1.Environment {
		key := types.NamespacedName{Name: name, Namespace: namespace}
		env := &kitchenv1alpha1.Environment{}
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: releaseName}
		ExpectWithOffset(1, k8sClient.Update(ctx, env)).To(Succeed())
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, k8sClient.Get(ctx, key, env)).To(Succeed())
		return env
	}

	get := func(name string, into client.Object) bool {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, into)
		if errors.IsNotFound(err) {
			return false
		}
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return true
	}

	containerEnv := func(deploy *appsv1.Deployment, name string) string {
		for _, variable := range deploy.Spec.Template.Spec.Containers[0].Env {
			if variable.Name == name {
				return variable.Value
			}
		}
		return ""
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		releases = 0

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/unitshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Previews: kitchenv1alpha1.PreviewsSpec{Protected: ptr.To(false)},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		for _, name := range []string{prodName, previewName} {
			env := &kitchenv1alpha1.Environment{}
			key := types.NamespacedName{Name: name, Namespace: namespace}
			if err := k8sClient.Get(ctx, key, env); err != nil {
				continue
			}
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			for range 2 {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		list := &kitchenv1alpha1.ReleaseList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
		for i := range list.Items {
			if strings.HasPrefix(list.Items[i].Name, "unitshop-rel-") {
				Expect(k8sClient.Delete(ctx, &list.Items[i])).To(Succeed())
			}
		}
	})

	It("gives a service an address inside the cluster and no route out of it", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release([]kitchenv1alpha1.ProcessSpec{apiService()},
				[]kitchenv1alpha1.WorkloadImage{{Name: "api", Image: apiImage}}))

		By("running it as a Deployment of its own")
		deploy := &appsv1.Deployment{}
		Expect(get(prodName+"-api", deploy)).To(BeTrue())
		Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(apiImage),
			"a workload built from its own directory runs its own image, not the release's")
		Expect(deploy.Spec.Template.Spec.Containers[0].Ports).To(HaveLen(1))
		Expect(deploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(8080)))

		By("putting a Service in front of it that selects only its own pods")
		svc := &corev1.Service{}
		Expect(get(prodName+"-api", svc)).To(BeTrue())
		Expect(svc.Spec.Selector).To(Equal(map[string]string{
			labelEnvironment: prodName,
			labelProcess:     "api",
		}), "the environment label alone would put the web process behind a service's address")
		Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8080)),
			"a renumbered port would make the address and the workload's own configuration disagree")

		By("publishing nothing: a route is what publishes, and a service gets none")
		route := &gatewayv1.HTTPRoute{}
		Expect(get(prodName+"-api", route)).To(BeFalse())

		By("saying where it answers, on the environment rather than only in the cluster")
		status := environmentStatus(ctx, prodName).FindProcessStatus("api")
		Expect(status).NotTo(BeNil())
		Expect(status.Address).To(Equal("http://" + prodName + "-api." + appNS + ".svc.cluster.local:8080"))
		Expect(status.Image).To(Equal(apiImage))
	})

	It("hands every workload of the unit its siblings' addresses", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release([]kitchenv1alpha1.ProcessSpec{
				apiService(),
				{Name: "worker", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"node", "worker.js"}},
			}, []kitchenv1alpha1.WorkloadImage{{Name: "api", Image: apiImage}}))

		want := "http://" + prodName + "-api." + appNS + ".svc.cluster.local:8080"

		web := &appsv1.Deployment{}
		Expect(get(prodName, web)).To(BeTrue())
		Expect(containerEnv(web, "KITCHEN_SERVICE_API")).To(Equal(want),
			"the web process cannot work out where its siblings are")
		Expect(containerEnv(web, "KITCHEN_SERVICE_API_HOST")).
			To(Equal(prodName + "-api." + appNS + ".svc.cluster.local"))
		Expect(containerEnv(web, "KITCHEN_SERVICE_API_PORT")).To(Equal("8080"))

		worker := &appsv1.Deployment{}
		Expect(get(prodName+"-worker", worker)).To(BeTrue())
		Expect(containerEnv(worker, "KITCHEN_SERVICE_API")).To(Equal(want),
			"a worker is as much of the unit as the web process is")
	})

	It("tells a service its own port and a worker the web process's", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release([]kitchenv1alpha1.ProcessSpec{
				apiService(),
				{Name: "worker", Type: kitchenv1alpha1.ProcessWorker},
			}, []kitchenv1alpha1.WorkloadImage{{Name: "api", Image: apiImage}}))

		api := &appsv1.Deployment{}
		Expect(get(prodName+"-api", api)).To(BeTrue())
		Expect(containerEnv(api, "PORT")).To(Equal("8080"),
			"a buildpacks-built service handed the web process's port listens where nothing connects")
		Expect(containerEnv(api, "KITCHEN_PROCESS")).To(Equal("api"))

		worker := &appsv1.Deployment{}
		Expect(get(prodName+"-worker", worker)).To(BeTrue())
		Expect(containerEnv(worker, "PORT")).To(Equal("3000"),
			"a worker publishes nothing, so PORT stays the platform's own contract for the unit")
	})

	It("brings up the whole set in a preview, wired to itself", func() {
		environment(previewName, kitchenv1alpha1.EnvironmentPreview,
			release([]kitchenv1alpha1.ProcessSpec{
				apiService(),
				{Name: "worker", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"node", "worker.js"}},
			}, []kitchenv1alpha1.WorkloadImage{{Name: "api", Image: apiImage}}))

		By("running the service without anybody opting it in")
		Expect(get(previewName+"-api", &appsv1.Deployment{})).To(BeTrue())
		Expect(get(previewName+"-api", &corev1.Service{})).To(BeTrue())

		By("still leaving the worker out, which is the default that protects a queue")
		Expect(get(previewName+"-worker", &appsv1.Deployment{})).To(BeFalse())

		By("pointing the preview's web process at the preview's own service")
		web := &appsv1.Deployment{}
		Expect(get(previewName, web)).To(BeTrue())
		Expect(containerEnv(web, "KITCHEN_SERVICE_API")).
			To(Equal("http://" + previewName + "-api." + appNS + ".svc.cluster.local:8080"))
		Expect(containerEnv(web, "KITCHEN_SERVICE_API")).NotTo(ContainSubstring(prodName),
			"four related workloads as four projects is exactly the preview this exists to prevent")
	})

	It("names no address for a service the environment does not run", func() {
		opted := apiService()
		opted.Previews = ptr.To(false)
		environment(previewName, kitchenv1alpha1.EnvironmentPreview,
			release([]kitchenv1alpha1.ProcessSpec{opted}, nil))

		Expect(get(previewName+"-api", &corev1.Service{})).To(BeFalse())
		web := &appsv1.Deployment{}
		Expect(get(previewName, web)).To(BeTrue())
		Expect(containerEnv(web, "KITCHEN_SERVICE_API")).To(BeEmpty(),
			"an address that resolves to nothing is worse than no address")
	})

	It("restores the exact set of workloads and images a release declared", func() {
		before := release([]kitchenv1alpha1.ProcessSpec{apiService()},
			[]kitchenv1alpha1.WorkloadImage{{Name: "api", Image: apiImage}})
		after := release([]kitchenv1alpha1.ProcessSpec{
			apiService(),
			{Name: "reports", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"node", "reports.js"}},
		}, []kitchenv1alpha1.WorkloadImage{
			{Name: "api", Image: "registry.example.com/kitchen/unitshop-api@sha256:3333333333333333"},
		})

		environment(prodName, kitchenv1alpha1.EnvironmentProduction, after)
		Expect(get(prodName+"-reports", &appsv1.Deployment{})).To(BeTrue())

		By("rolling back")
		moveTo(prodName, before)

		Expect(get(prodName+"-reports", &appsv1.Deployment{})).To(BeFalse(),
			"a rollback restores the set that release declared, never today's")
		api := &appsv1.Deployment{}
		Expect(get(prodName+"-api", api)).To(BeTrue())
		Expect(api.Spec.Template.Spec.Containers[0].Image).To(Equal(apiImage),
			"restoring a release has to restore the image each workload was built to")
	})

	It("takes a service's address away when it stops being one", func() {
		environment(prodName, kitchenv1alpha1.EnvironmentProduction,
			release([]kitchenv1alpha1.ProcessSpec{apiService()},
				[]kitchenv1alpha1.WorkloadImage{{Name: "api", Image: apiImage}}))
		Expect(get(prodName+"-api", &corev1.Service{})).To(BeTrue())

		By("declaring the same name as a worker instead")
		asWorker := release([]kitchenv1alpha1.ProcessSpec{{
			Name: "api", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"node", "api.js"},
		}}, nil)
		moveTo(prodName, asWorker)

		Expect(get(prodName+"-api", &appsv1.Deployment{})).To(BeTrue(),
			"the workload is still declared, so its Deployment stays")
		Expect(get(prodName+"-api", &corev1.Service{})).To(BeFalse(),
			"an address nothing is meant to answer on has to stop resolving")
	})
})

// testNamespace is where every suite in this package creates its Kitchen
// objects. It is the operator's own namespace in these tests, and the helpers
// below take a name rather than a pair because nothing here uses another.
const testNamespace = "default"

// environmentStatus reads an Environment back, for the assertions that are
// about what the reconciler recorded rather than what it created. Every suite
// in this package puts its objects in testNamespace, so it is not asked for.
func environmentStatus(ctx context.Context, name string) *kitchenv1alpha1.Environment {
	env := &kitchenv1alpha1.Environment{}
	ExpectWithOffset(1, k8sClient.Get(ctx,
		types.NamespacedName{Name: name, Namespace: testNamespace}, env)).To(Succeed())
	return env
}

// The plans a Build makes are a pure function of the project, the commit and
// the registry, so they are checked without a cluster.

func TestABuildPlansOneImagePerWorkloadThatDeclaresABuild(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kitchenv1alpha1.ProjectSpec{
			Build: kitchenv1alpha1.ProjectBuildSpec{RootDirectory: "apps/web", DockerfilePath: detect.DefaultDockerfile},
			Processes: []kitchenv1alpha1.ProcessSpec{
				{
					Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080,
					// Spelled loosely on purpose: a workload's build root is
					// a build root, so it reaches the builder spelled the way
					// a build spells one.
					Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "./services/api/"},
				},
				// A worker on the project's own image: no build, no plan.
				{Name: "worker", Type: kitchenv1alpha1.ProcessWorker},
			},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-abc123def456"},
		Spec:       kitchenv1alpha1.BuildSpec{Git: kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"}},
	}
	registry := provider.RegistryTarget{Prefix: "registry.example.com/kitchen"}

	plans := buildPlansFor(project, build,
		registry, webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile))

	if len(plans) != 2 {
		t.Fatalf("want the project's image and the one workload that declared a build, got %d plans", len(plans))
	}
	if !plans[0].isWeb() || plans[0].RootDirectory != "apps/web" || plans[0].Job != build.Name {
		t.Errorf("the project's own plan is not what it always was: %+v", plans[0])
	}
	if plans[0].Tag != "registry.example.com/kitchen/shop:abc123def456" {
		t.Errorf("the project's image moved: %s", plans[0].Tag)
	}

	api := plans[1]
	if api.Workload != "api" || api.RootDirectory != "services/api" {
		t.Errorf("the workload's build root is not spelled the way a build spells one: %+v", api)
	}
	if api.DockerfilePath != detect.DefaultDockerfile {
		t.Errorf("a workload that named no Dockerfile did not get the conventional one: %+v", api)
	}
	if api.Repository != "registry.example.com/kitchen/shop-api" {
		t.Errorf("a workload's image must sort with the project's and share its credential: %s", api.Repository)
	}
	if api.Job != "shop-bld-abc123def456-api" {
		t.Errorf("a workload's Job has to say which build and which workload: %s", api.Job)
	}
	if api.Strategy != kitchenv1alpha1.BuildStrategyDockerfile {
		t.Errorf("a workload's strategy defaults to dockerfile: %s", api.Strategy)
	}
}

// Which stage of its Dockerfile each image ships is a per-workload answer
// with one chain behind it: the workload's own, else the commit's
// kitchen.json, else the project's. A unit built from one multi-stage file
// names the stage once; a workload that differs says so.
func TestEachImageResolvesItsOwnDockerfileStage(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kitchenv1alpha1.ProjectSpec{
			Build: kitchenv1alpha1.ProjectBuildSpec{DockerfileTarget: "runtime"},
			Processes: []kitchenv1alpha1.ProcessSpec{
				{
					Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080,
					Build: &kitchenv1alpha1.ProcessBuildSpec{DockerfileTarget: " api-runtime "},
				},
				{
					Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
					Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/worker"},
				},
			},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-abc123def456"},
		Spec:       kitchenv1alpha1.BuildSpec{Git: kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"}},
	}
	registry := provider.RegistryTarget{Prefix: "registry.example.com/kitchen"}

	plans := buildPlansFor(project, build,
		registry, webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile))
	if len(plans) != 3 {
		t.Fatalf("want the project's image and two workloads, got %d plans", len(plans))
	}
	for _, tc := range []struct {
		plan buildPlan
		want string
	}{
		{plans[0], "runtime"},
		// Spelled loosely on purpose: a stage is spelled by the one place
		// that says what a stage may be called.
		{plans[1], "api-runtime"},
		// Named none of its own, so the unit's stands in.
		{plans[2], "runtime"},
	} {
		if tc.plan.DockerfileTarget != tc.want {
			t.Errorf("%s ships stage %q, want %q", tc.plan.describe(), tc.plan.DockerfileTarget, tc.want)
		}
	}

	// The commit's own file wins over the project for every image that named
	// no stage itself, which is what makes a rebuild of an old commit build
	// what that commit asked for.
	build.Status.Config = &kitchenv1alpha1.RepoConfig{
		Build: &kitchenv1alpha1.RepoBuildConfig{DockerfileTarget: "committed"},
	}
	plans = buildPlansFor(project, build,
		registry, webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile))
	if plans[0].DockerfileTarget != "committed" || plans[2].DockerfileTarget != "committed" {
		t.Errorf("the commit's own stage did not win: %q and %q",
			plans[0].DockerfileTarget, plans[2].DockerfileTarget)
	}
	if plans[1].DockerfileTarget != "api-runtime" {
		t.Errorf("a workload that named its own stage lost it to the commit's: %q", plans[1].DockerfileTarget)
	}

	// A buildpacks workload inherits nothing: a unit that names a stage can
	// still ship one workload through the lifecycle, which has no stages.
	// One that named a stage itself keeps it and is refused for it.
	project.Spec.Processes = append(project.Spec.Processes, kitchenv1alpha1.ProcessSpec{
		Name: "reports", Type: kitchenv1alpha1.ProcessWorker,
		Build: &kitchenv1alpha1.ProcessBuildSpec{Strategy: kitchenv1alpha1.BuildStrategyBuildpacks},
	})
	plans = buildPlansFor(project, build,
		registry, webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile))
	if plans[3].DockerfileTarget != "" {
		t.Errorf("a buildpacks workload inherited a stage it has no file for: %q", plans[3].DockerfileTarget)
	}
	project.Spec.Processes[2].Build.DockerfileTarget = "runner"
	plans = buildPlansFor(project, build,
		registry, webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile))
	if unbuildableTarget(plans) == nil {
		t.Error("a stage the workload named itself on a buildpacks build was not refused")
	}
	project.Spec.Processes = project.Spec.Processes[:2]

	// What a running build is observed against is what it recorded, so a
	// stage the project has since changed does not rewrite a build's history.
	build.Status.DockerfileTarget = "as-built"
	build.Status.Workloads = []kitchenv1alpha1.WorkloadBuildStatus{{
		Name: "api", Job: "shop-bld-abc123def456-api", DockerfileTarget: "api-as-built",
	}}
	underway := plansUnderway(project, build,
		registry, webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile))
	if underway[0].DockerfileTarget != "as-built" || underway[1].DockerfileTarget != "api-as-built" {
		t.Errorf("a running build is observed against settings that have moved: %q and %q",
			underway[0].DockerfileTarget, underway[1].DockerfileTarget)
	}
}

// A running Build waits on what it started, not on what the project says now.
// A workload added while a build was running would otherwise appear in a
// recomputed plan with no Job behind it, and a Build waiting on a Job nobody
// will ever create waits for ever while reporting Running.
func TestARunningBuildWaitsOnWhatItStarted(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec: kitchenv1alpha1.ProjectSpec{
			Processes: []kitchenv1alpha1.ProcessSpec{
				{
					Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080,
					Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/api"},
				},
				// Added after this build started.
				{
					Name: "billing", Type: kitchenv1alpha1.ProcessService, Port: 9000,
					Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/billing"},
				},
			},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-abc123def456"},
		Spec:       kitchenv1alpha1.BuildSpec{Git: kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"}},
		Status: kitchenv1alpha1.BuildStatus{
			Workloads: []kitchenv1alpha1.WorkloadBuildStatus{{
				Name:       "api",
				Job:        "shop-bld-abc123def456-api",
				Repository: "registry.example.com/kitchen/shop-api",
			}},
		},
	}
	registry := provider.RegistryTarget{Prefix: "registry.example.com/kitchen"}
	web := webPlan(project, build, registry, kitchenv1alpha1.BuildStrategyDockerfile)

	plans := plansUnderway(project, build, registry, web)
	if len(plans) != 2 {
		t.Fatalf("a running build waits on the two Jobs it created, got %d plans", len(plans))
	}
	if plans[1].Workload != "api" || plans[1].Job != "shop-bld-abc123def456-api" {
		t.Errorf("the recorded workload is not what is observed: %+v", plans[1])
	}
	if plans[1].Tag != "registry.example.com/kitchen/shop-api:abc123def456" {
		t.Errorf("the tag a digest is read against moved: %s", plans[1].Tag)
	}

	// A build that recorded nothing is every single-image project, and every
	// build from before workloads existed: the web plan alone.
	build.Status.Workloads = nil
	project.Spec.Processes = nil
	if plans := plansUnderway(project, build, registry, web); len(plans) != 1 || !plans[0].isWeb() {
		t.Fatalf("a project that ships one image plans one: %+v", plans)
	}
}

func TestAWorkloadJobNameStaysInsideALabelValue(t *testing.T) {
	build := strings.Repeat("b", 46) + "-bld-abc123def456"
	long := workloadJobName(build, strings.Repeat("w", 46))
	if len(long) > maxJobNameLength {
		t.Fatalf("a Job name has to fit a label value: %d characters", len(long))
	}
	// Two workloads sharing a prefix must not be handed each other's Job.
	other := workloadJobName(build, strings.Repeat("w", 45)+"x")
	if long == other {
		t.Fatalf("two workloads collided on one Job name: %s", long)
	}
	if short := workloadJobName("shop-bld-abc123", "api"); short != "shop-bld-abc123-api" {
		t.Fatalf("a name that fits is not hashed: %s", short)
	}
}
