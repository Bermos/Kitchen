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
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
)

// The two questions that are about the build rather than about its source
// (#500). Both of them are ways for the skip to be wrong, and both have to be
// answerable without asking the git provider anything.
func TestMayBeSkipped(t *testing.T) {
	project := func(setting bool) *kitchenv1alpha1.Project {
		return &kitchenv1alpha1.Project{
			Spec: kitchenv1alpha1.ProjectSpec{
				Build: kitchenv1alpha1.ProjectBuildSpec{SkipUnchanged: setting},
			},
		}
	}
	pushed := func() *kitchenv1alpha1.Build { return &kitchenv1alpha1.Build{} }
	asked := func() *kitchenv1alpha1.Build {
		return &kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{audit.RequestedByAnnotation: "ada@example.com"},
		}}
	}
	withConfig := func(build *kitchenv1alpha1.Build, declared *bool) *kitchenv1alpha1.Build {
		build.Status.Config = &kitchenv1alpha1.RepoConfig{
			Build: &kitchenv1alpha1.RepoBuildConfig{SkipUnchanged: declared},
		}
		return build
	}

	for name, tc := range map[string]struct {
		build   *kitchenv1alpha1.Build
		project *kitchenv1alpha1.Project
		want    bool
	}{
		"a project that never asked":                   {pushed(), project(false), false},
		"a project that asked":                         {pushed(), project(true), true},
		"a commit that turns it off":                   {withConfig(pushed(), ptr.To(false)), project(true), false},
		"a commit that turns it on":                    {withConfig(pushed(), ptr.To(true)), project(false), true},
		"a file that says nothing keeps the project's": {withConfig(pushed(), nil), project(true), true},
		// The escape hatch, and it has to hold whatever else is true: a
		// rebuild is what somebody reaches for when the derivation is wrong.
		"a build somebody asked for":                {asked(), project(true), false},
		"a build somebody asked for, file and all":  {withConfig(asked(), ptr.To(true)), project(true), false},
		"a build somebody asked for on a quiet one": {asked(), project(false), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := mayBeSkipped(tc.build, tc.project); got != tc.want {
				t.Errorf("mayBeSkipped = %v, want %v", got, tc.want)
			}
		})
	}
}

// The one sentence the condition, the commit status and the activity feed all
// carry. A build root of "." is the repository, and saying so is the
// difference between a readable line and one that reads as a missing value.
func TestSkipMessageNamesWhereAndWhat(t *testing.T) {
	message := skipMessage("services/api", "shop-bld-aaaa")
	for _, want := range []string{"services/api", "shop-bld-aaaa", "byte-identical"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q: %s", want, message)
		}
	}
	if whole := skipMessage("", "shop-bld-aaaa"); !strings.Contains(whole, "the repository") {
		t.Errorf("a project whose build root is the whole repository reads badly: %s", whole)
	}
}
