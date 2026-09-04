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

package api

import (
	"net/http"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Which image of a unit a request is about (#300).
//
// A commit produces one image per workload that declares a build of its own,
// every one of them is deployed by the Release, and every one of them carries
// its own evidence against its own digest. So an endpoint that attaches
// something to "the artifact" — a gate result, an exploitability assertion —
// has to be told *which* artifact, and an endpoint that reads one back has to
// be able to be asked about any of them.
//
// The default is the web process's, spelled as the absent parameter, because
// that is the artifact every one of these endpoints has always meant and the
// only one a single-workload project has. `workload=web` is the same thing
// said out loud.

// artifactRefusal is why a request names no artifact this build has.
type artifactRefusal struct {
	status  int
	message string
}

// requestedArtifact resolves which image of a unit a request names.
//
// `tail` completes the sentence a refusal is written in — "so there is
// nothing " plus "a gate result could be about" — so that the endpoints that
// ask this refuse in their own words rather than in a shared one that fits
// none of them.
func requestedArtifact(
	build *kitchenv1alpha1.Build, workload, tail string,
) (kitchenv1alpha1.BuildArtifact, *artifactRefusal) {
	workload = strings.TrimSpace(workload)
	if workload == kitchenv1alpha1.WebProcessName {
		workload = ""
	}
	if workload != "" && !buildHasWorkload(build, workload) {
		return kitchenv1alpha1.BuildArtifact{}, &artifactRefusal{
			status: http.StatusBadRequest,
			message: "`workload` names no workload of this build; it built " +
				strings.Join(buildArtifactNames(build), ", "),
		}
	}

	artifact := build.ArtifactFor(workload)
	if artifact == nil || artifact.Digest == "" || artifact.Repository == "" {
		subject := "this build produced no artifact digest"
		if workload != "" {
			subject = "workload " + workload + " of this build produced no artifact digest"
		}
		return kitchenv1alpha1.BuildArtifact{}, &artifactRefusal{
			status:  http.StatusConflict,
			message: subject + ", so there is nothing " + tail,
		}
	}
	return kitchenv1alpha1.BuildArtifact{Workload: workload, Artifact: artifact}, nil
}

// buildHasWorkload is whether the build recorded this workload at all, which
// is a different question from whether it produced an artifact: a workload
// that failed is one the build knows about and has no image for, and the two
// refusals say different things.
func buildHasWorkload(build *kitchenv1alpha1.Build, workload string) bool {
	for i := range build.Status.Workloads {
		if build.Status.Workloads[i].Name == workload {
			return true
		}
	}
	return false
}

// buildArtifactNames is every image of the unit by name, for a message that
// has to say what the caller could have asked for.
func buildArtifactNames(build *kitchenv1alpha1.Build) []string {
	names := []string{kitchenv1alpha1.WebProcessName}
	for i := range build.Status.Workloads {
		names = append(names, build.Status.Workloads[i].Name)
	}
	return names
}
