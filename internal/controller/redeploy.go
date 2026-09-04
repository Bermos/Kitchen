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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// A corrected setting reaching an environment that has no next commit (#392).
//
// A Release freezes the project's configuration at the moment it was cut, and
// that is what makes a rollback exact. The cost of it is that a setting
// corrected afterwards reaches nothing: the runtime lands with the next
// release, a rebuild of an unchanged commit resolves to the Release that
// commit already has, and a Release cannot be edited — so a project could hold
// a wrong setting, the right setting, and no path between them.
//
// The path is a *new* Release from the same commit and the same artifacts,
// carrying a fresh snapshot of what the project says now. Nothing here bends
// the two rules that closed the other doors: the old Release is untouched and
// still describes exactly what it deployed, and the new one is a snapshot of
// its own that a rollback can return to.

// redeployInfix separates a redeployed Release's name from the fingerprint of
// what it froze. It is `-cfg-` rather than a counter because the fingerprint
// is of the content: two redeploys of one commit with the same settings are
// one Release, and a Release name still identifies exactly one snapshot.
const redeployInfix = "-cfg-"

// redeployFingerprintLength is how much of the digest the name carries. Four
// bytes is enough to keep two snapshots of one commit apart — the space is
// the redeploys of a single commit, not the platform — and short enough to
// leave the name inside the 63 characters a label value may hold, which
// matters because the rescan job labels itself with the release's name.
const redeployFingerprintLength = 4

// maxReleaseNameLength is the ceiling a Release name is fitted to. An object
// name may be far longer; a *label value* may not, and the release's name is
// one.
const maxReleaseNameLength = 63

// ConfigSnapshotFor is the configuration a Release cut from this build freezes
// today: the project's settings as they stand, with the commit's own
// kitchen.json applied over them.
//
// It is one function because two things ask the question and their answers
// must not differ — the build that produces a Release, and a redeploy that
// produces one from a build that already ran. A redeploy computing the merge
// its own way would be a second implementation of what a build shipped.
func ConfigSnapshotFor(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
) (kitchenv1alpha1.ConfigSnapshot, error) {
	return repoconfig.Snapshot(kitchenv1alpha1.ConfigSnapshot{
		Env:       project.Spec.Env,
		Runtime:   runtimeFor(project, build),
		Processes: project.Spec.Processes,
		Files:     project.Spec.Files,
	}, build.Status.Config)
}

// RedeployRelease is the Release a redeploy of `current` makes: the same
// commit, the same images, and the configuration the project declares now.
//
// The build is the one that produced `current`, and it is required rather than
// optional: the commit's own kitchen.json lives on it, and a snapshot taken
// without it would quietly drop everything the repository declares. What is
// taken from `current` is what nothing can recompute — the digests that were
// built and attested — which is what makes this the same deployment with a
// corrected configuration rather than a new one.
func RedeployRelease(
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
	current *kitchenv1alpha1.Release,
) (*kitchenv1alpha1.Release, error) {
	snapshot, err := ConfigSnapshotFor(project, build)
	if err != nil {
		return nil, err
	}
	spec := kitchenv1alpha1.ReleaseSpec{
		ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project.Name},
		BuildRef:       current.Spec.BuildRef,
		Image:          current.Spec.Image,
		Workloads:      current.Spec.Workloads,
		ConfigSnapshot: snapshot,
	}
	name, err := RedeployReleaseName(current.Name, spec)
	if err != nil {
		return nil, err
	}
	return &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: current.Namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		Spec: spec,
	}, nil
}

// RedeployReleaseName names a redeployed Release after the commit it is still
// of, plus a fingerprint of everything it freezes.
//
// A build's Release is named after its commit, which is what makes two builds
// of one commit converge on one Release. A redeploy has the same commit and a
// different snapshot, so the commit alone can no longer name it — and a
// counter would make the name say only "the second one", which is not a fact
// about anything. The fingerprint covers the whole spec, so the name means
// what a Release name has always meant: this image set, with this
// configuration.
//
// Redeploying a redeploy does not stack suffixes: the fingerprint replaces the
// one already there, so a project that corrects a setting three times ends
// with three names of one shape rather than one name three times as long.
func RedeployReleaseName(currentName string, spec kitchenv1alpha1.ReleaseSpec) (string, error) {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("fingerprinting the release: %w", err)
	}
	sum := sha256.Sum256(encoded)
	suffix := redeployInfix + hex.EncodeToString(sum[:redeployFingerprintLength])

	base := strings.TrimSuffix(currentName, previousRedeploySuffix(currentName))
	if len(base)+len(suffix) > maxReleaseNameLength {
		base = strings.TrimRight(base[:maxReleaseNameLength-len(suffix)], "-")
	}
	return base + suffix, nil
}

// previousRedeploySuffix is the fingerprint a name already carries, or the
// empty string for a name a build gave.
func previousRedeploySuffix(name string) string {
	index := strings.LastIndex(name, redeployInfix)
	if index <= 0 {
		return ""
	}
	suffix := name[index:]
	if len(suffix) != len(redeployInfix)+2*redeployFingerprintLength {
		return ""
	}
	if _, err := hex.DecodeString(suffix[len(redeployInfix):]); err != nil {
		return ""
	}
	return suffix
}
