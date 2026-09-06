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

package controller

import (
	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// How a workload is started, which is not the same question as which image it
// runs (#440).
//
// A Dockerfile image is started by replacing its entrypoint, and that is what
// a container `command` is for. A Cloud Native Buildpacks image is not: its
// entrypoint *is* the thing that makes the image work.
//
//	Entrypoint: ["/cnb/lifecycle/launcher"]
//
// The launcher applies the environment the buildpacks provided — PATH
// included, since the runtime lives in a layer rather than in the base image —
// and then runs the process. A command written in place of it runs in an
// environment no buildpack has touched:
//
//	exec: "node": executable file not found in $PATH
//
// So the command is handed *to* the launcher rather than put in its place,
// which is the CNB contract and what `docker run --entrypoint launcher <image>
// <command>` does. The launcher is named explicitly rather than left to the
// image's own entrypoint, because an image with a default process type does
// not have the bare launcher as its entrypoint — the exporter points it at
// `/cnb/process/<type>`, which would take these words as *arguments to that
// process* instead of as a command of their own.
//
// This is the whole of why the buildpacks strategy could carry no command at
// all: `buildpacks` is what `auto` selects for a repository with no
// Dockerfile, so on the default path no worker, no scheduled run and no deploy
// task could run one.

// CNBLauncherPath is where the Cloud Native Buildpacks lifecycle puts the
// launcher in every image it exports. It is fixed by the CNB specification
// rather than by the builder, so it does not move with the pinned builder
// image above it.
const CNBLauncherPath = "/cnb/lifecycle/launcher"

// launchCommand is the container command and arguments one workload runs: the
// declaration as written for an image built from a Dockerfile, and the same
// declaration handed to the launcher for one built with buildpacks.
//
// A workload that declares neither is left alone under both strategies. That
// is the image starting itself — its own entrypoint for a Dockerfile image,
// and its default process type for a buildpacks one — and naming the launcher
// with nothing to run would turn "no default process" from a start into a
// failure with no command to fix it.
//
// `args` alone already reached the launcher, since a container that overrides
// only the arguments keeps the image's entrypoint. It goes through here all
// the same: an image with a default process type would append them to that
// process rather than run them, which is a different program from the one the
// project asked for.
func launchCommand(
	strategy kitchenv1alpha1.BuildStrategy,
	command, args []string,
) (containerCommand, containerArgs []string) {
	if strategy != kitchenv1alpha1.BuildStrategyBuildpacks || len(command)+len(args) == 0 {
		return command, args
	}
	launched := make([]string, 0, len(command)+len(args))
	launched = append(launched, command...)
	launched = append(launched, args...)
	return []string{CNBLauncherPath}, launched
}
