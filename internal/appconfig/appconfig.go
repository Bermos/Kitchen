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

// Package appconfig holds the shapes an application's build and runtime
// settings arrive in, and the rules they have to satisfy.
//
// It exists because there are now two ways to say the same thing. The REST
// API takes a settings PATCH; a repository takes a kitchen.json
// (internal/repoconfig). They describe one workload, so they have to agree on
// what a process is, what a health check is and what a quantity looks like —
// and the only way to be sure two validators agree is for there to be one.
//
// Everything here is about the *request*: the wire shape a client sends and
// the sentence it gets back when the shape is wrong. What the platform then
// does with the result is the caller's, which is why this package touches no
// cluster, writes no response and knows nothing about authorization.
package appconfig

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
)

// MaxProjectNameLength caps a project's name so that every name the platform
// derives from it still fits Kubernetes' 63-character limit.
const MaxProjectNameLength = 46

// ValidateProjectName checks a name the platform will derive other names
// from — a project's, and a process's, which becomes part of a workload's.
func ValidateProjectName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > MaxProjectNameLength {
		return fmt.Errorf(
			"name must be at most %d characters: the names the platform derives from it (releases, namespaces, hostnames) have to fit Kubernetes' 63-character limit",
			MaxProjectNameLength)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("name must work as a DNS label — lowercase letters, digits and '-', starting and ending alphanumeric (got %q)", name)
	}
	return nil
}

// Health is a health check as a client sends one. Sending `{}` is how a
// declared check is taken back off: an empty check is exactly the default — a
// TCP connect to the container's port on the platform's timings.
type Health struct {
	// Path is the HTTP path the probe asks for. Empty makes it a TCP
	// connect, which is deliberately not `GET /`.
	Path string `json:"path,omitempty"`
	// Port the probe is made against; empty takes the container's own. A
	// process has none, so a process's check must name one.
	Port int32 `json:"port,omitempty"`
	// The timings. Zero takes the platform's default for each.
	PeriodSeconds           int32 `json:"periodSeconds,omitempty"`
	TimeoutSeconds          int32 `json:"timeoutSeconds,omitempty"`
	FailureThreshold        int32 `json:"failureThreshold,omitempty"`
	StartupFailureThreshold int32 `json:"startupFailureThreshold,omitempty"`
}

// HealthSpec validates one health check. subject names what it belongs to in
// a refusal — "the health check", or a process by name — and needsPort is set
// for a workload that publishes no port of its own.
func HealthSpec(request Health, subject string, needsPort bool) (*kitchenv1alpha1.HealthSpec, error) {
	health := &kitchenv1alpha1.HealthSpec{
		Path:                    strings.TrimSpace(request.Path),
		Port:                    request.Port,
		PeriodSeconds:           request.PeriodSeconds,
		TimeoutSeconds:          request.TimeoutSeconds,
		FailureThreshold:        request.FailureThreshold,
		StartupFailureThreshold: request.StartupFailureThreshold,
	}
	if health.Path != "" && !strings.HasPrefix(health.Path, "/") {
		return nil, fmt.Errorf("%s: path must start with / (got %q)", subject, request.Path)
	}
	if health.Port < 0 || health.Port > 65535 {
		return nil, fmt.Errorf(
			"%s: port must be between 1 and 65535, or 0 to probe the port the application is published on (got %d)",
			subject, health.Port)
	}
	if needsPort && health.Port == 0 {
		return nil, fmt.Errorf(
			"%s: name the port the check is made against — a process publishes no port of its own", subject)
	}
	for _, timing := range []struct {
		name  string
		value int32
	}{
		{"periodSeconds", health.PeriodSeconds},
		{"timeoutSeconds", health.TimeoutSeconds},
		{"failureThreshold", health.FailureThreshold},
		{"startupFailureThreshold", health.StartupFailureThreshold},
	} {
		if timing.value < 0 {
			return nil, fmt.Errorf("%s: %s cannot be negative, and 0 takes the platform's default (got %d)",
				subject, timing.name, timing.value)
		}
	}
	return health, nil
}

// Security is the security posture as a client sends one. Sending `{}` is how
// a declared posture is taken back off: an empty posture is exactly the
// platform's default, which every workload gets either way.
//
// Every field is zero-means-the-default, the reading the health check's
// timings already have, so nothing here needs a pointer to tell an absent key
// from a cleared one.
type Security struct {
	// RunAsNonRoot refuses to start a container whose image would run as
	// uid 0.
	RunAsNonRoot bool `json:"runAsNonRoot,omitempty"`
	// RunAsUser and RunAsGroup override the image's own ids. Zero is the
	// image's user left alone, not a request to run as root.
	RunAsUser  int64 `json:"runAsUser,omitempty"`
	RunAsGroup int64 `json:"runAsGroup,omitempty"`
	// ReadOnlyRootFilesystem mounts the container's own filesystem read
	// only.
	ReadOnlyRootFilesystem bool `json:"readOnlyRootFilesystem,omitempty"`
	// AllowPrivilegeEscalation puts back the one thing the platform tightens
	// by default, for an image that needs a setuid binary.
	AllowPrivilegeEscalation bool `json:"allowPrivilegeEscalation,omitempty"`
	// DropCapabilities are the Linux capabilities taken away, in the
	// kernel's spelling without the CAP_ prefix — or the single entry ALL.
	DropCapabilities []string `json:"dropCapabilities,omitempty"`
}

// capabilityName is the shape of a Linux capability without its CAP_ prefix,
// which is how corev1 spells one and so how this takes one.
var capabilityName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// SecuritySpec validates one posture. subject names what it belongs to in a
// refusal, and it answers nil for a posture that declares nothing — an empty
// block and no block are the same posture, so a project that clears the form
// stores no field rather than an object full of zeroes.
func SecuritySpec(request Security, subject string) (*kitchenv1alpha1.SecuritySpec, error) {
	security := &kitchenv1alpha1.SecuritySpec{
		RunAsNonRoot:             request.RunAsNonRoot,
		RunAsUser:                request.RunAsUser,
		RunAsGroup:               request.RunAsGroup,
		ReadOnlyRootFilesystem:   request.ReadOnlyRootFilesystem,
		AllowPrivilegeEscalation: request.AllowPrivilegeEscalation,
	}
	for _, id := range []struct {
		name  string
		value int64
	}{{"runAsUser", security.RunAsUser}, {"runAsGroup", security.RunAsGroup}} {
		if id.value < 0 {
			return nil, fmt.Errorf(
				"%s: %s cannot be negative, and 0 leaves the image's own user alone (got %d)",
				subject, id.name, id.value)
		}
	}
	for _, capability := range request.DropCapabilities {
		dropped := strings.ToUpper(strings.TrimSpace(capability))
		if !capabilityName.MatchString(dropped) {
			return nil, fmt.Errorf(
				"%s: %q is not a Linux capability — write them the way the kernel does without the CAP_ prefix "+
					"(NET_RAW, SYS_ADMIN), or ALL for every one of them",
				subject, capability)
		}
		if dropped == kitchenv1alpha1.CapabilityDropAll && len(request.DropCapabilities) > 1 {
			return nil, fmt.Errorf(
				"%s: dropping ALL already drops every capability, so it cannot be listed beside another one",
				subject)
		}
		security.DropCapabilities = append(security.DropCapabilities, dropped)
	}
	// A posture that asks for nothing is no posture. The platform's default
	// is what an absent block already means, so storing an object of zeroes
	// would be a declaration that reads back as one and says nothing.
	if len(security.Declared()) == 0 {
		return nil, nil
	}
	return security, nil
}

// Process is one entry of the list a settings PATCH — or a kitchen.json —
// replaces.
//
// Resources are the two strings the runtime already takes — `cpu` and
// `memory`, applied as request and limit alike — rather than the full
// Kubernetes ResourceRequirements, because the project's own runtime is
// written that way and a process asking for its capacity in a second
// vocabulary would be the incoherence, not the saving.
type Process struct {
	Name string `json:"name"`
	// Type is worker, cron, service or task. The first three keep running or
	// fire again; a task runs once per deploy and the deploy waits for it,
	// which is where a schema migration goes.
	Type    string   `json:"type"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Port is a service workload's listening port, required on one and
	// refused on anything else: only a service is addressed.
	Port int32 `json:"port,omitempty"`
	// Build is this workload's own build, for a monorepo shipping several
	// images from one commit. Absent means it runs the project's image.
	Build *ProcessBuild `json:"build,omitempty"`
	// Replicas is a worker's copy count. Zero is allowed and means a worker
	// that is declared and parked, which is how one is turned off without
	// losing its command.
	Replicas *int32 `json:"replicas,omitempty"`
	// Singleton says two of this worker must never run at once, so a deploy
	// stops the old copy before starting the new one. It refuses more than
	// one replica rather than clamping the count, and it is refused on a
	// scheduled process: whether two of *its* runs may overlap is
	// ConcurrencyPolicy.
	Singleton bool   `json:"singleton,omitempty"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	// Schedule is a cron process's five-field expression, read in UTC.
	Schedule string `json:"schedule,omitempty"`
	// ConcurrencyPolicy is Allow, Forbid or Replace; empty means Forbid.
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`
	// Timeout is a Go duration ("30m") bounding one run — a scheduled one, or
	// a task's, which is how long a deploy waits for it. Empty means an hour.
	Timeout string `json:"timeout,omitempty"`
	// Previews opts this workload into preview environments, or out of them.
	// Absent takes the default for its type — off for a worker and a
	// scheduled job, on for a service and a task; see ProcessSpec.Previews
	// for why the default turns on the type and why this is a pointer.
	Previews *bool `json:"previews,omitempty"`
	// Health is a worker's or a service's health check. A worker's has to
	// name the port it is made against, because a worker publishes none of
	// its own; a service's falls back to its declared port. It is refused on
	// a scheduled process and on a task, whose verdict is the run's exit
	// status.
	Health *Health `json:"health,omitempty"`
}

// ProcessBuild is one workload's own build as a client sends it.
//
// There is no `auto` here and no root directory relative to the project's:
// see [v1alpha1.ProcessBuildSpec] for both, which are decisions rather than
// omissions.
type ProcessBuild struct {
	// Strategy is dockerfile or buildpacks; empty means dockerfile.
	Strategy string `json:"strategy,omitempty"`
	// DockerfilePath is relative to RootDirectory; empty means Dockerfile.
	DockerfilePath string `json:"dockerfilePath,omitempty"`
	// DockerfileTarget is the stage of that Dockerfile to ship; empty means
	// the project's own answer, and the file's last stage where the project
	// names none either.
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`
	// RootDirectory is relative to the repository root; empty means the
	// repository itself.
	RootDirectory string `json:"rootDirectory,omitempty"`
}

// Processes validates a whole process list and turns it into the spec. It
// replaces rather than merges, like the promotion stages and unlike the
// environment variables: the list is short, ordered by nothing, and a merge
// would leave no way to delete an entry.
func Processes(requests []Process) ([]kitchenv1alpha1.ProcessSpec, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	processes := make([]kitchenv1alpha1.ProcessSpec, 0, len(requests))
	seen := map[string]bool{}
	for _, request := range requests {
		process, err := ProcessSpec(request)
		if err != nil {
			return nil, err
		}
		if seen[process.Name] {
			return nil, fmt.Errorf("process %q is listed twice", process.Name)
		}
		seen[process.Name] = true
		processes = append(processes, process)
	}
	return processes, nil
}

// ProcessSpec validates one process.
func ProcessSpec(request Process) (kitchenv1alpha1.ProcessSpec, error) {
	process := kitchenv1alpha1.ProcessSpec{
		Name:     strings.TrimSpace(request.Name),
		Type:     kitchenv1alpha1.ProcessType(strings.TrimSpace(request.Type)),
		Command:  request.Command,
		Args:     request.Args,
		Previews: request.Previews,
	}
	if err := ValidateProcessName(process.Name); err != nil {
		return process, err
	}
	switch process.Type {
	case kitchenv1alpha1.ProcessWorker, kitchenv1alpha1.ProcessCron,
		kitchenv1alpha1.ProcessService, kitchenv1alpha1.ProcessTask:
	default:
		return process, fmt.Errorf("process %q: type must be worker, cron, service or task (got %q)",
			process.Name, request.Type)
	}

	if err := applyProcessPort(&process, request); err != nil {
		return process, err
	}
	if err := applyProcessBuild(&process, request); err != nil {
		return process, err
	}

	schedule := strings.TrimSpace(request.Schedule)
	switch {
	case process.Type == kitchenv1alpha1.ProcessCron && schedule == "":
		return process, fmt.Errorf("process %q: a cron process needs a schedule", process.Name)
	case process.Type == kitchenv1alpha1.ProcessTask && schedule != "":
		return process, fmt.Errorf(
			"process %q: a task runs once per deploy and has no schedule — give it type cron to run it on one",
			process.Name)
	case process.Type != kitchenv1alpha1.ProcessCron && schedule != "":
		return process, fmt.Errorf(
			"process %q: a %s runs continuously and has no schedule — give it type cron to run it on one",
			process.Name, process.Type)
	}
	process.Schedule = schedule

	if policy := strings.TrimSpace(request.ConcurrencyPolicy); policy != "" {
		switch kitchenv1alpha1.ConcurrencyPolicy(policy) {
		case kitchenv1alpha1.ConcurrencyAllow, kitchenv1alpha1.ConcurrencyForbid, kitchenv1alpha1.ConcurrencyReplace:
			process.ConcurrencyPolicy = kitchenv1alpha1.ConcurrencyPolicy(policy)
		default:
			return process, fmt.Errorf(
				"process %q: concurrencyPolicy must be Allow, Forbid or Replace (got %q)", process.Name, policy)
		}
	}
	if timeout := strings.TrimSpace(request.Timeout); timeout != "" {
		parsed, err := time.ParseDuration(timeout)
		if err != nil || parsed <= 0 {
			return process, fmt.Errorf(
				"process %q: timeout must be a positive Go duration like 30m or 2h (got %q)", process.Name, timeout)
		}
		process.Timeout = &metav1.Duration{Duration: parsed}
	}
	if request.Replicas != nil {
		if *request.Replicas < 0 {
			return process, fmt.Errorf("process %q: replicas cannot be negative (got %d)", process.Name, *request.Replicas)
		}
		process.Replicas = request.Replicas
	}
	if request.Singleton {
		if process.Type == kitchenv1alpha1.ProcessCron {
			return process, fmt.Errorf(
				"process %q: a scheduled process cannot be a singleton — whether two of its runs may "+
					"overlap is concurrencyPolicy, and Forbid (the default) is what says they may not",
				process.Name)
		}
		if process.Type == kitchenv1alpha1.ProcessTask {
			return process, fmt.Errorf(
				"process %q: a task is one run per deploy, so there is never a second copy of it to "+
					"overlap — singleton says nothing here",
				process.Name)
		}
		// Refused rather than clamped, the same way the project's own
		// runtime refuses it: a replica count quietly lowered reads back as
		// a setting that did not take.
		if request.Replicas != nil && *request.Replicas > 1 {
			return process, fmt.Errorf(
				"process %q: it says two of this worker must never run at once, so it cannot ask for %d replicas — "+
					"leave replicas at 1, or turn singleton off",
				process.Name, *request.Replicas)
		}
		process.Singleton = true
	}
	if request.Health != nil {
		if process.Type == kitchenv1alpha1.ProcessCron {
			return process, fmt.Errorf(
				"process %q: a scheduled process is not kept alive by a health check — how a run went is its exit status",
				process.Name)
		}
		if process.Type == kitchenv1alpha1.ProcessTask {
			return process, fmt.Errorf(
				"process %q: a task is not kept alive by a health check — how its run went is its exit status, "+
					"and the deploy waits for that",
				process.Name)
		}
		// A service publishes a port, so its check falls back to that one the
		// way the web process's does. A worker publishes none, which is why
		// its check has to name the listener it is made against.
		needsPort := process.Type != kitchenv1alpha1.ProcessService
		health, err := HealthSpec(*request.Health, fmt.Sprintf("process %q health", process.Name), needsPort)
		if err != nil {
			return process, err
		}
		process.Health = health
	}
	if err := applyProcessResources(&process, request); err != nil {
		return process, err
	}
	return process, nil
}

// applyProcessPort validates the port a service workload is addressed by.
//
// It is required on a service and refused on everything else. Refused rather
// than ignored: a port on a worker would read back as a setting that took,
// and nothing would ever connect to it.
func applyProcessPort(process *kitchenv1alpha1.ProcessSpec, request Process) error {
	if process.Type != kitchenv1alpha1.ProcessService {
		if request.Port != 0 {
			return fmt.Errorf(
				"process %q: only a service is addressed, so only a service has a port — a worker that serves a "+
					"health listener names that port on its health check instead",
				process.Name)
		}
		return nil
	}
	if request.Port < 1 || request.Port > 65535 {
		return fmt.Errorf(
			"process %q: a service has to say which port it listens on, between 1 and 65535 — it is what the rest "+
				"of the unit addresses it by (got %d)",
			process.Name, request.Port)
	}
	process.Port = request.Port
	return nil
}

// applyProcessBuild validates a workload's own build.
//
// A workload's root directory is **a build root**, and its Dockerfile path is
// relative to that root — the same two relations a project's own build has,
// so they are spelled and refused the same way. Both go through
// [detect.NormalizeRoot] and [detect.NormalizeDockerfile], and a path that
// reaches above what its build sees is refused by [detect.LeavesRoot] rather
// than resolved: there is nothing above a build root to resolve against,
// since the builder is handed that directory and nothing else.
//
// It is refused here rather than in the build pod, which is handed the path
// as-is — and here rather than in each caller, because this is the one
// validator a settings PATCH and a committed kitchen.json both go through.
func applyProcessBuild(process *kitchenv1alpha1.ProcessSpec, request Process) error {
	if request.Build == nil {
		return nil
	}
	if process.Type == kitchenv1alpha1.ProcessCron {
		return fmt.Errorf(
			"process %q: a scheduled process runs an image, it is not one — declare the build on the worker or "+
				"service that ships it, and run this on that image",
			process.Name)
	}

	build := &kitchenv1alpha1.ProcessBuildSpec{}
	switch strategy := strings.TrimSpace(request.Build.Strategy); kitchenv1alpha1.BuildStrategy(strategy) {
	case "":
		build.Strategy = kitchenv1alpha1.BuildStrategyDockerfile
	case kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks:
		build.Strategy = kitchenv1alpha1.BuildStrategy(strategy)
	default:
		return fmt.Errorf(
			"process %q: build.strategy must be dockerfile or buildpacks (got %q) — there is no auto for a "+
				"workload, which has neither question detection answers",
			process.Name, request.Build.Strategy)
	}

	for _, declared := range []struct{ field, value, within string }{
		{"build.rootDirectory", request.Build.RootDirectory, withinRepository},
		{"build.dockerfilePath", request.Build.DockerfilePath, withinWorkloadRoot},
	} {
		if detect.LeavesRoot(declared.value) {
			return fmt.Errorf("process %q: %s must stay inside %s (got %q)",
				process.Name, declared.field, declared.within, strings.TrimSpace(declared.value))
		}
	}

	// A stage is refused on the shape of its name, by the one rule the
	// project's own target is refused by: which stages the file has is not
	// knowable here — the repository is not read on a write, and the file
	// changes with every commit — but a name the dockerfile frontend cannot
	// hold could never match a stage that exists, and refusing it on the
	// form beats a build that fails several minutes later.
	if target := detect.NormalizeTarget(request.Build.DockerfileTarget); target != "" {
		if !detect.ValidTarget(target) {
			return fmt.Errorf(
				"process %q: build.dockerfileTarget must name a stage of the Dockerfile — %s (got %q)",
				process.Name, detect.StageNameRule, request.Build.DockerfileTarget)
		}
		build.DockerfileTarget = target
	}

	// An unset path stays unset, so that the CRD's own default is what says
	// what it means — the same reading the project's own build takes, and the
	// reason neither reads back as a setting somebody has to notice they can
	// clear.
	build.RootDirectory = detect.NormalizeRoot(request.Build.RootDirectory)
	if dockerfile := strings.TrimSpace(request.Build.DockerfilePath); dockerfile != "" {
		build.DockerfilePath = detect.NormalizeDockerfile(dockerfile)
	}
	process.Build = build
	return nil
}

// The two things a workload's build paths are relative to, named once so the
// refusal reads the same either way. They are the API's own two sentences for
// a project's build paths, one workload down.
const (
	withinRepository   = "the repository"
	withinWorkloadRoot = "this workload's root directory, which is the whole of what its build sees"
)

func applyProcessResources(process *kitchenv1alpha1.ProcessSpec, request Process) error {
	for name, value := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    strings.TrimSpace(request.CPU),
		corev1.ResourceMemory: strings.TrimSpace(request.Memory),
	} {
		if value == "" {
			continue
		}
		if _, err := resource.ParseQuantity(value); err != nil {
			return fmt.Errorf("process %q: %s must be a Kubernetes quantity like 250m or 512Mi (got %q)",
				process.Name, name, value)
		}
		if err := ApplyResource(&process.Resources, name, value); err != nil {
			return fmt.Errorf("process %q: %w", process.Name, err)
		}
	}
	return nil
}

// ValidateProcessName checks a process's name, which becomes part of the name
// of the workload that runs it.
func ValidateProcessName(name string) error {
	if name == "" {
		return errors.New("every process needs a name")
	}
	if name == "web" {
		return errors.New(
			"a process cannot be called \"web\": the web process is the project's own runtime " +
				"(its port, replicas and resources), and this list is what the project runs besides it")
	}
	if err := ValidateProjectName(name); err != nil {
		return fmt.Errorf("process %q: %w", name, err)
	}
	return nil
}

// ApplyResource sets one compute resource as both request and limit —
// applications get the guaranteed class, not a burstable surprise — or clears
// it for an empty value.
func ApplyResource(resources *corev1.ResourceRequirements, name corev1.ResourceName, value string) error {
	if value == "" {
		delete(resources.Requests, name)
		delete(resources.Limits, name)
		return nil
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return fmt.Errorf("%s must be a Kubernetes quantity like 250m or 512Mi (got %q)", name, value)
	}
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{}
	}
	if resources.Limits == nil {
		resources.Limits = corev1.ResourceList{}
	}
	resources.Requests[name] = quantity
	resources.Limits[name] = quantity
	return nil
}
