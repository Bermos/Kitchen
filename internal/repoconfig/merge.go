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

package repoconfig

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/appconfig"
)

// The file wins over the project for every setting it names, and touches
// nothing else.
//
// That is the rule, and it is the one a reader of a repository expects: a
// value written in a file that is read on every build is a value that takes
// effect, or the file is decoration. The dashboard keeps the settings the
// file says nothing about, and shows the ones it does as the repository's —
// [v1alpha1.RepoConfig.Declares] is what it reads to know which those are.
//
// The merged result is frozen into the Release, so a rollback replays the
// configuration its commit declared rather than today's.

// Strategy is the build strategy for this commit: the file's, or the one the
// project and the platform had already settled on.
func Strategy(config *kitchenv1alpha1.RepoConfig, resolved kitchenv1alpha1.BuildStrategy) kitchenv1alpha1.BuildStrategy {
	if config == nil || config.Build == nil || config.Build.Strategy == "" {
		return resolved
	}
	return config.Build.Strategy
}

// DockerfilePath is the Dockerfile for this commit, relative to the build
// root: the file's, or the project's.
func DockerfilePath(config *kitchenv1alpha1.RepoConfig, projectPath string) string {
	if config == nil || config.Build == nil || config.Build.DockerfilePath == "" {
		return projectPath
	}
	return config.Build.DockerfilePath
}

// Runtime overlays the file's runtime declarations onto the runtime the
// project and the detected framework produced.
//
// The singleton rule is checked again on the result, because either half can
// come from either side: a project that runs three replicas and a file that
// newly declares the workload a singleton is a contradiction neither one
// could see on its own.
func Runtime(
	base kitchenv1alpha1.RuntimeSpec,
	config *kitchenv1alpha1.RepoConfig,
) (kitchenv1alpha1.RuntimeSpec, error) {
	if config == nil || config.Runtime == nil {
		return base, nil
	}
	declared := config.Runtime
	merged := *base.DeepCopy()

	if declared.Port != nil {
		merged.Port = *declared.Port
	}
	if declared.Replicas != nil {
		merged.Replicas = declared.Replicas
	}
	if declared.Singleton != nil {
		merged.Singleton = *declared.Singleton
	}
	if declared.NotRequestDriven != nil {
		merged.NotRequestDriven = *declared.NotRequestDriven
	}
	if len(declared.Command) > 0 {
		merged.Command = slices.Clone(declared.Command)
	}
	if len(declared.Args) > 0 {
		merged.Args = slices.Clone(declared.Args)
	}
	if len(declared.PreviewArgs) > 0 {
		merged.PreviewArgs = slices.Clone(declared.PreviewArgs)
	}
	if declared.Health != nil {
		merged.Health = declared.Health.DeepCopy()
	}
	if resources := declared.Resources; resources != nil {
		for name, value := range map[corev1.ResourceName]string{
			corev1.ResourceCPU:    resources.CPU,
			corev1.ResourceMemory: resources.Memory,
		} {
			if value == "" {
				continue
			}
			if err := appconfig.ApplyResource(&merged.Resources, name, value); err != nil {
				return base, fmt.Errorf("%w: runtime.resources.%w", ErrInvalid, err)
			}
		}
	}

	if merged.Singleton && merged.Replicas != nil && *merged.Replicas > 1 {
		return base, fmt.Errorf(
			"%w: it declares this workload a singleton, and the project runs %d replicas — "+
				"set runtime.replicas to 1 in the file, or turn singleton off",
			ErrInvalid, *merged.Replicas)
	}
	return merged, nil
}

// Env merges the file's variables onto the project's, by name.
//
// It merges rather than replaces, which is the opposite of what the file does
// to processes, and for a reason that is not symmetry: a project's variables
// are how it reaches its database, its object store and whatever else the
// platform provisioned for it, and those arrive as references to a credential
// the file is not allowed to write. A file that replaced the list would
// unbind them, and the failure would be a running application that cannot
// reach anything.
//
// A name the file declares that the project binds to a credential is refused
// outright rather than resolved either way. Letting the file win would let a
// pull request repoint a database URL at a host it chose; letting the project
// win would leave a value in the repository that reads as though it applies
// and does not.
func Env(
	base []kitchenv1alpha1.EnvVar,
	config *kitchenv1alpha1.RepoConfig,
) ([]kitchenv1alpha1.EnvVar, error) {
	if config == nil || len(config.Env) == 0 {
		return base, nil
	}

	merged := make([]kitchenv1alpha1.EnvVar, len(base))
	for i, variable := range base {
		merged[i] = *variable.DeepCopy()
	}

	for _, declared := range config.Env {
		at := slices.IndexFunc(merged, func(v kitchenv1alpha1.EnvVar) bool { return v.Name == declared.Name })
		if at < 0 {
			merged = append(merged, *declared.DeepCopy())
			continue
		}
		if existing := merged[at]; existing.SecretRef != nil || existing.FromResourceClaim != nil {
			return base, fmt.Errorf(
				"%w: it declares %s, which this project already takes from %s — a value in a committed file cannot "+
					"stand in for a credential. Rename the variable in the file, or take the binding off the project",
				ErrInvalid, declared.Name, credentialSource(existing))
		}
		merged[at].Value = declared.Value
		merged[at].PreviewValue = declared.PreviewValue
	}
	return merged, nil
}

// credentialSource names where a bound variable gets its value, for the
// refusal above.
func credentialSource(variable kitchenv1alpha1.EnvVar) string {
	switch {
	case variable.SecretRef != nil:
		return fmt.Sprintf("the secret %q", variable.SecretRef.Name)
	case variable.FromResourceClaim != nil:
		return fmt.Sprintf("the resource claim %q", variable.FromResourceClaim.Name)
	default:
		return "a credential"
	}
}

// Processes is the file's process list where it declared one, and the
// project's otherwise.
//
// It replaces rather than merges, unlike the variables. A worker is defined by
// the code it runs, and a worker the commit no longer declares is one whose
// command may no longer exist in the image — merging would keep it running
// until somebody noticed.
func Processes(
	base []kitchenv1alpha1.ProcessSpec,
	config *kitchenv1alpha1.RepoConfig,
) []kitchenv1alpha1.ProcessSpec {
	if config == nil || len(config.Processes) == 0 {
		return base
	}
	processes := make([]kitchenv1alpha1.ProcessSpec, len(config.Processes))
	for i, process := range config.Processes {
		processes[i] = *process.DeepCopy()
	}
	return processes
}

// Snapshot is the whole of what a Release freezes, with the file applied: the
// three lists an Environment reads, resolved once at the end of the build
// that produced them.
func Snapshot(
	base kitchenv1alpha1.ConfigSnapshot,
	config *kitchenv1alpha1.RepoConfig,
) (kitchenv1alpha1.ConfigSnapshot, error) {
	if config == nil {
		return base, nil
	}
	env, err := Env(base.Env, config)
	if err != nil {
		return base, err
	}
	runtime, err := Runtime(base.Runtime, config)
	if err != nil {
		return base, err
	}
	return kitchenv1alpha1.ConfigSnapshot{
		Env:       env,
		Runtime:   runtime,
		Processes: Processes(base.Processes, config),
	}, nil
}
