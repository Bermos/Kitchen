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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What the repository declares is recorded on the project, so that everything
// asking "is bridge one of this project's processes" can be asked (#593).
//
// Before this, the answer lived on the Build alone: the build built the
// workload, the environment ran it, the processes endpoint reported it
// healthy, and POST /claims refused a volume for it as naming a process the
// project did not have.
func TestAProductionBuildRecordsWhatTheRepositoryDeclares(t *testing.T) {
	project := projectFor("services")
	succeeded := buildFor("services-4f2c9ab", "4f2c9ab", time.Now().Add(-time.Hour))
	succeeded.Status.Phase = kitchenv1alpha1.BuildSucceeded
	succeeded.Status.Config = &kitchenv1alpha1.RepoConfig{
		Path: kitchenv1alpha1.RepoConfigFileName,
		Processes: []kitchenv1alpha1.ProcessSpec{
			{Name: "bridge", Type: kitchenv1alpha1.ProcessService, Port: 8080},
		},
	}

	r := reconcilerWith(t, project, succeeded)
	r.updateReferences(context.Background(), project)

	declared := project.Status.DeclaredProcesses
	if declared == nil {
		t.Fatal("the declaration was not recorded")
	}
	if declared.Build != succeeded.Name || declared.Commit != "4f2c9ab" {
		t.Errorf("the record does not say where it came from: %+v", declared)
	}
	if len(declared.Processes) != 1 || declared.Processes[0].Name != "bridge" {
		t.Fatalf("the file's workloads were not recorded: %+v", declared.Processes)
	}
	if names := project.ProcessNames(); len(names) != 2 || names[1] != "bridge" {
		t.Errorf("a claim would still be refused: %v", names)
	}
}

// A preview's file declares nothing, and neither does a failed build's.
//
// The preview is the one that matters: the author of a pull request's
// kitchen.json need not be anybody with access to the project, so a workload
// list read from one would let a fork declare a workload and then claim a
// volume for it.
func TestOnlyASucceededProductionBuildMaySayWhatTheRepositoryDeclares(t *testing.T) {
	config := &kitchenv1alpha1.RepoConfig{
		Path:      kitchenv1alpha1.RepoConfigFileName,
		Processes: []kitchenv1alpha1.ProcessSpec{{Name: "bridge", Type: kitchenv1alpha1.ProcessWorker}},
	}

	preview := buildFor("services-preview", "aaaaaaa", time.Now())
	preview.Status.Phase = kitchenv1alpha1.BuildSucceeded
	preview.Status.Config = config
	preview.Annotations = map[string]string{kitchenv1alpha1.PullRequestAnnotation: "7"}

	failed := buildFor("services-failed", "bbbbbbb", time.Now().Add(-time.Minute))
	failed.Status.Phase = kitchenv1alpha1.BuildFailed
	failed.Status.Config = config

	project := projectFor("services")
	r := reconcilerWith(t, project, preview, failed)
	r.updateReferences(context.Background(), project)

	if project.Status.DeclaredProcesses != nil {
		t.Errorf("a preview or a failed build declared workloads: %+v", project.Status.DeclaredProcesses)
	}
}

// A file that stops declaring workloads takes the record with it: the
// project's own list is the whole answer again, and a claim written against a
// workload nothing declares any more is refused rather than left pointing at
// a mount no deploy would make.
func TestADeclarationThatStopsBeingMadeIsCleared(t *testing.T) {
	project := projectFor("services")
	project.Status.DeclaredProcesses = &kitchenv1alpha1.DeclaredProcesses{
		Build:     "services-old",
		Processes: []kitchenv1alpha1.ProcessSpec{{Name: "bridge", Type: kitchenv1alpha1.ProcessService}},
	}

	newer := buildFor("services-new", "ccccccc", time.Now())
	newer.Status.Phase = kitchenv1alpha1.BuildSucceeded
	newer.Status.Config = &kitchenv1alpha1.RepoConfig{Path: kitchenv1alpha1.RepoConfigFileName}

	r := reconcilerWith(t, project, newer)
	r.updateReferences(context.Background(), project)

	if project.Status.DeclaredProcesses != nil {
		t.Errorf("the record outlived the declaration: %+v", project.Status.DeclaredProcesses)
	}
}

func projectFor(name string) *kitchenv1alpha1.Project {
	return &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace},
	}
}

func buildFor(name, sha string, created time.Time) *kitchenv1alpha1.Build {
	return &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         PlatformNamespace,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "services"},
			Git:        kitchenv1alpha1.GitRevision{SHA: sha},
		},
	}
}

func reconcilerWith(t *testing.T, objects ...client.Object) *ProjectReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, object := range objects {
		builder = builder.WithObjects(object)
	}
	c := builder.Build()
	return &ProjectReconciler{Client: c, Scheme: scheme}
}
