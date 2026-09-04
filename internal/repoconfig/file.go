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
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/appconfig"
	"github.com/Bermos/Kitchen/internal/detect"
)

// File is kitchen.json as it is written, which is not quite how the platform
// stores it: variables are an object here and a list there, because an object
// is what a person writing one reaches for and a list is what merges by name.
// The conversion, and every refusal, is [File.config].
type File struct {
	// Schema is `$schema`, read only so that setting it is not an unknown
	// field. Editors use it; the platform does not.
	Schema string `json:"$schema,omitempty"`

	Build      *FileBuild          `json:"build,omitempty"`
	Runtime    *FileRuntime        `json:"runtime,omitempty"`
	Env        map[string]Value    `json:"env,omitempty"`
	PreviewEnv map[string]Value    `json:"previewEnv,omitempty"`
	Processes  []appconfig.Process `json:"processes,omitempty"`
	Files      []FileConfigFile    `json:"files,omitempty"`
}

// FileConfigFile is one entry of `files`: a configuration file the commit
// carries, mounted into the workloads it names.
//
// It is not [appconfig.File], and the difference is the point. That shape
// carries `secret`, and a repository may not declare a secret file for
// exactly the reason it may not point a variable at a credential: this file
// is committed, so what is in it is public, and a declaration that the
// platform holds a credential for this path is a claim about the project's
// standing rather than about the code. `content` is required here for the
// same reason it is optional there — a file has nowhere else to put it, and
// "keep whatever is stored" is not a thing a commit can mean.
type FileConfigFile struct {
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	Content   string   `json:"content"`
	Workloads []string `json:"workloads,omitempty"`

	// Secret is here only so that it can be refused by name, the way
	// build.rootDirectory is. It is the first thing somebody will reach for
	// after reading about secret files in the API docs, and "unknown field"
	// would be a true answer that explains nothing.
	Secret *bool `json:"secret,omitempty"`
}

// FileBuild is the `build` object.
type FileBuild struct {
	Strategy         string `json:"strategy,omitempty"`
	DockerfilePath   string `json:"dockerfilePath,omitempty"`
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`

	// RootDirectory is here only so that it can be refused by name. It is
	// the first thing somebody arriving from vercel.json will reach for, and
	// "unknown field" would be a true answer that explains nothing. See
	// [v1alpha1.RepoConfigFileName] for why the platform has to know where
	// the project is before it can read the file that would move it.
	RootDirectory *string `json:"rootDirectory,omitempty"`
}

// FileRuntime is the `runtime` object.
type FileRuntime struct {
	Port             *int32              `json:"port,omitempty"`
	Replicas         *int32              `json:"replicas,omitempty"`
	Singleton        *bool               `json:"singleton,omitempty"`
	NotRequestDriven *bool               `json:"notRequestDriven,omitempty"`
	Command          []string            `json:"command,omitempty"`
	Args             []string            `json:"args,omitempty"`
	PreviewArgs      []string            `json:"previewArgs,omitempty"`
	Resources        *FileResources      `json:"resources,omitempty"`
	Health           *appconfig.Health   `json:"health,omitempty"`
	Security         *appconfig.Security `json:"security,omitempty"`
}

// FileResources is `runtime.resources`.
type FileResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// Value is one environment variable's value in the file: a string, and
// nothing else.
//
// It is its own type for the sake of one error message. The natural thing to
// write for a variable that should come from a secret is
// `{"secretRef": {...}}`, copied from the CRD, and a plain map[string]string
// answers that with a type error about a Go type. What it needs to say is
// that a file in a repository is not where a credential goes — which is a
// rule, not a syntax error, and the reader is about to commit something that
// would have published one.
type Value string

// UnmarshalJSON accepts a JSON string, and explains anything else.
func (v *Value) UnmarshalJSON(raw []byte) error {
	var literal string
	if err := json.Unmarshal(raw, &literal); err == nil {
		*v = Value(literal)
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil {
		for _, credential := range []string{"secretRef", "fromResourceClaim"} {
			if _, asks := object[credential]; asks {
				return fmt.Errorf(
					"a variable in %s cannot take its value from %s: this file is committed, so everything in it is "+
						"public — set the variable in the dashboard or with `kitchen env set`, which is where a "+
						"reference to a credential belongs",
					FileName, credential)
			}
		}
	}
	return fmt.Errorf("every value in this object must be a string (got %s)", strings.TrimSpace(string(raw)))
}

// envNamePattern is the shape a POSIX environment variable's name has, which
// is also the shape a shell can export and a Kubernetes container accepts.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// config validates the whole file and turns it into what the Build records.
func (f File) config() (*kitchenv1alpha1.RepoConfig, error) {
	config := &kitchenv1alpha1.RepoConfig{}

	build, err := f.buildConfig()
	if err != nil {
		return nil, err
	}
	config.Build = build

	runtime, err := f.runtimeConfig()
	if err != nil {
		return nil, err
	}
	config.Runtime = runtime

	env, err := f.envConfig()
	if err != nil {
		return nil, err
	}
	config.Env = env

	processes, err := appconfig.Processes(f.Processes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	config.Processes = processes

	files, err := f.filesConfig(processes)
	if err != nil {
		return nil, err
	}
	config.Files = files

	return config, nil
}

// filesConfig validates the `files` list. The workload names it may mention
// are the ones this same file declares plus the web process: a file read at
// build time cannot see the project it is about to be merged onto, and a
// commit that names a workload only the project knows about would pass here
// and be refused by the merge, which is a worse place to find out.
func (f File) filesConfig(processes []kitchenv1alpha1.ProcessSpec) ([]kitchenv1alpha1.ConfigFile, error) {
	if len(f.Files) == 0 {
		return nil, nil
	}
	requests := make([]appconfig.File, 0, len(f.Files))
	for _, declared := range f.Files {
		if declared.Secret != nil {
			return nil, fmt.Errorf(
				"%w: files[%q] sets secret — a file in %s is committed, so everything in it is public, and the "+
					"platform holding a credential for a project is not something a commit gets to declare. "+
					"Declare the secret file in the dashboard or with `kitchen files set --secret`, and leave it "+
					"out of this file",
				ErrInvalid, declared.Name, FileName)
		}
		if declared.Content == "" {
			return nil, fmt.Errorf(
				"%w: files[%q] has no content — a file declared in %s carries what is in it, since there is "+
					"nowhere else for a committed declaration to have put it",
				ErrInvalid, declared.Name, FileName)
		}
		content := declared.Content
		requests = append(requests, appconfig.File{
			Name:      declared.Name,
			Path:      declared.Path,
			Content:   &content,
			Workloads: declared.Workloads,
		})
	}
	names := make([]string, 0, len(processes))
	for _, process := range processes {
		names = append(names, process.Name)
	}
	files, err := appconfig.Files(requests, nil, names)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return files, nil
}

func (f File) buildConfig() (*kitchenv1alpha1.RepoBuildConfig, error) {
	if f.Build == nil {
		return nil, nil
	}
	if f.Build.RootDirectory != nil {
		return nil, fmt.Errorf(
			"%w: build.rootDirectory cannot be set here — it is how the platform found this file, so a file that "+
				"moved it would have to be read before it could say where to read it. Set the root directory on the "+
				"project, in the dashboard or with `kitchen api PATCH /projects/<name>`", ErrInvalid)
	}

	build := &kitchenv1alpha1.RepoBuildConfig{}
	if strategy := strings.TrimSpace(f.Build.Strategy); strategy != "" {
		switch kitchenv1alpha1.BuildStrategy(strategy) {
		case kitchenv1alpha1.BuildStrategyAuto,
			kitchenv1alpha1.BuildStrategyDockerfile,
			kitchenv1alpha1.BuildStrategyBuildpacks:
			build.Strategy = kitchenv1alpha1.BuildStrategy(strategy)
		default:
			return nil, fmt.Errorf("%w: build.strategy must be auto, dockerfile or buildpacks (got %q)",
				ErrInvalid, f.Build.Strategy)
		}
	}
	if dockerfile := strings.TrimSpace(f.Build.DockerfilePath); dockerfile != "" {
		if err := validateRepoPath("build.dockerfilePath", dockerfile); err != nil {
			return nil, err
		}
		build.DockerfilePath = dockerfile
	}
	if target := detect.NormalizeTarget(f.Build.DockerfileTarget); target != "" {
		// Refused here rather than at the build, because the file is read
		// before anything is created and a name no stage could have will
		// never match one: BuildKit would fail the build several minutes
		// later with its own sentence about a target it could not find.
		if !detect.ValidTarget(target) {
			return nil, fmt.Errorf("%w: build.dockerfileTarget must name a stage of the Dockerfile — %s (got %q)",
				ErrInvalid, detect.StageNameRule, f.Build.DockerfileTarget)
		}
		build.DockerfileTarget = target
	}
	if *build == (kitchenv1alpha1.RepoBuildConfig{}) {
		return nil, nil
	}
	return build, nil
}

// validateRepoPath refuses a path that leaves the build root. The build pod
// is handed it as-is, so a `../` here is the file reaching for a directory
// the project was not pointed at.
func validateRepoPath(field, value string) error {
	if strings.HasPrefix(value, "/") {
		return fmt.Errorf("%w: %s must be relative to the project's root directory (got %q)", ErrInvalid, field, value)
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return fmt.Errorf("%w: %s cannot leave the project's root directory (got %q)", ErrInvalid, field, value)
		}
	}
	return nil
}

func (f File) runtimeConfig() (*kitchenv1alpha1.RepoRuntimeConfig, error) {
	if f.Runtime == nil {
		return nil, nil
	}
	source := f.Runtime
	runtime := &kitchenv1alpha1.RepoRuntimeConfig{
		Replicas:         source.Replicas,
		Singleton:        source.Singleton,
		NotRequestDriven: source.NotRequestDriven,
		Command:          source.Command,
		Args:             source.Args,
		PreviewArgs:      source.PreviewArgs,
	}

	if source.Port != nil {
		// Unlike the settings PATCH, zero is not accepted: there it means
		// "hand the question back to the platform", which is what leaving
		// the key out of a file already means.
		if *source.Port < 1 || *source.Port > 65535 {
			return nil, fmt.Errorf(
				"%w: runtime.port must be between 1 and 65535 — leave it out to take the detected framework's (got %d)",
				ErrInvalid, *source.Port)
		}
		runtime.Port = source.Port
	}
	if source.Replicas != nil && *source.Replicas < 1 {
		return nil, fmt.Errorf("%w: runtime.replicas must be at least 1 (got %d) — production never scales to zero",
			ErrInvalid, *source.Replicas)
	}
	if source.Singleton != nil && *source.Singleton && source.Replicas != nil && *source.Replicas > 1 {
		return nil, fmt.Errorf(
			"%w: runtime.singleton says two of this workload must never run at once, so it cannot ask for %d replicas",
			ErrInvalid, *source.Replicas)
	}
	if source.Resources != nil {
		resources := &kitchenv1alpha1.RepoResources{
			CPU:    strings.TrimSpace(source.Resources.CPU),
			Memory: strings.TrimSpace(source.Resources.Memory),
		}
		// Parsed here so the refusal names the file, rather than surfacing
		// as an admission error when the Deployment is written.
		probe := corev1.ResourceRequirements{}
		for field, value := range map[corev1.ResourceName]string{
			corev1.ResourceCPU:    resources.CPU,
			corev1.ResourceMemory: resources.Memory,
		} {
			if value == "" {
				continue
			}
			if err := appconfig.ApplyResource(&probe, field, value); err != nil {
				return nil, fmt.Errorf("%w: runtime.resources.%w", ErrInvalid, err)
			}
		}
		if *resources != (kitchenv1alpha1.RepoResources{}) {
			runtime.Resources = resources
		}
	}
	if source.Health != nil {
		health, err := appconfig.HealthSpec(*source.Health, "runtime.health", false)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		runtime.Health = health
	}
	if source.Security != nil {
		security, err := appconfig.SecuritySpec(*source.Security, "runtime.security")
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		runtime.Security = security
	}
	return runtime, nil
}

// envConfig turns the two variable objects into the platform's list, in name
// order so that two builds of one file record the same thing.
func (f File) envConfig() ([]kitchenv1alpha1.EnvVar, error) {
	if len(f.Env) == 0 && len(f.PreviewEnv) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(f.Env))
	for name := range f.Env {
		names = append(names, name)
	}
	slices.Sort(names)

	for name := range f.PreviewEnv {
		if _, declared := f.Env[name]; !declared {
			return nil, fmt.Errorf(
				"%w: previewEnv sets %s, which env does not declare — a preview value replaces a value, so the "+
					"variable has to have one",
				ErrInvalid, name)
		}
	}

	variables := make([]kitchenv1alpha1.EnvVar, 0, len(names))
	for _, name := range names {
		if !envNamePattern.MatchString(name) {
			return nil, fmt.Errorf(
				"%w: %q is not a usable environment variable name — letters, digits and underscores, not starting with a digit",
				ErrInvalid, name)
		}
		variables = append(variables, kitchenv1alpha1.EnvVar{
			Name:         name,
			Value:        string(f.Env[name]),
			PreviewValue: string(f.PreviewEnv[name]),
		})
	}
	return variables, nil
}
