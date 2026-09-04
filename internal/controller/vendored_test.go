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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Software the platform did not build (#307).
//
// The cases here are the acceptance criteria of that issue, in the order it
// states them: a workload can name an image nobody here built, a project's
// source can be one, the pods pull with the image's own credential rather
// than the push credential, a mixed unit rolls back as one, and a project
// with no repository is refused previews in words rather than quietly given
// none.

// stubResolver answers what a registry would, without one. It records what it
// was asked, so a case can assert that a digest already pinned costs no
// request at all.
type stubResolver struct {
	digests map[string]string
	asked   []string
}

func (s *stubResolver) Resolve(_ context.Context, ref string) (string, error) {
	s.asked = append(s.asked, ref)
	if digest, ok := s.digests[ref]; ok {
		return digest, nil
	}
	return "", fmt.Errorf("no such image %q", ref)
}

var _ = Describe("A workload that runs an image this platform did not build", func() {
	const (
		namespace = "default"

		// The home lab's case: a project with no repository at all.
		vendorProject = "homeassistant"
		vendorRepo    = "ghcr.io/home-assistant/home-assistant"
		vendorTag     = "2026.9.1"
		vendorDigest  = "sha256:" +
			"1111111111111111111111111111111111111111111111111111111111111111"

		// The mixed case: a repository this platform builds, with an
		// upstream image beside it as one workload of the same unit.
		mixedProject = "mixedshop"
		sidecarRepo  = "docker.io/library/redis"
		sidecarTag   = "7.4"
		sidecarPin   = "sha256:" +
			"2222222222222222222222222222222222222222222222222222222222222222"
		builtWeb = "registry.example.com/kitchen/mixedshop@sha256:" +
			"3333333333333333333333333333333333333333333333333333333333333333"
	)

	ctx := context.Background()

	vendorRef := vendorRepo + ":" + vendorTag
	vendorImage := vendorRepo + "@" + vendorDigest
	sidecarRef := sidecarRepo + ":" + sidecarTag
	sidecarImage := sidecarRepo + "@" + sidecarPin

	// created is everything a case made, torn down in reverse. envtest runs
	// no garbage collector, so nothing here is owned by anything else.
	var created []client.Object

	track := func(obj client.Object) client.Object {
		created = append(created, obj)
		return obj
	}

	create := func(obj client.Object) {
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
		track(obj)
	}

	// pullConnection is a dockerRegistry Connection with a docker config in
	// it: the credential a private vendored image is pulled with, which is
	// deliberately not the credential anything pushes under.
	pullConnection := func(name string) {
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-creds", Namespace: namespace},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
				`{"auths":{"ghcr.io":{"username":"bot","password":"secret"}}}`)},
		})
		create(&kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: name + "-creds"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"url":"https://ghcr.io"}`)},
			},
		})
	}

	AfterEach(func() {
		for i := len(created) - 1; i >= 0; i-- {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, created[i]))).To(Succeed())
		}
		created = nil
	})

	Describe("what admission refuses", func() {
		project := func(spec kitchenv1alpha1.ProjectSpec) *kitchenv1alpha1.Project {
			return &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-", Namespace: namespace},
				Spec:       spec,
			}
		}
		git := func() *kitchenv1alpha1.GitSourceSpec {
			return &kitchenv1alpha1.GitSourceSpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:          "acme/shop",
			}
		}
		registry := func() *kitchenv1alpha1.RegistrySpec {
			return &kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
			}
		}
		image := func() *kitchenv1alpha1.ImageSourceSpec {
			return &kitchenv1alpha1.ImageSourceSpec{Repository: vendorRepo, Tag: vendorTag}
		}

		It("refuses a project whose source is neither a repository nor an image", func() {
			err := k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("a project's source is one thing"))
		})

		It("refuses a project whose source is both", func() {
			err := k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{
				Source:   kitchenv1alpha1.ProjectSourceSpec{Git: git(), Image: image()},
				Registry: registry(),
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("a project's source is one thing"))
		})

		It("refuses a vendored image that names no version", func() {
			err := k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{
					Image: &kitchenv1alpha1.ImageSourceSpec{Repository: vendorRepo},
				},
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("needs a tag or a digest"))
		})

		It("refuses a repository with no registry to push to, and an image with one", func() {
			err := k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: git()},
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("a registry is where built images are pushed"))

			err = k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{
				Source:   kitchenv1alpha1.ProjectSourceSpec{Image: image()},
				Registry: registry(),
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("a registry is where built images are pushed"))
		})

		It("refuses a workload that is both built and vendored", func() {
			err := k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{
				Source:   kitchenv1alpha1.ProjectSourceSpec{Git: git()},
				Registry: registry(),
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name:  "api",
					Type:  kitchenv1alpha1.ProcessService,
					Port:  8080,
					Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/api"},
					Image: image(),
				}},
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("never both"))
		})

		It("refuses a workload built from a repository the project does not have", func() {
			err := k8sClient.Create(ctx, project(kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Image: image()},
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name:  "api",
					Type:  kitchenv1alpha1.ProcessService,
					Port:  8080,
					Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/api"},
				}},
			}))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("needs a repository"))
		})

		It("takes a vendored workload beside a built one, which is what the shape is for", func() {
			create(project(kitchenv1alpha1.ProjectSpec{
				Source:   kitchenv1alpha1.ProjectSourceSpec{Git: git()},
				Registry: registry(),
				Processes: []kitchenv1alpha1.ProcessSpec{
					{
						Name:  "api",
						Type:  kitchenv1alpha1.ProcessService,
						Port:  8080,
						Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/api"},
					},
					{
						Name: "cache",
						Type: kitchenv1alpha1.ProcessService,
						Port: 6379,
						Image: &kitchenv1alpha1.ImageSourceSpec{
							Repository: sidecarRepo, Tag: sidecarTag,
						},
					},
				},
			}))
		})

		It("refuses a Build that names half a commit, and takes one that names none", func() {
			err := k8sClient.Create(ctx, &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-bld-", Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "cel"},
					Git:        kitchenv1alpha1.GitRevision{SHA: "abc1234def56"},
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("names the commit it builds"))

			create(&kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "cel-acq-", Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "cel"},
				},
			})
		})
	})

	Describe("a project with no repository at all", func() {
		var resolver *stubResolver
		var builds *BuildReconciler
		var project *kitchenv1alpha1.Project

		appNS := appNamespace(vendorProject)

		BeforeEach(func() {
			resolver = &stubResolver{digests: map[string]string{
				vendorRef:  vendorImage,
				sidecarRef: sidecarImage,
			}}
			builds = &BuildReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
				Resolvers: func([]byte, string) (ImageResolver, error) { return resolver, nil },
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
			}))).To(Succeed())
			ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
			})
			pullConnection("ghcr")

			project = &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: vendorProject, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.ProjectSourceSpec{Image: &kitchenv1alpha1.ImageSourceSpec{
						Repository:    vendorRepo,
						Tag:           vendorTag,
						ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "ghcr"},
					}},
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8123},
				},
			}
			create(project)
			track(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: registrySecretName("ghcr"), Namespace: appNS}})
		})

		It("needs no registry and no source connection, and is refused previews in words", func() {
			projects := &ProjectReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: vendorProject, Namespace: namespace}
			_, err := projects.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, project)).To(Succeed())
			By("not asking for connections it has no use for")
			Expect(meta.FindStatusCondition(project.Status.Conditions, condSourceConnected)).To(BeNil())
			Expect(meta.FindStatusCondition(project.Status.Conditions, condRegistryConnected)).To(BeNil())
			Expect(meta.IsStatusConditionTrue(project.Status.Conditions, condReady)).To(BeTrue(),
				"a project that builds nothing is ready without the two connections a build needs")

			By("saying why there are no previews rather than simply never making one")
			previews := meta.FindStatusCondition(project.Status.Conditions, condPreviews)
			Expect(previews).NotTo(BeNil())
			Expect(previews.Status).To(Equal(metav1.ConditionFalse))
			Expect(previews.Reason).To(Equal(reasonNoRepository))
			Expect(previews.Message).To(ContainSubstring("no repository to open one against"))
			Expect(previews.Message).To(ContainSubstring(vendorRef))
			Expect(previewsEnabled(project)).To(BeFalse())

			By("seeding the acquisition, so that creating the project deploys it")
			Expect(project.Status.InitialBuildRef).NotTo(BeNil())
			seeded := project.Status.InitialBuildRef.Name
			Expect(seeded).To(Equal(AcquisitionNameFor(vendorProject, vendorRef)))
			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: seeded, Namespace: namespace}, build)).To(Succeed())
			track(build)
			Expect(build.FromRepository()).To(BeFalse(), "nothing fakes a commit")
		})

		It("resolves the digest and releases it without running a builder", func() {
			build := &kitchenv1alpha1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: vendorProject + "-acq-1", Namespace: namespace},
				Spec: kitchenv1alpha1.BuildSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: vendorProject},
				},
			}
			create(build)
			key := types.NamespacedName{Name: build.Name, Namespace: namespace}
			_, err := builds.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, build)).To(Succeed())

			Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildSucceeded))
			Expect(build.Status.Image).To(Equal(vendorImage))
			Expect(resolver.asked).To(ContainElement(vendorRef))

			By("running no builder Job: there is nothing to build")
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(appNS))).To(Succeed())
			Expect(jobs.Items).To(BeEmpty())

			By("freezing the digest it resolved onto a Release")
			release := &kitchenv1alpha1.Release{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: acquisitionReleaseName(vendorProject, vendorImage), Namespace: namespace,
			}, release)).To(Succeed())
			track(release)
			Expect(release.Spec.Image).To(Equal(vendorImage))
			Expect(release.Spec.BuildRef.Name).To(Equal(build.Name))

			By("putting the pull credential where the pods will need it")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: registrySecretName("ghcr"), Namespace: appNS}, secret)).To(Succeed())

			By("deploying it with that credential and no other")
			env := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: vendorProject + "-production", Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: vendorProject},
					Type:       kitchenv1alpha1.EnvironmentProduction,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: release.Name},
				},
			}
			create(env)
			environments := &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			envKey := types.NamespacedName{Name: env.Name, Namespace: namespace}
			for range 2 {
				_, err := environments.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
				Expect(err).NotTo(HaveOccurred())
			}
			deploy := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: env.Name, Namespace: appNS}, deploy)).To(Succeed())
			Expect(deploy.Spec.Template.Spec.Containers[0].Image).To(Equal(vendorImage))
			Expect(deploy.Spec.Template.Spec.ImagePullSecrets).To(Equal(
				[]corev1.LocalObjectReference{{Name: registrySecretName("ghcr")}}),
				"the pods pull with the image's own credential, not the push credential")
		})

		It("pulls a public image with no credential at all", func() {
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: vendorProject, Namespace: namespace}, project)).To(Succeed())
			project.Spec.Source.Image.ConnectionRef = nil
			Expect(k8sClient.Update(ctx, project)).To(Succeed())

			Expect(pullSecretsFor(project, nil, kitchenv1alpha1.WebProcessName)).To(BeNil(),
				"naming a Secret that does not exist would keep the pod from ever starting")
		})
	})

	Describe("a unit of a built workload and a vendored one", func() {
		var project *kitchenv1alpha1.Project
		appNS := appNamespace(mixedProject)

		// two releases: the second is what the first rolls back from, and
		// both name a vendored digest as well as a built one.
		olderSidecar := sidecarRepo + "@sha256:" +
			"4444444444444444444444444444444444444444444444444444444444444444"

		release := func(name, sidecar string) *kitchenv1alpha1.Release {
			return &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: mixedProject},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: mixedProject + "-bld-1"},
					Image:      builtWeb,
					Workloads: []kitchenv1alpha1.WorkloadImage{
						{Name: "cache", Image: sidecar},
					},
					ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
						Runtime: kitchenv1alpha1.RuntimeSpec{Port: 3000},
						Processes: []kitchenv1alpha1.ProcessSpec{{
							Name: "cache",
							Type: kitchenv1alpha1.ProcessService,
							Port: 6379,
							Image: &kitchenv1alpha1.ImageSourceSpec{
								Repository:    sidecarRepo,
								Tag:           sidecarTag,
								ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "ghcr"},
							},
						}},
					},
				},
			}
		}

		BeforeEach(func() {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
			}))).To(Succeed())
			ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
			})
			pullConnection("ghcr")
			project = &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: mixedProject, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
						Repo:          "acme/mixedshop",
					}},
					Registry: &kitchenv1alpha1.RegistrySpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
					},
					Processes: []kitchenv1alpha1.ProcessSpec{{
						Name: "cache",
						Type: kitchenv1alpha1.ProcessService,
						Port: 6379,
						Image: &kitchenv1alpha1.ImageSourceSpec{
							Repository:    sidecarRepo,
							Tag:           sidecarTag,
							ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "ghcr"},
						},
					}},
				},
			}
			create(project)
		})

		It("deploys both halves and rolls back the vendored digest exactly as the built one", func() {
			newer := release(mixedProject+"-rel-2", sidecarImage)
			older := release(mixedProject+"-rel-1", olderSidecar)
			create(older)
			create(newer)

			env := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: mixedProject + "-production", Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: mixedProject},
					Type:       kitchenv1alpha1.EnvironmentProduction,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: newer.Name},
				},
			}
			create(env)
			environments := &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			key := types.NamespacedName{Name: env.Name, Namespace: namespace}
			reconcileTwice := func() {
				for range 2 {
					_, err := environments.Reconcile(ctx, reconcile.Request{NamespacedName: key})
					ExpectWithOffset(1, err).NotTo(HaveOccurred())
				}
			}
			reconcileTwice()

			web := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: env.Name, Namespace: appNS}, web)).To(Succeed())
			Expect(web.Spec.Template.Spec.Containers[0].Image).To(Equal(builtWeb))
			Expect(web.Spec.Template.Spec.ImagePullSecrets).To(Equal(
				[]corev1.LocalObjectReference{{Name: registrySecretName("registry")}}),
				"the built half still pulls with what the build pushed under")

			cache := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: env.Name + "-cache", Namespace: appNS}, cache)).To(Succeed())
			Expect(cache.Spec.Template.Spec.Containers[0].Image).To(Equal(sidecarImage))
			Expect(cache.Spec.Template.Spec.ImagePullSecrets).To(Equal(
				[]corev1.LocalObjectReference{{Name: registrySecretName("ghcr")}}),
				"the vendored half pulls with the image's own credential, in the same unit")

			By("rolling the whole unit back")
			Expect(k8sClient.Get(ctx, key, env)).To(Succeed())
			env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: older.Name}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())
			reconcileTwice()

			Expect(k8sClient.Get(ctx,
				types.NamespacedName{Name: env.Name + "-cache", Namespace: appNS}, cache)).To(Succeed())
			Expect(cache.Spec.Template.Spec.Containers[0].Image).To(Equal(olderSidecar),
				"a rollback restores the vendored digest that release declared, not the tag's current content")
		})
	})
})

// The unit-level half of the same question: which credential one workload's
// pods pull with, asked of the project and the release's own process list.
var _ = Describe("The credential a workload pulls with", func() {
	project := func(source kitchenv1alpha1.ProjectSourceSpec, registry string) *kitchenv1alpha1.Project {
		spec := kitchenv1alpha1.ProjectSpec{Source: source}
		if registry != "" {
			spec.Registry = &kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: registry},
			}
		}
		return &kitchenv1alpha1.Project{Spec: spec}
	}

	It("is the push credential for anything this platform built", func() {
		built := project(kitchenv1alpha1.ProjectSourceSpec{
			Git: &kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"},
		}, "registry")
		processes := []kitchenv1alpha1.ProcessSpec{{
			Name:  "api",
			Type:  kitchenv1alpha1.ProcessService,
			Build: &kitchenv1alpha1.ProcessBuildSpec{RootDirectory: "services/api"},
		}}
		Expect(pullSecretName(built, processes, kitchenv1alpha1.WebProcessName)).
			To(Equal("kitchen-registry-registry"))
		Expect(pullSecretName(built, processes, "api")).To(Equal("kitchen-registry-registry"))
	})

	It("is the image's own where the workload names one, and nothing where it is public", func() {
		built := project(kitchenv1alpha1.ProjectSourceSpec{
			Git: &kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"},
		}, "registry")
		processes := []kitchenv1alpha1.ProcessSpec{
			{
				Name: "private",
				Type: kitchenv1alpha1.ProcessService,
				Image: &kitchenv1alpha1.ImageSourceSpec{
					Repository:    "ghcr.io/acme/thing",
					Tag:           "1",
					ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "ghcr"},
				},
			},
			{
				Name: "public",
				Type: kitchenv1alpha1.ProcessService,
				Image: &kitchenv1alpha1.ImageSourceSpec{
					Repository: "docker.io/library/redis", Tag: "7.4",
				},
			},
		}
		Expect(pullSecretName(built, processes, "private")).To(Equal("kitchen-registry-ghcr"))
		Expect(pullSecretName(built, processes, "public")).To(BeEmpty())
	})
})

// registryServerOf is what a docker config's key is, and Docker Hub is the one
// host in the world spelled by omission.
var _ = Describe("Which registry a repository reference is on", func() {
	It("names the host where there is one and Docker Hub where there is not", func() {
		Expect(registryServerOf("ghcr.io/home-assistant/home-assistant")).To(Equal("ghcr.io"))
		Expect(registryServerOf("registry.example.com:5000/team/app")).To(Equal("registry.example.com:5000"))
		Expect(registryServerOf("localhost/app")).To(Equal("localhost"))
		Expect(registryServerOf("library/postgres")).To(Equal("index.docker.io"))
		Expect(registryServerOf("postgres")).To(Equal("index.docker.io"))
	})
})

// A Build and a project that disagree about what kind of thing is being
// deployed is refused rather than half-run.
var _ = Describe("A Build whose shape does not match its project", func() {
	const namespace = "default"
	ctx := context.Background()

	It("refuses a commit for a project with no repository", func() {
		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: "mismatch", Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Image: &kitchenv1alpha1.ImageSourceSpec{
					Repository: "docker.io/library/redis", Tag: "7.4",
				}},
				Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(false)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())
		build := &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: "mismatch-bld-1", Namespace: namespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "mismatch"},
				Git:        kitchenv1alpha1.GitRevision{SHA: "abc1234def56", Branch: "main"},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, build))).To(Succeed())
		defer func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, build))).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, project))).To(Succeed())
		}()

		builds := &BuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient}
		key := types.NamespacedName{Name: build.Name, Namespace: namespace}
		_, err := builds.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, key, build)).To(Succeed())
		Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildFailed))
		condition := meta.FindStatusCondition(build.Status.Conditions, condReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal(reasonSourceMismatch))
		Expect(condition.Message).To(ContainSubstring("has no repository"))
	})
})
