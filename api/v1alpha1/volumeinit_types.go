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

import "slices"

// Preparing a volume before the process that mounts it starts (#348).
//
// A `volume` claim (#267) hands a workload an empty filesystem, and a large
// part of the vendored estate will not start on one: Gitea wants a directory
// tree that exists before it looks at it, Home Assistant wants a
// `configuration.yaml` it may then rewrite. `fsGroup` (#347) settled who owns
// the volume and #311 settled how a file is placed into a *container* —
// neither creates a directory, and a file the platform mounts is read-only by
// construction, so neither of them gets an empty volume into a state the
// process will accept.
//
// **This is an init step in the model, and it is declarative.** The three
// answers the issue put up were an init step, a task allowed to mount another
// process's volume, and saying plainly that such images are not Kitchen
// projects. The second changes #267's one-process rule — two pods on one
// ReadWriteOnce volume is the `Multi-Attach` failure that rule exists to
// prevent — and the third leaves the two features that do work still adding
// up to an application that does not start. So: the first, with the shape
// that keeps the platform out of the business of running somebody's shell.
//
// What a project declares is **not a command**. It is a list of typed steps
// the platform executes itself, in an init container in the process's own
// pod, with that process's volume mounted and the project's own
// `runtime.security` posture on it. There is no argv taken from a request
// anywhere in it — the same rule the KEDA install job follows — and no shell
// at any point.
//
// Every step is **idempotent by construction**, because this runs on every
// start and not only the first:
//
//   - a directory is created if it is absent and otherwise left exactly as it
//     is, mode and ownership included. A step that re-applied a mode on every
//     start would fight the application for its own volume.
//   - a seed is copied only when the destination does not exist. A second
//     deploy therefore never clobbers what the application wrote, which is
//     the whole difference between this and a step that would be worse than
//     no step at all.
//
// Ownership is nobody's job here. The init container runs as the pod runs —
// the same `runAsUser`, the same `fsGroup` — so a directory it creates comes
// out owned by the process that will use it. That is #347's field doing the
// work, and it is why there is no `owner` to declare and no `chown` to run.

// VolumeInit is the preparation one workload needs done inside one of the
// volumes it mounts, before its own container starts.
//
// It names the volume claim rather than carrying absolute paths, and that is
// what makes the whole feature safe rather than carefully validated: every
// path in it is relative to that claim's mount, and the init container mounts
// nothing but the volumes named here and the platform's own copy of the files
// it seeds from. A step cannot reach out of the volume because there is
// nothing out there to reach.
//
// +kubebuilder:validation:XValidation:rule="has(self.directories) || has(self.seed)",message="a volume init has to do something: give it directories to create, files to seed, or leave it out"
type VolumeInit struct {
	// Volume is the ResourceClaim of type `volume` this prepares, by name,
	// and it has to be one this same workload mounts.
	//
	// A claim names the one process that mounts it (#267), so the pairing is
	// already decided elsewhere and this only points at it. An init naming a
	// volume this workload does not mount is refused where the two can be
	// compared — the environment's reconcile — and says so on the
	// environment rather than producing a pod that fails obscurely.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Volume string `json:"volume"`

	// Directories are created inside the volume if they are not there.
	// +optional
	// +listType=map
	// +listMapKey=path
	Directories []VolumeInitDirectory `json:"directories,omitempty"`

	// Seed copies configuration files into the volume, once each.
	// +optional
	// +listType=map
	// +listMapKey=path
	Seed []VolumeInitSeed `json:"seed,omitempty"`
}

// VolumeInitDirectory is one directory the process needs to find.
type VolumeInitDirectory struct {
	// Path is where it goes inside the volume, relative to the claim's own
	// mount path: `data`, `custom_components/foo`.
	//
	// The pattern is what makes it relative *by construction* rather than by
	// a check that could be forgotten: every segment starts with a letter, a
	// digit or an underscore, so `..`, `.` and a leading `/` are not
	// spellable and there is no path here that leaves the volume.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`

	// Mode is the permission bits the directory is created with, in octal
	// and written as a string — `"0750"`. A string because the number is
	// octal and JSON's is not: `0750` decimal is 1356 octal, which is the
	// classic way a mode field means something nobody intended.
	//
	// It applies only when the directory is created. A directory that is
	// already there keeps whatever mode it has, because the volume belongs
	// to the application from its first start onwards.
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +optional
	Mode string `json:"mode,omitempty"`
}

// VolumeInitSeed is a configuration file copied into the volume, and only if
// the destination is not there already.
//
// The source is a file of `spec.files` (#311) — the same declaration, the
// same content frozen into the Release — because a config file is the thing
// the platform already knows how to hold, snapshot and roll back. What is
// different is where it ends up: mounted, a config file is read-only and
// would shadow the volume's own copy for ever; seeded, it is written once and
// is the application's from then on.
//
// A file used only as a seed is a file with no `path`: it is placed in no
// container and exists to be copied in here. See [ConfigFile.Path].
type VolumeInitSeed struct {
	// File is the name of the configuration file to copy — the key in
	// `spec.files`, not a path.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +kubebuilder:validation:MaxLength=253
	File string `json:"file"`

	// Path is where the copy goes inside the volume, relative to the claim's
	// mount path and naming the file itself: `configuration.yaml`,
	// `conf/app.ini`. Parent directories are created as needed, so a seed
	// deep in a tree does not need a `directories` entry as well.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`
	// +kubebuilder:validation:MaxLength=512
	Path string `json:"path"`

	// Mode is the permission bits the copy is created with, in octal as a
	// string. Applied only when the file is written, for the reason a
	// directory's is.
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +optional
	Mode string `json:"mode,omitempty"`
}

// VolumeInitFor is the init declaration for one volume, or nil where the
// workload declared none for it.
func VolumeInitFor(inits []VolumeInit, volume string) *VolumeInit {
	for i := range inits {
		if inits[i].Volume == volume {
			return &inits[i]
		}
	}
	return nil
}

// SeededFiles are the configuration files a workload's init copies, in the
// order they are declared and each named once however many volumes seed it.
//
// It is what decides which files the init container is handed: a workload
// that seeds two of a project's nine files gets those two and no others, for
// the reason the config file mounts project their items rather than the whole
// object.
func SeededFiles(inits []VolumeInit) []string {
	var files []string
	for _, init := range inits {
		for _, seed := range init.Seed {
			if !slices.Contains(files, seed.File) {
				files = append(files, seed.File)
			}
		}
	}
	return files
}
