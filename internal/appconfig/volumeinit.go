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

package appconfig

import (
	"fmt"
	"regexp"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What a workload prepares inside its volumes before it starts (#348), as a
// client sends it.
//
// The whole vocabulary is two typed steps — a directory to create and a file
// to seed — and there is deliberately no third that takes a command. What a
// project declares here is executed by the platform's own program in an init
// container in the workload's pod; a step that could name an argv would be a
// project choosing what the platform runs, which is the rule the KEDA install
// job holds to and this holds to for the same reason.

// volumeInitPath is a path inside a volume: relative, and made of segments
// that start with a letter, a digit or an underscore. That last part is not
// decoration — it is what makes `..`, `.` and a leading `/` unspellable, so
// there is no path here that leaves the volume and no check anybody has to
// remember to write. It mirrors the CRD's own pattern, so the refusal is a
// sentence rather than an admission error quoting a regexp.
var volumeInitPath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`)

// volumeInitMode is an octal mode written as a string — `"0750"`. A string
// because the number is octal and JSON's is not.
var volumeInitMode = regexp.MustCompile(`^0[0-7]{3}$`)

// VolumeInit is one volume's preparation as a client sends it.
type VolumeInit struct {
	// Volume is the volume claim this prepares, by name. It has to be one
	// the same workload mounts — a claim names the one process that mounts
	// it — and the pairing is checked where the two can be compared, which
	// is the environment's reconcile.
	Volume string `json:"volume"`
	// Directories are created inside the volume if they are not there, and
	// left exactly as they are if they are.
	Directories []VolumeInitDirectory `json:"directories,omitempty"`
	// Seed copies configuration files in, each only where the destination
	// does not exist — so a second deploy never clobbers what the
	// application wrote.
	Seed []VolumeInitSeed `json:"seed,omitempty"`
}

// VolumeInitDirectory is one directory the process needs to find.
type VolumeInitDirectory struct {
	// Path is relative to the volume's mount path: `data`, `conf/ssl`.
	Path string `json:"path"`
	// Mode is octal as a string — `"0750"`. Empty is 0755, and it applies
	// only when the directory is created.
	Mode string `json:"mode,omitempty"`
}

// VolumeInitSeed is a configuration file copied into the volume once.
type VolumeInitSeed struct {
	// File is the name of a file in the project's `files`, not a path.
	File string `json:"file"`
	// Path is where the copy goes inside the volume, relative to the mount.
	Path string `json:"path"`
	// Mode is octal as a string. Empty is 0644.
	Mode string `json:"mode,omitempty"`
}

// VolumeInits validates one workload's whole init declaration and turns it
// into the spec. `subject` names the workload for the refusals — "the web
// process", `process "worker"` — since the same rules are read for every
// workload of a unit and a sentence that did not say which would be useless
// on a project with four.
//
// It replaces rather than merges, like the process list and the file list:
// the declaration is short, ordered by nothing, and a merge would leave no
// way to delete an entry.
func VolumeInits(requests []VolumeInit, subject string) ([]kitchenv1alpha1.VolumeInit, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	inits := make([]kitchenv1alpha1.VolumeInit, 0, len(requests))
	seen := map[string]bool{}
	for _, request := range requests {
		init, err := volumeInitSpec(request, subject)
		if err != nil {
			return nil, err
		}
		if seen[init.Volume] {
			return nil, fmt.Errorf("%s prepares the volume %q twice: one volume is one entry, "+
				"with all of its steps in it", subject, init.Volume)
		}
		seen[init.Volume] = true
		inits = append(inits, init)
	}
	return inits, nil
}

// volumeInitSpec validates one volume's preparation.
func volumeInitSpec(request VolumeInit, subject string) (kitchenv1alpha1.VolumeInit, error) {
	init := kitchenv1alpha1.VolumeInit{Volume: strings.TrimSpace(request.Volume)}
	if init.Volume == "" {
		return init, fmt.Errorf("%s prepares a volume without saying which: name one of the volume "+
			"claims this workload mounts", subject)
	}
	if err := ValidateProjectName(init.Volume); err != nil {
		return init, fmt.Errorf("%s prepares the volume %q, which cannot be the name of a claim: %w",
			subject, init.Volume, err)
	}
	if len(request.Directories) == 0 && len(request.Seed) == 0 {
		return init, fmt.Errorf("%s prepares the volume %q and says nothing to do to it: give it "+
			"directories to create, files to seed, or take the entry out",
			subject, init.Volume)
	}

	where := fmt.Sprintf("%s, preparing the volume %q", subject, init.Volume)
	paths := map[string]bool{}
	for _, request := range request.Directories {
		path, err := validateVolumeInitPath(request.Path, where, "directory")
		if err != nil {
			return init, err
		}
		mode, err := validateVolumeInitMode(request.Mode, where, path)
		if err != nil {
			return init, err
		}
		if paths[path] {
			return init, fmt.Errorf("%s: %q is listed twice", where, path)
		}
		paths[path] = true
		init.Directories = append(init.Directories,
			kitchenv1alpha1.VolumeInitDirectory{Path: path, Mode: mode})
	}

	seeded := map[string]bool{}
	for _, request := range request.Seed {
		path, err := validateVolumeInitPath(request.Path, where, "seeded file")
		if err != nil {
			return init, err
		}
		file := strings.TrimSpace(request.File)
		if err := ValidateFileName(file); err != nil {
			return init, fmt.Errorf("%s: the seed for %q names no usable file — %w", where, path, err)
		}
		mode, err := validateVolumeInitMode(request.Mode, where, path)
		if err != nil {
			return init, err
		}
		if seeded[path] {
			return init, fmt.Errorf("%s: two files are seeded to %q, and one path is one file", where, path)
		}
		seeded[path] = true
		init.Seed = append(init.Seed,
			kitchenv1alpha1.VolumeInitSeed{File: file, Path: path, Mode: mode})
	}
	return init, nil
}

// validateVolumeInitPath checks a path inside a volume.
func validateVolumeInitPath(path, where, what string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("%s: every %s needs a path inside the volume, like %q",
			where, what, "data/config")
	}
	if !volumeInitPath.MatchString(path) {
		return "", fmt.Errorf(
			"%s: %q cannot be a %s here — the path is relative to the volume's mount, with no leading "+
				"slash and no %q in it, like %q",
			where, path, what, "..", "data/config")
	}
	return path, nil
}

// validateVolumeInitMode checks an octal mode, and lets an empty one through
// as the platform's default.
func validateVolumeInitMode(mode, where, path string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return "", nil
	}
	if !volumeInitMode.MatchString(mode) {
		return "", fmt.Errorf(
			"%s: %q has the mode %q, which is not four octal digits like \"0750\" — "+
				"it is a string because the number is octal and JSON's is not",
			where, path, mode)
	}
	return mode, nil
}

// ValidateSeededFiles checks every seed of a whole unit against the files
// that unit will hold once the write lands.
//
// It is here rather than in [VolumeInits] because it is the one rule a single
// workload's declaration cannot state: the files are the *project's*, and a
// request may add a file and the seed that reads it in one body. It is
// checked at the write so that the refusal names the file, rather than
// arriving later as an environment that will not deploy.
func ValidateSeededFiles(
	runtime []kitchenv1alpha1.VolumeInit,
	processes []kitchenv1alpha1.ProcessSpec,
	files []kitchenv1alpha1.ConfigFile,
) error {
	declared := make(map[string]bool, len(files))
	for _, file := range files {
		declared[file.Name] = true
	}
	subjects := []struct {
		subject string
		inits   []kitchenv1alpha1.VolumeInit
	}{{"the web process", runtime}}
	for _, process := range processes {
		subjects = append(subjects, struct {
			subject string
			inits   []kitchenv1alpha1.VolumeInit
		}{fmt.Sprintf("process %q", process.Name), process.Init})
	}
	for _, workload := range subjects {
		for _, init := range workload.inits {
			for _, seed := range init.Seed {
				if !declared[seed.File] {
					return fmt.Errorf(
						"%s seeds the volume %q from the file %q, which this project does not declare: "+
							"add it to `files` — a file seeded into a volume usually wants no `path`, "+
							"since a mounted file is read-only and would shadow the copy the "+
							"application then owns",
						workload.subject, init.Volume, seed.File)
				}
			}
		}
	}
	return nil
}
