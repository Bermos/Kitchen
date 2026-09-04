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

	// Files are the configuration files it declared. Like the variables and
	// unlike the processes they merge by name onto the project's, because a
	// project may hold a *secret* file the repository is not allowed to
	// write: a list that replaced would take the declaration away and leave
	// the application without the credential file it starts on.
	//
	// They carry content and never secrecy, for the reason the variables
	// carry literals and never a reference: the file is committed to a
	// repository anybody who can open a pull request may write, so a
	// declaration in it is public by construction.
	// +optional
	// +listType=map
	// +listMapKey=name
	Files []ConfigFile `json:"files,omitempty"`

	// Volumes are the persistent volumes the commit says it needs: which
	// claim, mounted where, by which process.
	//
	// **They are a requirement, not a request.** Everything else in this
	// file describes the code and the file may set it; a volume claim is
	// the project asking the platform for storage — for a bound volume,
	// for storage the platform did not create and does not own — and that
	// is the project's standing rather than a fact about the commit. A file
	// that could make one would let a pull request mount somebody's NAS
	// export into its own preview, which is the "no credential, ever" rule
	// wearing a different hat.
	//
	// So the file declares what the code needs and the build checks it: a
	// declaration the project has no claim for fails the build naming the
	// claim to make, and one whose process or mount path disagrees with the
	// claim fails naming both. That catches the failure this is actually
	// for — the code writes to /data and the claim mounts /var/data, which
	// otherwise deploys green and loses everything on restart.
	// +optional
	// +listType=map
	// +listMapKey=name
	Volumes []RepoVolume `json:"volumes,omitempty"`
}

// RepoVolume is one entry of kitchen.json's `volumes`: a volume claim the
// commit needs, as the commit understands it.
type RepoVolume struct {
	// Name is the resource claim's name, which is what ties the
	// declaration to the thing the project actually has.
	Name string `json:"name"`

	// Process is the project's process that mounts it, and MountPath where
	// it appears in that process's container — the two facts about a volume
	// that really are about the code, and the two worth checking a claim
	// against.
	Process   string `json:"process"`
	MountPath string `json:"mountPath"`

	// Source is provision or bind, where the file wants to say which. Empty
	// declares no opinion and matches either; a value that disagrees with
	// the claim fails the build, because a commit written against twelve
	// terabytes of existing media and a claim that cut a fresh empty disk
	// are not the same application.
	// +kubebuilder:validation:Enum=provision;bind
	// +optional
	Source VolumeSource `json:"source,omitempty"`

	// AccessMode is how the commit expects to mount it — the same values a
	// bound claim declares. Empty declares no opinion; a value that
	// disagrees with the claim fails the build, since read-only and
	// read-write are the difference between an application that works and
	// one that fails on its first write.
	// +kubebuilder:validation:Enum=ReadOnlyMany;ReadWriteOnce;ReadWriteMany
	// +optional
	AccessMode string `json:"accessMode,omitempty"`
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

	// DockerfileTarget is the stage of a multi-stage Dockerfile to ship,
	// declared by the commit rather than by the project — which is where it
	// belongs, for the same reason DockerfilePath does: the stage a file has
	// is a fact about the file, so a rebuild of an old commit builds the
	// stage that commit asked for rather than the one the project names
	// today.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_.-]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`
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
		if b.DockerfileTarget != "" {
			fields = append(fields, "build.dockerfileTarget")
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
	// A file is named one by one, the way a variable is and unlike the
	// process list: the two merge by name, so "the file declared this one"
	// is the fact a screen needs beside each row.
	for _, file := range c.Files {
		fields = append(fields, "files."+file.Name)
	}
	// A volume is named one by one too: each is a requirement checked
	// against one claim, so "the file declared this one" is the fact beside
	// the row rather than a count.
	for _, vol := range c.Volumes {
		fields = append(fields, "volumes."+vol.Name)
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

// DeclaresFile reports whether the file declared the named configuration
// file, which is what the dashboard and `kitchen files list` read to mark one
// as the repository's rather than the project's.
func (c *RepoConfig) DeclaresFile(name string) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(c.Files, func(f ConfigFile) bool { return f.Name == name })
}

// DeclaresVolume reports whether the file declared the named volume claim,
// which is what the dashboard reads to mark a claim as one the repository
// says it needs rather than one somebody added by hand.
func (c *RepoConfig) DeclaresVolume(name string) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(c.Volumes, func(v RepoVolume) bool { return v.Name == name })
}

// String names the file and what it set, for a log line or an audit detail.
func (c *RepoConfig) String() string {
	if c == nil {
		return ""
	}
	return fmt.Sprintf("%s declares %v", c.Path, c.Declares())
}
