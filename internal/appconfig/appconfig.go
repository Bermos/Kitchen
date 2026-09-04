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
	// FSGroup is the gid the volumes mounted into the containers are owned
	// by, which is what makes a non-root workload able to write the volume
	// it was given. Zero is the volume's own ownership left alone.
	FSGroup int64 `json:"fsGroup,omitempty"`
	// FSGroupChangePolicy is when the kubelet applies that ownership.
	// Empty is Kubernetes' default, Always; OnRootMismatch skips the
	// recursive walk when the volume's root already matches.
	FSGroupChangePolicy string `json:"fsGroupChangePolicy,omitempty"`
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
		FSGroup:                  request.FSGroup,
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
	if security.FSGroup < 0 {
		return nil, fmt.Errorf(
			"%s: fsGroup cannot be negative, and 0 leaves the volume's own ownership alone (got %d)",
			subject, security.FSGroup)
	}
	if policy := request.FSGroupChangePolicy; policy != "" {
		switch kitchenv1alpha1.FSGroupChangePolicy(policy) {
		case kitchenv1alpha1.FSGroupChangeAlways, kitchenv1alpha1.FSGroupChangeOnRootMismatch:
			security.FSGroupChangePolicy = kitchenv1alpha1.FSGroupChangePolicy(policy)
		default:
			return nil, fmt.Errorf(
				"%s: fsGroupChangePolicy is %q or %q (got %q)",
				subject, kitchenv1alpha1.FSGroupChangeAlways,
				kitchenv1alpha1.FSGroupChangeOnRootMismatch, policy)
		}
		// A policy without a group changes nothing at all: the kubelet
		// reads it only when there is an fsGroup to apply. Storing one
		// would be a setting that reads back as a declaration and does
		// nothing.
		if security.FSGroup == 0 {
			return nil, fmt.Errorf(
				"%s: fsGroupChangePolicy is when the volume's ownership is changed, so it needs an fsGroup to change it to",
				subject)
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
	// Image is an image this platform did not build, and the third answer to
	// the question Build asks: a repository somewhere and a tag or a digest
	// of it. It excludes Build — a workload is built here or published
	// elsewhere, never both — and absent from both still means the project's
	// own image run with another command.
	Image *Image `json:"image,omitempty"`
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
// The root directory is relative to the repository root rather than to the
// project's own: see [v1alpha1.ProcessBuildSpec], where that is a decision
// rather than an omission.
type ProcessBuild struct {
	// Strategy is auto, dockerfile or buildpacks; empty means auto, which
	// reads this workload's root directory and decides.
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
	if err := applyProcessImage(&process, request); err != nil {
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
		build.Strategy = kitchenv1alpha1.BuildStrategyAuto
	case kitchenv1alpha1.BuildStrategyAuto,
		kitchenv1alpha1.BuildStrategyDockerfile,
		kitchenv1alpha1.BuildStrategyBuildpacks:
		build.Strategy = kitchenv1alpha1.BuildStrategy(strategy)
	default:
		return fmt.Errorf(
			"process %q: build.strategy must be auto, dockerfile or buildpacks (got %q)",
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

// Image is an image this platform did not build, as a client sends it (#307).
//
// It is one shape for both places that take one — a project whose web process
// is vendored, and a workload of any project that is — because they are one
// question, and two spellings of it is how a project's image and a workload's
// would come to be validated differently.
//
// `connection` is the credential the image is *pulled* with, named the way
// every other Connection in this API is named: by name, never as a nested
// spec. It is optional, because a public image is pulled anonymously and
// requiring one would be a Connection somebody had to invent.
type Image struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Connection string `json:"connection,omitempty"`
}

// imageRepository is what a repository reference may look like: lowercase
// path segments, optionally preceded by a registry host with a port. It is
// the CRD's own pattern, stated here so that a bad reference is a sentence
// rather than an admission error naming a regular expression.
var imageRepository = regexp.MustCompile(
	`^([a-z0-9]+([.\-_][a-z0-9]+)*(:[0-9]+)?/)?[a-z0-9]+([._\-][a-z0-9]+)*(/[a-z0-9]+([._\-][a-z0-9]+)*)*$`)

// imageTag and imageDigest are the two ways of naming a version.
var (
	imageTag    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	imageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ImageSource validates one vendored image reference. `field` names where it
// arrived, so that the same sentence can be about a project's source and
// about one workload of it.
func ImageSource(field string, request Image) (*kitchenv1alpha1.ImageSourceSpec, error) {
	image := &kitchenv1alpha1.ImageSourceSpec{
		Repository: strings.TrimSpace(request.Repository),
		Tag:        strings.TrimSpace(request.Tag),
		Digest:     strings.TrimSpace(request.Digest),
	}
	switch {
	case image.Repository == "":
		return nil, fmt.Errorf(
			"%s.repository is required: where the image lives, registry host included — "+
				"ghcr.io/home-assistant/home-assistant", field)
	case strings.ContainsAny(image.Repository, "@: ") && !imageRepository.MatchString(image.Repository):
		return nil, fmt.Errorf(
			"%s.repository is the repository alone, without a tag or a digest: send the version as "+
				"%s.tag or %s.digest (got %q)", field, field, field, image.Repository)
	case !imageRepository.MatchString(image.Repository):
		return nil, fmt.Errorf(
			"%s.repository is not a repository reference: lowercase path segments, "+
				"optionally after a registry host (got %q)", field, image.Repository)
	}
	if image.Tag != "" && !imageTag.MatchString(image.Tag) {
		return nil, fmt.Errorf("%s.tag is not a tag (got %q)", field, image.Tag)
	}
	if image.Digest != "" && !imageDigest.MatchString(image.Digest) {
		return nil, fmt.Errorf(
			"%s.digest is `sha256:` and sixty-four lowercase hex digits (got %q)", field, image.Digest)
	}
	if image.Tag == "" && image.Digest == "" {
		return nil, fmt.Errorf(
			"%s needs a tag or a digest: without one there is no way to say which version of %s to run",
			field, image.Repository)
	}
	if connection := strings.TrimSpace(request.Connection); connection != "" {
		image.ConnectionRef = &kitchenv1alpha1.LocalObjectReference{Name: connection}
	}
	return image, nil
}

// applyProcessImage validates a workload's vendored image.
//
// The two refusals are the two things the CRD's own rules say, restated where
// a person can be answered: a workload is built or vendored and not both, and
// a vendored image needs a version. Whether the *project* has a repository to
// build from at all is not asked here — this validator sees one process and
// no project — and is refused at admission by the rule on ProjectSpec.
func applyProcessImage(process *kitchenv1alpha1.ProcessSpec, request Process) error {
	if request.Image == nil {
		return nil
	}
	if request.Build != nil {
		return fmt.Errorf(
			"process %q: a workload is built from the repository or run from an image somebody else built, "+
				"never both — keep \"build\" or keep \"image\"",
			process.Name)
	}
	image, err := ImageSource(fmt.Sprintf("process %q image", process.Name), *request.Image)
	if err != nil {
		return err
	}
	process.Image = image
	return nil
}

// File is one configuration file as a client sends it (#311).
//
// It is one shape for both surfaces that take one — the settings PATCH and a
// repository's kitchen.json — for the reason [Image] is: they are one
// declaration, and two spellings of it is how the two would come to be
// validated differently.
//
// Content is a pointer, and that is the whole of what makes an editor
// possible against an API that never reads a credential back. A client that
// read the list, changed one entry and sent the rest back has **nothing to
// send** for a secret file's content: an absent `content` therefore keeps
// whatever is stored, and only an explicit one replaces it. A plain file
// reads its content back, so it round-trips either way.
type File struct {
	Name string `json:"name"`
	// Path is where the file appears in the container: absolute, naming the
	// file rather than the directory holding it.
	Path string `json:"path"`
	// Content is the file, verbatim. Absent keeps the stored content;
	// present replaces it. It is refused on a secret file, whose content has
	// a route of its own that no response reads back.
	Content *string `json:"content,omitempty"`
	// Secret says the content is a credential. The declaration stays
	// readable; the content does not.
	Secret bool `json:"secret,omitempty"`
	// Workloads that mount the file, by name — `web` and the project's own
	// process names. Empty is every workload of the unit.
	Workloads []string `json:"workloads,omitempty"`
}

// configFilePath is a mount path: absolute, no trailing slash, and naming a
// file rather than a directory. It mirrors the CRD's own pattern so the
// refusal is a sentence rather than an admission error quoting a regexp.
var configFilePath = regexp.MustCompile(`^/([^/]+/)*[^/]+$`)

// Files validates a whole file list and turns it into the spec, replacing
// rather than merging — the list is what the project declares, and a merge
// would leave no way to delete an entry.
//
// `stored` is what the project already holds, consulted for exactly one
// thing: the content of a file whose request left it out. `workloads` is what
// the unit runs besides its web process, so that a file naming a workload
// nobody declared is refused here rather than mounted into nothing.
func Files(requests []File, stored []kitchenv1alpha1.ConfigFile, workloads []string) ([]kitchenv1alpha1.ConfigFile, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	held := make(map[string]kitchenv1alpha1.ConfigFile, len(stored))
	for _, file := range stored {
		held[file.Name] = file
	}
	known := map[string]bool{kitchenv1alpha1.WebProcessName: true}
	for _, workload := range workloads {
		known[workload] = true
	}

	files := make([]kitchenv1alpha1.ConfigFile, 0, len(requests))
	seen, paths := map[string]bool{}, map[string]string{}
	for _, request := range requests {
		file, err := FileSpec(request, held, known)
		if err != nil {
			return nil, err
		}
		if seen[file.Name] {
			return nil, fmt.Errorf("file %q is listed twice", file.Name)
		}
		seen[file.Name] = true
		// Two files at one path is one file: the second mount wins and the
		// first silently never appears, which is a config file that is
		// declared, saved, shown on the screen and not there.
		for _, workload := range mountedOn(file, workloads) {
			at := workload + ":" + file.Path
			if other, taken := paths[at]; taken {
				return nil, fmt.Errorf(
					"files %q and %q are both mounted at %s on the %s workload: one path is one file",
					other, file.Name, file.Path, workload)
			}
			paths[at] = file.Name
		}
		files = append(files, file)
	}
	return files, nil
}

// mountedOn lists the workloads one file reaches, which for a file that named
// none is every workload the unit has.
func mountedOn(file kitchenv1alpha1.ConfigFile, workloads []string) []string {
	if len(file.Workloads) > 0 {
		return file.Workloads
	}
	return append([]string{kitchenv1alpha1.WebProcessName}, workloads...)
}

// FileSpec validates one configuration file. `held` is what the project
// already holds by name, and `known` the workload names that exist.
func FileSpec(
	request File,
	held map[string]kitchenv1alpha1.ConfigFile,
	known map[string]bool,
) (kitchenv1alpha1.ConfigFile, error) {
	file := kitchenv1alpha1.ConfigFile{
		Name:      strings.TrimSpace(request.Name),
		Path:      strings.TrimSpace(request.Path),
		Secret:    request.Secret,
		Workloads: request.Workloads,
	}
	if err := ValidateFileName(file.Name); err != nil {
		return kitchenv1alpha1.ConfigFile{}, err
	}
	if err := ValidateFilePath(file.Name, file.Path); err != nil {
		return kitchenv1alpha1.ConfigFile{}, err
	}
	for _, workload := range file.Workloads {
		if !known[strings.TrimSpace(workload)] {
			return kitchenv1alpha1.ConfigFile{}, fmt.Errorf(
				"file %q is for the workload %q, which this project does not declare: "+
					"name \"web\" for the web process, or one of the project's own workloads, "+
					"or leave the list out and every workload gets it",
				file.Name, workload)
		}
	}

	switch {
	case file.Secret && request.Content != nil:
		return kitchenv1alpha1.ConfigFile{}, fmt.Errorf(
			"file %q is secret, so its content is not written here: "+
				"send it to PUT /projects/{name}/files/%s, which no response reads back", file.Name, file.Name)
	case file.Secret:
		// Nothing to carry: the content is in the platform's own Secret, and
		// this list holds the declaration.
	case request.Content != nil:
		file.Content = *request.Content
	default:
		// Absent content keeps what is stored. That is what lets a client
		// read the list, change one file's path and send the rest back
		// without having to resend every file's body.
		file.Content = held[file.Name].Content
	}
	if len(file.Content) > kitchenv1alpha1.ConfigFileContentLimit {
		return kitchenv1alpha1.ConfigFile{}, fmt.Errorf(
			"file %q is %d bytes, and a configuration file may be at most %d — "+
				"it is configuration rather than data, and data belongs in a volume",
			file.Name, len(file.Content), kitchenv1alpha1.ConfigFileContentLimit)
	}
	return file, nil
}

// ValidateFileName checks a configuration file's name, which is the key the
// platform stores its content under.
func ValidateFileName(name string) error {
	if name == "" {
		return errors.New("every file needs a name")
	}
	if errs := validation.IsConfigMapKey(name); len(errs) > 0 {
		return fmt.Errorf(
			"%q cannot be the name of a file: use letters, digits, '-', '_' and '.', at most 253 characters — "+
				"it is a name for the file, not its path", name)
	}
	return nil
}

// ValidateFilePath checks where a file is mounted: absolute, naming a file
// rather than a directory, and not reaching for one with `..`.
func ValidateFilePath(name, path string) error {
	if path == "" {
		return fmt.Errorf("file %q names no path: say where in the container it is mounted, like /config/app.yaml", name)
	}
	if !configFilePath.MatchString(path) {
		return fmt.Errorf(
			"file %q is mounted at %q: the path is absolute and names the file itself, like /config/app.yaml", name, path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." || segment == "." {
			return fmt.Errorf("file %q is mounted at %q: a mount path has no %q in it", name, path, segment)
		}
	}
	return nil
}
