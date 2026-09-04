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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// kitchen.json declares the volumes the commit needs; it cannot ask for one
// to be made. So the build is where the declaration is met, and what it
// checks is the three ways a commit and a claim can disagree about the same
// volume — each of which otherwise deploys green and loses the data at the
// first restart.
func TestTheBuildHoldsTheCommitsVolumesAgainstTheProjectsClaims(t *testing.T) {
	const project = "plex"

	claim := func(name, config string) *kitchenv1alpha1.ResourceClaim {
		return &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kitchen-system"},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project},
				Type:       kitchenv1alpha1.ClaimTypeVolume,
				Config:     &runtime.RawExtension{Raw: []byte(config)},
			},
		}
	}
	claims := []*kitchenv1alpha1.ResourceClaim{
		claim("config", `{"volume": {"process": "web", "size": "50Gi", "mountPath": "/config"}}`),
		claim("media", `{"volume": {"source": "bind", "process": "web", "mountPath": "/media",
			"bind": {"persistentVolume": "nas-media", "accessMode": "ReadOnlyMany"}}}`),
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, c := range claims {
		builder = builder.WithObjects(c)
	}
	reconciler := &BuildReconciler{Client: builder.Build(), Scheme: scheme}
	proj := &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: project, Namespace: "kitchen-system"}}

	for name, testCase := range map[string]struct {
		volumes []kitchenv1alpha1.RepoVolume
		says    string
	}{
		"what the project has": {
			volumes: []kitchenv1alpha1.RepoVolume{
				{Name: "config", Process: "web", MountPath: "/config"},
				{Name: "media", Process: "web", MountPath: "/media",
					Source: kitchenv1alpha1.VolumeBind, AccessMode: "ReadOnlyMany"},
			},
		},
		"a claim that is not there": {
			volumes: []kitchenv1alpha1.RepoVolume{{Name: "ghost", Process: "web", MountPath: "/ghost"}},
			says:    "this project's volume claims are config, media",
		},
		"a mount path the claim disagrees with": {
			volumes: []kitchenv1alpha1.RepoVolume{{Name: "config", Process: "web", MountPath: "/var/config"}},
			says:    "the claim mounts it at /config",
		},
		"a process the claim disagrees with": {
			volumes: []kitchenv1alpha1.RepoVolume{{Name: "config", Process: "worker", MountPath: "/config"}},
			says:    "by process web",
		},
		"a source the claim disagrees with": {
			volumes: []kitchenv1alpha1.RepoVolume{
				{Name: "config", Process: "web", MountPath: "/config", Source: kitchenv1alpha1.VolumeBind},
			},
			says: "and the claim is provision",
		},
		"an access mode the claim disagrees with": {
			volumes: []kitchenv1alpha1.RepoVolume{
				{Name: "media", Process: "web", MountPath: "/media", AccessMode: "ReadWriteMany"},
			},
			says: "the claim mounts it ReadOnlyMany",
		},
		"a read-only mount of a volume the project cut for itself": {
			volumes: []kitchenv1alpha1.RepoVolume{
				{Name: "config", Process: "web", MountPath: "/config", AccessMode: "ReadOnlyMany"},
			},
			says: "which is always writable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := &kitchenv1alpha1.RepoConfig{Path: "kitchen.json", Volumes: testCase.volumes}
			err := reconciler.checkDeclaredVolumes(t.Context(), config, proj)
			if testCase.says == "" {
				if err != nil {
					t.Fatalf("the commit and the claims agree, and the build failed anyway: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the commit and the claim disagree, and the build did not say so")
			}
			if !strings.Contains(err.Error(), testCase.says) {
				t.Errorf("the message does not say what to change (%s): %v", testCase.says, err)
			}
		})
	}
}
