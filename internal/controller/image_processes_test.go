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
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider"
)

// processStubResolver answers what a registry would about a buildpacks image,
// without one. A nil entry for a reference is a read that failed, which is
// what an unreachable registry looks like from here.
type processStubResolver struct {
	types map[string][]string
	found map[string]bool
	fail  map[string]bool
}

func (p *processStubResolver) Resolve(_ context.Context, ref string) (string, error) { return ref, nil }

func (p *processStubResolver) ImageProcessTypes(_ context.Context, ref string) ([]string, bool, error) {
	if p.fail[ref] {
		return nil, false, errors.New("the registry could not be reached")
	}
	found, declared := p.found[ref]
	if !declared {
		found = true
	}
	return p.types[ref], found, nil
}

// blindResolver has no opinion about an image's processes, which is what every
// installation that cannot read the label looks like.
type blindResolver struct{}

func (blindResolver) Resolve(_ context.Context, ref string) (string, error) { return ref, nil }

const (
	processTestNamespace = "kitchen-system"
	// The workload the per-workload cases are about, named once: the linter
	// counts a literal compared against, and this file compares this one.
	workerWorkload = "worker"
)

func startlessFixture(resolver ImageResolver) (*BuildReconciler, buildTarget) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = kitchenv1alpha1.AddToScheme(scheme)

	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: processTestNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "registry-creds"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: processTestNamespace},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(connection, secret).Build()

	return &BuildReconciler{
			Client:    client,
			Scheme:    scheme,
			Resolvers: func([]byte, string) (ImageResolver, error) { return resolver, nil },
		}, buildTarget{
			Connection: connection,
			Registry:   provider.RegistryTarget{Server: "registry.example.com"},
			Namespace:  "kitchen-shop",
		}
}

func buildpacksBuild(workloads ...kitchenv1alpha1.WorkloadBuildStatus) *kitchenv1alpha1.Build {
	return &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-1", Namespace: processTestNamespace},
		Status: kitchenv1alpha1.BuildStatus{
			Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop", Digest: "sha256:web",
			},
			Workloads: workloads,
		},
	}
}

// The dead end #440 is about: an image whose buildpacks declared no process
// type, deployed by a project that supplies no command. The launcher exits
// saying a command is required, for ever — so the Release is refused instead.
func TestABuildpacksImageThatDeclaresNoProcessIsRefused(t *testing.T) {
	reconciler, target := startlessFixture(&processStubResolver{
		types: map[string][]string{"registry.example.com/shop@sha256:web": {}},
	})

	found := reconciler.startlessWorkloads(context.Background(), buildpacksBuild(), target,
		kitchenv1alpha1.ConfigSnapshot{})
	if len(found) != 1 || found[0].Name() != kitchenv1alpha1.WebProcessName {
		t.Fatalf("the web process should be refused, got %+v", found)
	}
}

// Everything else is left alone, and each for its own reason.
func TestWhatIsNotRefusedForItsProcessTypes(t *testing.T) {
	web := "registry.example.com/shop@sha256:web"

	for name, tc := range map[string]struct {
		build    *kitchenv1alpha1.Build
		snapshot kitchenv1alpha1.ConfigSnapshot
		resolver ImageResolver
	}{
		"the image declares a process type": {
			build:    buildpacksBuild(),
			resolver: &processStubResolver{types: map[string][]string{web: {"web"}}},
		},
		"the project supplies a command": {
			build: buildpacksBuild(),
			snapshot: kitchenv1alpha1.ConfigSnapshot{
				Runtime: kitchenv1alpha1.RuntimeSpec{Command: []string{"node", ".output/server/index.mjs"}},
			},
			resolver: &processStubResolver{types: map[string][]string{web: {}}},
		},
		"the image was built from a Dockerfile": {
			build: func() *kitchenv1alpha1.Build {
				build := buildpacksBuild()
				build.Status.Strategy = kitchenv1alpha1.BuildStrategyDockerfile
				return build
			}(),
			resolver: &processStubResolver{types: map[string][]string{web: {}}},
		},
		"the image carries no lifecycle metadata at all": {
			build:    buildpacksBuild(),
			resolver: &processStubResolver{found: map[string]bool{web: false}},
		},
		"the label could not be read": {
			build:    buildpacksBuild(),
			resolver: &processStubResolver{fail: map[string]bool{web: true}},
		},
		"the resolver has no opinion about processes": {
			build:    buildpacksBuild(),
			resolver: blindResolver{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			reconciler, target := startlessFixture(tc.resolver)
			if found := reconciler.startlessWorkloads(
				context.Background(), tc.build, target, tc.snapshot); len(found) != 0 {
				t.Fatalf("nothing should be refused here: %+v", found)
			}
		})
	}
}

// A unit is several images, and the question is asked of each: the workload
// built from a Dockerfile keeps its entrypoint, and the one that declares a
// command of its own runs it through the launcher.
func TestTheProcessTypeQuestionIsAskedPerWorkload(t *testing.T) {
	const (
		web    = "registry.example.com/shop@sha256:web"
		worker = "registry.example.com/shop-worker@sha256:worker"
		api    = "registry.example.com/shop-api@sha256:api"
	)
	build := buildpacksBuild(
		kitchenv1alpha1.WorkloadBuildStatus{
			Name: workerWorkload, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop-worker", Digest: "sha256:worker"},
		},
		kitchenv1alpha1.WorkloadBuildStatus{
			Name: "api", Strategy: kitchenv1alpha1.BuildStrategyDockerfile,
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop-api", Digest: "sha256:api"},
		},
	)
	snapshot := kitchenv1alpha1.ConfigSnapshot{
		Runtime: kitchenv1alpha1.RuntimeSpec{Command: []string{"node", "server.mjs"}},
		Processes: []kitchenv1alpha1.ProcessSpec{
			{Name: workerWorkload, Type: kitchenv1alpha1.ProcessWorker},
			{Name: "api", Type: kitchenv1alpha1.ProcessService, Port: 8080},
		},
	}
	reconciler, target := startlessFixture(&processStubResolver{
		types: map[string][]string{web: {}, worker: {}, api: {}},
	})

	found := reconciler.startlessWorkloads(context.Background(), build, target, snapshot)
	if len(found) != 1 || found[0].Name() != workerWorkload {
		t.Fatalf("only the worker should be refused, got %+v", found)
	}
}

// The message is the whole fix: which workload, which image, what the
// launcher would have said, and the field that settles it.
func TestTheProcessTypeRefusalNamesTheFix(t *testing.T) {
	message := startlessWorkloadsMessage([]kitchenv1alpha1.BuildArtifact{
		{Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "registry.example.com/shop", Digest: "sha256:abc",
		}},
	})

	for _, want := range []string{
		kitchenv1alpha1.WebProcessName,
		"registry.example.com/shop@sha256:abc",
		"when there is no default process a command is required",
		"runtime.command",
		"launcher",
		"Procfile",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, message)
		}
	}
}
