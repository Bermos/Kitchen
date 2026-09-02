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

package detect

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// stubSource is a repository as file contents, which is what reading a
// Dockerfile's stages needs and what treeReader — a repository as a list of
// names — deliberately does not carry.
type stubSource struct {
	files map[string]string
}

func (stubSource) ListDir(context.Context, string, string, string) ([]gitprovider.DirEntry, error) {
	return nil, fmt.Errorf("%w: nothing is listed here", gitprovider.ErrFileNotFound)
}

func (s *stubSource) ReadFile(_ context.Context, _, _, file string) ([]byte, error) {
	if content, ok := s.files[file]; ok {
		return []byte(content), nil
	}
	return nil, fmt.Errorf("%w: %s", gitprovider.ErrFileNotFound, file)
}

// The stages a Dockerfile declares are what somebody chooses a target from, so
// the reader has to find the names a build would find — including the ones
// written in a form nobody expects to have to handle.
func TestTheStagesADockerfileDeclares(t *testing.T) {
	for name, tc := range map[string]struct {
		dockerfile string
		want       []string
	}{
		"a single-stage file names none": {
			dockerfile: "FROM alpine\nRUN echo hi\n",
			want:       []string{},
		},
		"in the order the file declares them": {
			dockerfile: "FROM node AS deps\nFROM deps AS build\nFROM nginx AS web\n",
			want:       []string{"deps", "build", "web"},
		},
		"lowercase instructions and keywords": {
			dockerfile: "from node as build\nfrom nginx as web\n",
			want:       []string{"build", "web"},
		},
		"a platform flag before the image": {
			dockerfile: "FROM --platform=$BUILDPLATFORM node:22 AS build\n",
			want:       []string{"build"},
		},
		"a continuation onto the next line": {
			dockerfile: "FROM \\\n  node:22 AS build\n",
			want:       []string{"build"},
		},
		"a commented-out stage is not a stage": {
			dockerfile: "# FROM node AS old\nFROM node AS build\n",
			want:       []string{"build"},
		},
		"the last stage unnamed contributes nothing": {
			dockerfile: "FROM node AS build\nFROM nginx\n",
			want:       []string{"build"},
		},
		"an empty file": {dockerfile: "", want: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Stages([]byte(tc.dockerfile)); !slices.Equal(got, tc.want) {
				t.Errorf("stages of %q: got %v, want %v", tc.dockerfile, got, tc.want)
			}
		})
	}
}

// A target is refused where it is written when no stage could be called that,
// because a name the frontend cannot hold could never match one.
func TestWhichTargetsAreNamesAStageCouldHave(t *testing.T) {
	for target, valid := range map[string]bool{
		"":                  true,
		"web":               true,
		"Web":               true,
		"build-2":           true,
		"node_22.x":         true,
		"1st":               false,
		"-web":              false,
		"web stage":         false,
		"web/stage":         false,
		"../etc/passwd":     false,
		"$(whoami)":         false,
		"--output=type=oci": false,
	} {
		if got := ValidTarget(target); got != valid {
			t.Errorf("ValidTarget(%q) = %v, want %v", target, got, valid)
		}
	}
}

// The frontend matches a stage name without regard to case, so the platform
// has to agree with it about which targets are the file's.
func TestATargetMatchesAStageWithoutRegardToCase(t *testing.T) {
	stages := []string{"deps", "Web"}
	for target, want := range map[string]bool{
		"web":    true,
		"WEB":    true,
		"deps":   true,
		"":       true,
		"worker": false,
	} {
		if got := HasStage(stages, target); got != want {
			t.Errorf("HasStage(%v, %q) = %v, want %v", stages, target, got, want)
		}
	}
}

// Reading the file is the preflight's own step: it is relative to the build
// root like everything else the project declares, and a file that is not there
// is an answer rather than a failure.
func TestReadingTheStagesOfAProjectsDockerfile(t *testing.T) {
	reader := &stubSource{files: map[string]string{
		"apps/shop/docker/prod.Dockerfile": "FROM node AS build\nFROM nginx AS web\n",
	}}
	target := Target{
		Repo: "acme/shop", Ref: "main",
		RootDirectory: "apps/shop", DockerfilePath: "docker/prod.Dockerfile",
	}

	stages, err := DockerfileStages(context.Background(), reader, target)
	if err != nil {
		t.Fatalf("reading the stages: %v", err)
	}
	if !slices.Equal(stages, []string{"build", "web"}) {
		t.Errorf("stages: got %v", stages)
	}

	// A path that leaves the build root names a file no build can read, so it
	// is not read here either.
	target.DockerfilePath = "../../Dockerfile"
	if _, err := DockerfileStages(context.Background(), reader, target); err == nil {
		t.Error("a Dockerfile above the build root was read as this project's")
	}
}
