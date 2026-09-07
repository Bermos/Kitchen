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

// DockerfileTarget is the stage of the Dockerfile this commit ships: the
// file's, or the project's.
//
// It travels with the commit for the reason the Dockerfile path does — which
// stages a file has is a fact about the file, so a rebuild of an old commit
// builds the stage that commit named rather than the one the project names
// today.
func DockerfileTarget(config *kitchenv1alpha1.RepoConfig, projectTarget string) string {
	if config == nil || config.Build == nil || config.Build.DockerfileTarget == "" {
		return projectTarget
	}
	return config.Build.DockerfileTarget
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
	if declared.Security != nil {
		// The project's posture is the ceiling, so this is a field-by-field
		// merge that keeps only what tightens rather than the whole-block
		// override every other setting here gets (#431). A file that
		// weakened one was two things at once: an override that dropped
		// every constraint it did not repeat, since a posture is a value and
		// not a set of keys, and `allowPrivilegeEscalation`, which puts back
		// the one default the platform tightens on every container — and
		// since #422 the author of a pull request's kitchen.json need not be
		// anybody with access to the project.
		//
		// What it would not take is dropped rather than fatal, and
		// [IgnoredSecurity] is what the build reports it with.
		merged.Security, _ = merged.Security.TightenedBy(declared.Security)
	}
	if len(declared.Init) > 0 {
		// Deep, not a slice clone: each entry carries its own step lists, and
		// a shallow copy would leave the merged runtime sharing them with the
		// RepoConfig the build read.
		merged.Init = make([]kitchenv1alpha1.VolumeInit, len(declared.Init))
		for i := range declared.Init {
			declared.Init[i].DeepCopyInto(&merged.Init[i])
		}
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
//
// `ceiling` is the project's own `runtime.security`, and each declared
// workload's posture is held to it exactly as the unit's is (#431). A ceiling
// only the unit's block answered to would not be one: `processes[].security`
// is written over the unit's per workload, so a worker declared in the file
// could ask for the escalation the file may not ask for one line higher up.
func Processes(
	base []kitchenv1alpha1.ProcessSpec,
	ceiling *kitchenv1alpha1.SecuritySpec,
	config *kitchenv1alpha1.RepoConfig,
) []kitchenv1alpha1.ProcessSpec {
	if config == nil || len(config.Processes) == 0 {
		return base
	}
	processes := make([]kitchenv1alpha1.ProcessSpec, len(config.Processes))
	for i, process := range config.Processes {
		processes[i] = *process.DeepCopy()
		if processes[i].Security != nil {
			processes[i].Security, _ = ceiling.TighteningOnly(processes[i].Security)
		}
	}
	return processes
}

// Files merges the file's configuration files onto the project's, by name.
//
// It merges rather than replaces, which is what the variables do and not what
// the processes do — and for the variables' reason rather than for symmetry.
// A project may hold a **secret** file, whose content the platform keeps
// where nothing reads it back and which a committed file is not allowed to
// declare. A list that replaced would take that declaration away on the next
// build, and the failure would be an application starting without the
// credential file it is configured by.
//
// A name the file declares that the project holds as a secret is refused
// outright rather than resolved either way, exactly as a variable bound to a
// credential is: letting the file win would replace a credential with a
// public value out of a pull request, and letting the project win would leave
// content in the repository that reads as though it applies and does not.
func Files(
	base []kitchenv1alpha1.ConfigFile,
	config *kitchenv1alpha1.RepoConfig,
) ([]kitchenv1alpha1.ConfigFile, error) {
	if config == nil || len(config.Files) == 0 {
		return base, nil
	}

	merged := make([]kitchenv1alpha1.ConfigFile, len(base))
	for i, file := range base {
		merged[i] = *file.DeepCopy()
	}

	for _, declared := range config.Files {
		at := slices.IndexFunc(merged, func(f kitchenv1alpha1.ConfigFile) bool { return f.Name == declared.Name })
		if at < 0 {
			merged = append(merged, *declared.DeepCopy())
			continue
		}
		if merged[at].Secret {
			return base, fmt.Errorf(
				"%w: it declares the file %s, which this project holds as a secret — content in a committed file "+
					"cannot stand in for a credential. Rename the file in %s, or take the secret file off the project",
				ErrInvalid, declared.Name, FileName)
		}
		merged[at] = *declared.DeepCopy()
	}
	return merged, nil
}

// Snapshot is the whole of what a Release freezes, with the file applied: the
// four lists an Environment reads, resolved once at the end of the build
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
	files, err := Files(base.Files, config)
	if err != nil {
		return base, err
	}
	return kitchenv1alpha1.ConfigSnapshot{
		Env:       env,
		Runtime:   runtime,
		Processes: Processes(base.Processes, base.Runtime.Security, config),
		Files:     files,
	}, nil
}

// IgnoredSecurity names the `runtime.security` fields of this commit's own
// kitchen.json that the project's posture does not allow it to change — the
// same answer [Runtime] acts on, asked separately so that a build can say so.
//
// It is the second half of ignoring rather than refusing: a field silently
// dropped is a file that reads as though it applies and does not, which is
// the failure mode kitchen.json exists to avoid. The build carries a warning
// naming these; nothing fails.
func IgnoredSecurity(
	base kitchenv1alpha1.RuntimeSpec,
	config *kitchenv1alpha1.RepoConfig,
) []string {
	if config == nil || config.Runtime == nil || config.Runtime.Security == nil {
		return nil
	}
	_, ignored := base.Security.TightenedBy(config.Runtime.Security)
	return ignored
}

// IgnoredProcessSecurity is the same answer for each workload the file
// declares: the process's name against the `security` fields the project's
// posture does not allow it to change, for the workloads that asked for one.
//
// It is separate from [IgnoredSecurity] rather than one flat list because the
// two are different sentences to whoever has to fix it — "the file's
// runtime.security" and "the file's worker" are two different lines to go and
// look at.
func IgnoredProcessSecurity(
	base kitchenv1alpha1.RuntimeSpec,
	config *kitchenv1alpha1.RepoConfig,
) map[string][]string {
	if config == nil || len(config.Processes) == 0 {
		return nil
	}
	var ignored map[string][]string
	for i := range config.Processes {
		process := &config.Processes[i]
		if process.Security == nil {
			continue
		}
		_, refused := base.Security.TighteningOnly(process.Security)
		if len(refused) == 0 {
			continue
		}
		if ignored == nil {
			ignored = map[string][]string{}
		}
		ignored[process.Name] = refused
	}
	return ignored
}
