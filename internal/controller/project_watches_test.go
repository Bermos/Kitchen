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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func TestBuildsAndEnvironmentsMapBackToTheirProject(t *testing.T) {
	r := &ProjectReconciler{}
	const project = "shop"

	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-aaaaaaaaaaaa", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project},
		},
	}
	requests := r.mapBuildToProject(context.Background(), build)
	if len(requests) != 1 {
		t.Fatalf("a build should enqueue its project, got %d requests", len(requests))
	}
	if requests[0].Name != project || requests[0].Namespace != PlatformNamespace {
		t.Errorf("build mapped to %s/%s, want %s/shop", requests[0].Namespace, requests[0].Name, PlatformNamespace)
	}

	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project},
		},
	}
	requests = r.mapEnvironmentToProject(context.Background(), env)
	if len(requests) != 1 {
		t.Fatalf("an environment should enqueue its project, got %d requests", len(requests))
	}
	if requests[0].Name != project || requests[0].Namespace != PlatformNamespace {
		t.Errorf("environment mapped to %s/%s, want %s/shop", requests[0].Namespace, requests[0].Name, PlatformNamespace)
	}
}

func TestNothingWithoutAProjectIsMapped(t *testing.T) {
	r := &ProjectReconciler{}
	cases := map[string]client.Object{
		"a build naming no project":        &kitchenv1alpha1.Build{},
		"an environment naming no project": &kitchenv1alpha1.Environment{},
		"an object of the wrong kind":      &corev1.Pod{},
	}
	for name, obj := range cases {
		t.Run(name, func(t *testing.T) {
			if requests := r.mapBuildToProject(context.Background(), obj); requests != nil {
				t.Errorf("mapBuildToProject enqueued %v", requests)
			}
			if requests := r.mapEnvironmentToProject(context.Background(), obj); requests != nil {
				t.Errorf("mapEnvironmentToProject enqueued %v", requests)
			}
		})
	}
}

// A project reconcile re-registers the git webhook with the provider, so the
// watches are deliberately deaf to the status churn a running build and a
// running environment produce: neither can change which build is newest or
// whether the production environment exists.
func TestOnlyExistenceWakesTheProject(t *testing.T) {
	p := existenceChanged()
	if !p.Create(event.CreateEvent{}) {
		t.Error("a created build or environment should reconcile its project")
	}
	if !p.Delete(event.DeleteEvent{}) {
		t.Error("a deleted build or environment should reconcile its project")
	}
	if p.Update(event.UpdateEvent{}) {
		t.Error("a status update should not reconcile the project")
	}
}
