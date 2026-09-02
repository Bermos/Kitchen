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
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Command []string `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
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
	// Timeout is a Go duration ("30m") bounding one run. Empty means an hour.
	Timeout string `json:"timeout,omitempty"`
	// Previews opts this process into preview environments. It is off unless
	// asked for; see ProcessSpec.Previews for why that is the decision and
	// not an omission.
	Previews bool `json:"previews,omitempty"`
	// Health is a worker's health check, and it has to name the port it is
	// made against: a process publishes none of its own. It is refused on a
	// scheduled process, whose verdict is its run's exit status.
	Health *Health `json:"health,omitempty"`
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
	case kitchenv1alpha1.ProcessWorker, kitchenv1alpha1.ProcessCron:
	default:
		return process, fmt.Errorf("process %q: type must be worker or cron (got %q)", process.Name, request.Type)
	}

	schedule := strings.TrimSpace(request.Schedule)
	switch {
	case process.Type == kitchenv1alpha1.ProcessCron && schedule == "":
		return process, fmt.Errorf("process %q: a cron process needs a schedule", process.Name)
	case process.Type == kitchenv1alpha1.ProcessWorker && schedule != "":
		return process, fmt.Errorf(
			"process %q: a worker runs continuously and has no schedule — give it type cron to run it on one", process.Name)
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
				"process %q: only a worker can be a singleton — whether two runs of a scheduled process may "+
					"overlap is concurrencyPolicy, and Forbid (the default) is what says they may not",
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
		health, err := HealthSpec(*request.Health, fmt.Sprintf("process %q health", process.Name), true)
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
