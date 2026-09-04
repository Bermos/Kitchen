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

// Package volumeinit is the contract between the operator and the small
// program that prepares a workload's volume before its container starts
// (#348).
//
// The operator builds a [Plan] from what the Release snapshotted and hands it
// to the init container **through the environment**, as JSON under
// [PlanVariable]. It is deliberately not argv: the KEDA install job's rule is
// that nothing from a request reaches a command line, and the same rule
// governs this. The command is the one fixed word `/volume-init`, always,
// whatever the project declared — and there is no shell anywhere in it, so
// there is nothing for a path or a filename to be interpreted by.
//
// Putting the plan in the pod's own spec has a second property worth having:
// a project that changes what it prepares changes the pod template, so the
// workload rolls and the new steps run. Nothing has to digest anything for
// that to be true.
package volumeinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	// PlanVariable carries the whole plan, JSON-encoded.
	PlanVariable = "KITCHEN_VOLUME_INIT"

	// SeedDir is where the operator mounts the platform's copy of the files
	// a plan seeds from — one file per key, named by the file's name. It is
	// a platform path rather than anything a project chose, and it is passed
	// to [Run] rather than read from it so that the rules can be exercised
	// against a directory instead of against the one path the container has.
	SeedDir = "/etc/kitchen/seed"

	// TerminationLog is where a failure is written so that the environment
	// can report it. The kubelet reads this file into the container status's
	// termination message, which is how a step's own words reach a condition
	// on the Environment instead of dying in a pod nobody looks at.
	TerminationLog = "/dev/termination-log"

	// DefaultDirectoryMode and DefaultFileMode are what a step that named no
	// mode creates with. They are the ordinary umask-free answers rather
	// than anything clever: the pod's own identity is what owns the result,
	// so the bits only have to let that identity in.
	DefaultDirectoryMode = fs.FileMode(0o755)
	DefaultFileMode      = fs.FileMode(0o644)
)

// Plan is everything one workload's init container does, in order.
type Plan struct {
	Volumes []Volume `json:"volumes"`
}

// Volume is the preparation of one mounted volume.
type Volume struct {
	// Claim is the ResourceClaim's name, carried only so that a failure can
	// say which volume it was about.
	Claim string `json:"claim"`

	// MountPath is where that volume is mounted in this container, which is
	// the same path the application's own container mounts it at.
	MountPath string `json:"mountPath"`

	Directories []Directory `json:"directories,omitempty"`
	Seeds       []Seed      `json:"seeds,omitempty"`
}

// Directory is one directory created if it is absent.
type Directory struct {
	Path string `json:"path"`
	// Mode is octal as a string, empty for [DefaultDirectoryMode].
	Mode string `json:"mode,omitempty"`
}

// Seed is one file copied in if the destination is absent.
type Seed struct {
	// File is the name under [SeedDir] the content is read from, which is
	// the configuration file's own name.
	File string `json:"file"`
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
}

// Parse reads a plan out of the environment.
func Parse(encoded string) (Plan, error) {
	plan := Plan{}
	if strings.TrimSpace(encoded) == "" {
		return plan, fmt.Errorf("%s is empty: this container is started only with a plan to run", PlanVariable)
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return plan, fmt.Errorf("%s is not a plan this version understands: %w", PlanVariable, err)
	}
	return plan, nil
}

// Step is one thing the plan does, named the way a failure has to name it.
type Step struct {
	// Volume is the claim the step is inside, and What a phrase a person
	// reads: `directory "data"`, `seed "configuration" into "app.yaml"`.
	Volume string
	What   string
}

func (s Step) String() string { return fmt.Sprintf("%s on volume %q", s.What, s.Volume) }

// Run executes the plan against the filesystem, and answers the step that
// failed and why.
//
// Every operation is conditional on the destination not being there, which is
// what makes running it on every start the same as running it once. It is
// also what makes it safe on a volume the application has been living in for
// a year: the platform creates what is missing and touches nothing else.
func Run(plan Plan, seedDir string) (Step, error) {
	for _, volume := range plan.Volumes {
		for _, dir := range volume.Directories {
			step := Step{Volume: volume.Claim, What: fmt.Sprintf("directory %q", dir.Path)}
			if err := makeDirectory(path.Join(volume.MountPath, dir.Path), dir.Mode); err != nil {
				return step, err
			}
		}
		for _, seed := range volume.Seeds {
			step := Step{
				Volume: volume.Claim,
				What:   fmt.Sprintf("seed %q into %q", seed.File, seed.Path),
			}
			if err := placeSeed(volume.MountPath, seedDir, seed); err != nil {
				return step, err
			}
		}
	}
	return Step{}, nil
}

// makeDirectory creates one directory and its parents, and leaves one that is
// already there exactly as it is — mode and owner both. A directory the
// application has since chmod'ed is the application's business.
func makeDirectory(at, mode string) error {
	perm, err := parseMode(mode, DefaultDirectoryMode)
	if err != nil {
		return err
	}
	info, err := os.Stat(at)
	switch {
	case err == nil && info.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%s is already there and is not a directory", at)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	// The parents are created with the platform's own default rather than
	// the step's: the mode was declared for this directory, and silently
	// applying it to a tree the step did not name would be the platform
	// deciding something nobody asked it to.
	if parent := path.Dir(at); parent != "." && parent != "/" {
		if err := os.MkdirAll(parent, DefaultDirectoryMode); err != nil {
			return err
		}
	}
	if err := os.Mkdir(at, perm); err != nil {
		return err
	}
	// Mkdir applies the process umask to the mode, which would quietly turn
	// a declared 0750 into 0750&^umask. A mode that was asked for is applied.
	return os.Chmod(at, perm)
}

// placeSeed copies one file in, and only if nothing is at the destination.
//
// The write is not atomic and does not need to be: nothing else is running in
// this pod yet — the application's container starts after this one exits —
// and a torn write from a killed init container is retried by the same
// container on the next attempt, which finds a destination that exists and a
// process that is about to be started against it. The alternative, a
// temporary file and a rename, would leave that temporary file behind in the
// application's own volume, which is worse.
func placeSeed(mountPath, seedDir string, seed Seed) error {
	perm, err := parseMode(seed.Mode, DefaultFileMode)
	if err != nil {
		return err
	}
	at := path.Join(mountPath, seed.Path)
	switch _, err := os.Stat(at); {
	case err == nil:
		// Already there: the application owns it now. This is the whole of
		// what makes a second deploy safe.
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	content, err := os.ReadFile(path.Join(seedDir, seed.File))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf(
				"the platform did not place the file %q for this pod to seed from: "+
					"the project declares the seed and no file of that name", seed.File)
		}
		return err
	}
	if parent := path.Dir(at); parent != "." && parent != "/" {
		if err := os.MkdirAll(parent, DefaultDirectoryMode); err != nil {
			return err
		}
	}
	if err := os.WriteFile(at, content, perm); err != nil {
		return err
	}
	return os.Chmod(at, perm)
}

// parseMode reads the octal string the API validated, and answers the default
// for an empty one.
func parseMode(mode string, fallback fs.FileMode) (fs.FileMode, error) {
	if mode == "" {
		return fallback, nil
	}
	bits, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("%q is not an octal mode like 0750: %w", mode, err)
	}
	return fs.FileMode(bits), nil
}
