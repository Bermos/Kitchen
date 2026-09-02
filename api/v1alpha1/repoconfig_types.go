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

package v1alpha1

import (
	"fmt"
	"slices"
)

// RepoConfigFileName is the file a repository declares its own build and
// runtime settings in — Kitchen's answer to `vercel.json`.
//
// It is read from the project's build root at the commit under build, which
// is the one place a monorepo's second project can put its own copy. That is
// also why the file cannot set the root directory itself: the platform has to
// know where the project is before it can read anything, and a file that
// moved the directory it was read from would have to be read twice to find
// out where to read it. The root directory stays on the project, where the
// new-project form already asks for it.
const RepoConfigFileName = "kitchen.json"

// RepoConfig is what a repository's kitchen.json declared, recorded on the
// Build that read it.
//
// It is stored rather than re-read because the file is read once — in the
// pass that creates the build Job — and applied twice: to the Job, and to the
// Release snapshot a later pass writes. Reading it again at the end would
// spend a second provider request to answer a question already answered, and
// would answer it differently if the branch moved underneath.
//
// Every field is a pointer or a slice with no default, so "declared" and
// "left alone" are different states rather than the same one. That is what
// [RepoConfig.Declares] reads, and it is what makes the merge onto a Project
// exact: a file that says nothing about replicas does not quietly set them to
// the CRD's default.
type RepoConfig struct {
	// Path the file was read from, relative to the repository root — the
	// project's build root plus kitchen.json, so "kitchen.json" for a
	// project at the top of its repository and "apps/web/kitchen.json" for
	// one in a monorepo.
	// +optional
	Path string `json:"path,omitempty"`

	// Build is what the file said about how the commit is built.
	// +optional
	Build *RepoBuildConfig `json:"build,omitempty"`

	// Runtime is what it said about how the result runs.
	// +optional
	Runtime *RepoRuntimeConfig `json:"runtime,omitempty"`

	// Env are the environment variables it declared, already in the
	// platform's own form. They carry literal values only: the file is
	// committed to a repository anybody who can open a pull request can
	// write to, so a value in it is public by construction and a reference
	// to a credential is refused rather than resolved.
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// Processes are the workers and scheduled jobs it declared. Unlike the
	// variables, which merge by name, this list replaces the project's
	// wholesale — a process the file does not name is a process the commit
	// does not have, and a worker that outlives the code that defined it is
	// the failure this replaces.
	// +optional
	// +listType=map
	// +listMapKey=name
	Processes []ProcessSpec `json:"processes,omitempty"`
}

// RepoBuildConfig is the build half of kitchen.json. It is deliberately not
// [ProjectBuildSpec]: that type carries RootDirectory, which is the one build
// setting the file may not touch.
type RepoBuildConfig struct {
	// Strategy the commit is built with: auto, dockerfile or buildpacks.
	// +optional
	Strategy BuildStrategy `json:"strategy,omitempty"`

	// DockerfilePath, relative to the build root — which it may not leave,
	// for the same reason the project's own setting may not: the build root
	// is the whole of what a build sees.
	// +optional
	DockerfilePath string `json:"dockerfilePath,omitempty"`
}

// RepoRuntimeConfig is the runtime half of kitchen.json: the subset of
// [RuntimeSpec] a repository may declare about itself, with every field
// optional and none of them defaulted.
//
// Resources are the API's two-string shape rather than Kubernetes'
// ResourceRequirements, because that is the shape the dashboard and the CLI
// already ask for and a repository file is the developer's surface, not the
// cluster's.
type RepoRuntimeConfig struct {
	// +optional
	Port *int32 `json:"port,omitempty"`

	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// +optional
	Singleton *bool `json:"singleton,omitempty"`

	// +optional
	NotRequestDriven *bool `json:"notRequestDriven,omitempty"`

	// Command, Args and PreviewArgs are the exec-form lists RuntimeSpec
	// takes. An empty list is the same as an absent key here, unlike on the
	// settings PATCH: a repository declares what the commit runs with, and
	// "declared, and empty" is not a thing a file needs to say.
	// +optional
	// +listType=atomic
	Command []string `json:"command,omitempty"`

	// +optional
	// +listType=atomic
	Args []string `json:"args,omitempty"`

	// +optional
	// +listType=atomic
	PreviewArgs []string `json:"previewArgs,omitempty"`

	// +optional
	Resources *RepoResources `json:"resources,omitempty"`

	// +optional
	Health *HealthSpec `json:"health,omitempty"`

	// Security is the posture the commit's workloads run under. A
	// repository is where it belongs as much as the settings screen is: an
	// image knows whether it can survive a read-only root filesystem, and
	// the commit that makes it able to is the commit that should say so.
	// +optional
	Security *SecuritySpec `json:"security,omitempty"`
}

// RepoResources is a workload's CPU and memory as two Kubernetes quantity
// strings, applied to requests and limits the way the API's own patch does.
type RepoResources struct {
	// +optional
	CPU string `json:"cpu,omitempty"`

	// +optional
	Memory string `json:"memory,omitempty"`
}

// Declares lists the settings this file declared, in the dotted form the API,
// the dashboard and the CLI all show them in — "build.strategy",
// "runtime.port", "env.DATABASE_HOST", "processes" — sorted, so two builds of
// the same file produce the same list.
//
// It is derived rather than stored. The alternative was a second field
// listing what the first field contains, which is two places to be wrong
// about one fact.
func (c *RepoConfig) Declares() []string {
	if c == nil {
		return nil
	}
	fields := make([]string, 0, 16)
	if b := c.Build; b != nil {
		if b.Strategy != "" {
			fields = append(fields, "build.strategy")
		}
		if b.DockerfilePath != "" {
			fields = append(fields, "build.dockerfilePath")
		}
	}
	if r := c.Runtime; r != nil {
		for name, declared := range map[string]bool{
			"runtime.port":             r.Port != nil,
			"runtime.replicas":         r.Replicas != nil,
			"runtime.singleton":        r.Singleton != nil,
			"runtime.notRequestDriven": r.NotRequestDriven != nil,
			"runtime.command":          len(r.Command) > 0,
			"runtime.args":             len(r.Args) > 0,
			"runtime.previewArgs":      len(r.PreviewArgs) > 0,
			"runtime.health":           r.Health != nil,
			"runtime.security":         r.Security != nil,
		} {
			if declared {
				fields = append(fields, name)
			}
		}
		if res := r.Resources; res != nil {
			if res.CPU != "" {
				fields = append(fields, "runtime.resources.cpu")
			}
			if res.Memory != "" {
				fields = append(fields, "runtime.resources.memory")
			}
		}
	}
	for _, variable := range c.Env {
		fields = append(fields, "env."+variable.Name)
	}
	if len(c.Processes) > 0 {
		fields = append(fields, "processes")
	}
	slices.Sort(fields)
	return fields
}

// DeclaresEnv reports whether the file declared the named environment
// variable, which is what the dashboard and `kitchen env list` read to mark a
// variable as the repository's rather than the project's.
func (c *RepoConfig) DeclaresEnv(name string) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(c.Env, func(v EnvVar) bool { return v.Name == name })
}

// String names the file and what it set, for a log line or an audit detail.
func (c *RepoConfig) String() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s declares %v", c.Path, c.Declares())
}
