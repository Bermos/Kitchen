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
	"reflect"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What a move between two releases would change (#181).
//
// A Release is an immutable snapshot of an image digest *and the configuration
// it runs with*, which is what makes rollback exact — and also what makes it
// more than a change of image. The environment variables, the runtime and the
// process list all come back with the release, and until this route existed
// nothing could say so before the write was made: the confirm step revealed
// nothing the release list had not already shown.
//
// **The diff is computed here because the values must not leave.** The API
// never reads a variable's value back — `envVarView` reports only that a
// variable has one — so a client cannot compare two snapshots itself without
// the platform first handing it every literal the project ever set. The
// comparison happens on this side of the wire and only the verdict crosses it:
// `changed`, never what it changed to. That is the whole answer to "does the
// diff need an endpoint" — it needs one precisely so that it does not need the
// values.
//
// The runtime and the process list are a different matter and are reported in
// full: a port, a replica count and a cron expression are configuration a
// viewer already reads off the project, and a rollback that quietly restored
// yesterday's schedule would be exactly the surprise this route exists to
// prevent.

// Change kinds, from the perspective of the release named in the path: what
// moving *to* it from `against` would do.
const (
	changeAdded     = "added"
	changeRemoved   = "removed"
	changeChanged   = "changed"
	changeUnchanged = "unchanged"
)

// Where a variable's value comes from. A literal is `value`; the other two
// name a reference, which is not a secret and travels.
const (
	sourceValue  = "value"
	sourceSecret = "secret"
	sourceClaim  = "claim"
)

// variableChangeView is one environment variable across the two snapshots.
//
// There is no `value` and there never will be one. `change` is the platform's
// comparison of two literals it holds and the client does not, and `source`
// says where each side's value comes from — which is the part a reader
// actually acts on, because a variable that moved from a literal to a claim
// binding has changed in a way no diff of values would explain.
type variableChangeView struct {
	Name   string `json:"name"`
	Change string `json:"change"`
	// Source is where the value comes from in the release named in the path;
	// empty when the release does not carry the variable at all.
	Source string `json:"source,omitempty"`
	// AgainstSource is the same for the release compared against; empty when
	// that one does not carry it.
	AgainstSource string `json:"againstSource,omitempty"`
	// Ref and AgainstRef name the key a reference-backed variable reads, on
	// each side. Absent for a literal.
	Ref        *keyRefView `json:"ref,omitempty"`
	AgainstRef *keyRefView `json:"againstRef,omitempty"`
	// PreviewOnly marks a change that is confined to the preview override:
	// the two releases agree about what every environment but a preview
	// runs with. Without it a preview-only edit reads on a production
	// environment as a change to production, which it is not.
	PreviewOnly bool `json:"previewOnly,omitempty"`
}

// fieldChangeView is one runtime field that differs, with both values. These
// are not secrets — the port, the replica count and the compute request are
// project settings a viewer already reads — so unlike a variable they are
// reported as themselves.
type fieldChangeView struct {
	Field   string `json:"field"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Changed bool   `json:"changed"`
}

// processChangeView is one workload across the two releases.
// `schedule` is carried for a cron process because "the nightly job goes back
// to running at 02:00" is the kind of consequence somebody rolling back at
// 01:55 would want to have been told.
type processChangeView struct {
	Name     string `json:"name"`
	Change   string `json:"change"`
	Type     string `json:"type,omitempty"`
	Schedule string `json:"schedule,omitempty"`
	// Image is what this workload runs after the move, for one built from its
	// own directory of the repository. It travels for the reason the schedule
	// does: a unit ships several images, and "the API goes back two commits
	// too" is the consequence somebody rolling back is least likely to have
	// worked out for themselves. Absent for a workload that runs the
	// release's own image, which is what the release name already says.
	Image string `json:"image,omitempty"`
}

// configDiffBody is what GET /releases/{name}/config-diff answers with.
//
// Every list is complete — unchanged entries included — because the count of
// what did not move is part of the reassurance. A caller that wants only the
// differences filters on `change`.
type configDiffBody struct {
	Release   string               `json:"release"`
	Against   string               `json:"against"`
	Project   string               `json:"project"`
	Variables []variableChangeView `json:"variables"`
	Runtime   []fieldChangeView    `json:"runtime"`
	Processes []processChangeView  `json:"processes"`
	Files     []fileChangeView     `json:"files"`
}

// fileChangeView is one configuration file across the two snapshots.
//
// A **plain** file's content is compared here and never travels, for the
// reason a variable's literal does not: the comparison is over what the
// platform holds, and only the verdict crosses the wire. A **secret** file's
// content is not in either snapshot at all — it is the project's, held where
// nothing reads it back — so what moves between two releases is its
// declaration, and `change` says so and says nothing about the credential.
type fileChangeView struct {
	Name string `json:"name"`
	// Path each side mounts it at. It is configuration a viewer already
	// reads off the project, so unlike the content it travels as itself: a
	// rollback that moved a file from one path to another is exactly the
	// surprise this route exists to name.
	Path        string `json:"path,omitempty"`
	AgainstPath string `json:"againstPath,omitempty"`
	// Secret says the content is a credential on the side named in the path,
	// which is what tells a reader that "changed" here cannot be about
	// content.
	Secret        bool   `json:"secret,omitempty"`
	AgainstSecret bool   `json:"againstSecret,omitempty"`
	Change        string `json:"change"`
}

// releaseConfigDiff compares the configuration snapshot of the release named
// in the path against another release of the same project.
//
// The direction is "what would happen if this release became current": the
// path names where the environment is going, `against` where it is now. So a
// variable the current release sets and the target does not reads `removed`,
// which is the sentence somebody about to roll back needs.
func (s *Server) releaseConfigDiff(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	release := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, req.PathValue("name"), release); err != nil {
		s.writeError(w, err)
		return
	}

	againstName := strings.TrimSpace(req.URL.Query().Get("against"))
	if againstName == "" {
		badRequest(w, "name the release to compare against with ?against=<release name>")
		return
	}
	if againstName == release.Name {
		badRequest(w, "release %q cannot be compared against itself", release.Name)
		return
	}
	against := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, againstName, against); err != nil {
		s.writeError(w, err)
		return
	}
	// The authorization on this route is resolved from the release in the
	// path. A second release from another project would be read past that
	// check, so it is refused rather than compared — and refused as a bad
	// request, since comparing two projects' snapshots is meaningless
	// whoever asks.
	if against.Spec.ProjectRef.Name != release.Spec.ProjectRef.Name {
		badRequest(w, "release %q belongs to project %q, but release %q belongs to project %q",
			against.Name, against.Spec.ProjectRef.Name, release.Name, release.Spec.ProjectRef.Name)
		return
	}

	writeJSON(w, http.StatusOK, configDiffBody{
		Release:   release.Name,
		Against:   against.Name,
		Project:   release.Spec.ProjectRef.Name,
		Variables: diffEnv(release.Spec.ConfigSnapshot.Env, against.Spec.ConfigSnapshot.Env),
		Runtime:   diffRuntime(release.Spec.ConfigSnapshot.Runtime, against.Spec.ConfigSnapshot.Runtime),
		Processes: diffProcesses(release, against),
		Files:     diffFiles(release.Spec.ConfigSnapshot.Files, against.Spec.ConfigSnapshot.Files),
	})
}

// changeRank orders the answer by how much a reader needs to see the row:
// what changed first, then what appears, then what disappears, then the
// reassurance. Both clients render the list in the order it arrives, so the
// sort lives here rather than being decided twice.
func changeRank(change string) int {
	switch change {
	case changeChanged:
		return 0
	case changeRemoved:
		return 1
	case changeAdded:
		return 2
	default:
		return 3
	}
}

// envSource names where a variable's value comes from. Exactly one of the
// three is set on a well-formed EnvVar; a literal is the fallback, which is
// also what an empty variable reads as.
func envSource(v kitchenv1alpha1.EnvVar) string {
	switch {
	case v.SecretRef != nil:
		return sourceSecret
	case v.FromResourceClaim != nil:
		return sourceClaim
	default:
		return sourceValue
	}
}

func envRef(v kitchenv1alpha1.EnvVar) *keyRefView {
	switch {
	case v.SecretRef != nil:
		return &keyRefView{Name: v.SecretRef.Name, Key: v.SecretRef.Key}
	case v.FromResourceClaim != nil:
		return &keyRefView{Name: v.FromResourceClaim.Name, Key: v.FromResourceClaim.Key}
	default:
		return nil
	}
}

// diffEnv compares two snapshots' variables by name.
//
// The comparison is over the whole EnvVar — literal, preview literal and both
// references — so that a variable which kept its value but moved from a Secret
// to a claim binding reads as changed, which it is. Nothing about the literals
// survives into the answer beyond the verdict.
func diffEnv(release, against []kitchenv1alpha1.EnvVar) []variableChangeView {
	byName := func(vars []kitchenv1alpha1.EnvVar) map[string]kitchenv1alpha1.EnvVar {
		out := make(map[string]kitchenv1alpha1.EnvVar, len(vars))
		for _, v := range vars {
			out[v.Name] = v
		}
		return out
	}
	head, base := byName(release), byName(against)

	names := make([]string, 0, len(head)+len(base))
	for name := range head {
		names = append(names, name)
	}
	for name := range base {
		if _, both := head[name]; !both {
			names = append(names, name)
		}
	}

	out := make([]variableChangeView, 0, len(names))
	for _, name := range names {
		to, inHead := head[name]
		from, inBase := base[name]
		view := variableChangeView{Name: name}
		switch {
		case inHead && !inBase:
			view.Change = changeAdded
			view.Source, view.Ref = envSource(to), envRef(to)
		case !inHead && inBase:
			view.Change = changeRemoved
			view.AgainstSource, view.AgainstRef = envSource(from), envRef(from)
		default:
			view.Change = changeUnchanged
			if !reflect.DeepEqual(to, from) {
				view.Change = changeChanged
				// Blanking the preview override on both sides leaves what
				// every environment but a preview runs with. Equal there and
				// the difference is a preview's alone.
				to.PreviewValue, from.PreviewValue = "", ""
				view.PreviewOnly = reflect.DeepEqual(to, from)
			}
			view.Source, view.Ref = envSource(to), envRef(to)
			view.AgainstSource, view.AgainstRef = envSource(from), envRef(from)
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := changeRank(out[i].Change), changeRank(out[j].Change); ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// diffFiles compares two snapshots' configuration files by name.
//
// The comparison is over the whole ConfigFile — content, path, secrecy and
// the workloads it reaches — so that a file which kept its content and moved
// to another path reads as changed, which it is. Nothing of the content
// survives into the answer beyond the verdict.
func diffFiles(release, against []kitchenv1alpha1.ConfigFile) []fileChangeView {
	byName := func(files []kitchenv1alpha1.ConfigFile) map[string]kitchenv1alpha1.ConfigFile {
		out := make(map[string]kitchenv1alpha1.ConfigFile, len(files))
		for _, file := range files {
			out[file.Name] = file
		}
		return out
	}
	head, base := byName(release), byName(against)

	names := make([]string, 0, len(head)+len(base))
	for name := range head {
		names = append(names, name)
	}
	for name := range base {
		if _, both := head[name]; !both {
			names = append(names, name)
		}
	}

	out := make([]fileChangeView, 0, len(names))
	for _, name := range names {
		to, inHead := head[name]
		from, inBase := base[name]
		view := fileChangeView{
			Name:          name,
			Path:          to.Path,
			AgainstPath:   from.Path,
			Secret:        to.Secret,
			AgainstSecret: from.Secret,
		}
		switch {
		case inHead && !inBase:
			view.Change = changeAdded
		case !inHead && inBase:
			view.Change = changeRemoved
		case reflect.DeepEqual(to, from):
			view.Change = changeUnchanged
		default:
			view.Change = changeChanged
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := changeRank(out[i].Change), changeRank(out[j].Change); ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func replicaCount(replicas *int32) string {
	if replicas == nil {
		return ""
	}
	return strconv.FormatInt(int64(*replicas), 10)
}

// portString leaves an unset port empty rather than rendering it "0": a
// release built before the project named one has its port derived from the
// framework, and "0" would read as a choice nobody made.
func portString(value int32) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(int64(value), 10)
}

// wordList renders a command or an argument list for the diff. Arguments are
// configuration a viewer already reads off the project, so unlike a variable's
// value they travel as themselves — and a rollback that restored the image but
// not the flags would have restored the wrong thing, which is exactly what
// this route exists to say before the write is made.
//
// The words are joined with a space rather than rendered as JSON because the
// answer is read by a person; an empty list is empty, which reads as "the
// image's own", the same as an absent one.
func wordList(words []string) string {
	return strings.Join(words, " ")
}

// diffRuntime reports the runtime fields, changed ones first. Every field is
// listed rather than only the differing ones: "the port is the same" is an
// answer, and a list that shrinks to nothing would leave a reader unable to
// tell "nothing changed" from "nothing was compared".
func diffRuntime(release, against kitchenv1alpha1.RuntimeSpec) []fieldChangeView {
	fields := []fieldChangeView{
		{Field: "port", To: portString(release.Port), From: portString(against.Port)},
		{Field: "replicas", To: replicaCount(release.Replicas), From: replicaCount(against.Replicas)},
		{Field: "command", To: wordList(release.Command), From: wordList(against.Command)},
		{Field: "args", To: wordList(release.Args), From: wordList(against.Args)},
		{Field: "previewArgs", To: wordList(release.PreviewArgs), From: wordList(against.PreviewArgs)},
		// The posture in words rather than as six fields: what a reader
		// about to roll back needs is whether the workload is about to run
		// under different constraints, and an empty one is the platform's
		// default on both sides.
		{Field: "security",
			To:   strings.Join(release.Security.Declared(), "; "),
			From: strings.Join(against.Security.Declared(), "; ")},
		{Field: "cpuRequest",
			To:   quantityString(release.Resources.Requests, corev1.ResourceCPU),
			From: quantityString(against.Resources.Requests, corev1.ResourceCPU)},
		{Field: "cpuLimit",
			To:   quantityString(release.Resources.Limits, corev1.ResourceCPU),
			From: quantityString(against.Resources.Limits, corev1.ResourceCPU)},
		{Field: "memoryRequest",
			To:   quantityString(release.Resources.Requests, corev1.ResourceMemory),
			From: quantityString(against.Resources.Requests, corev1.ResourceMemory)},
		{Field: "memoryLimit",
			To:   quantityString(release.Resources.Limits, corev1.ResourceMemory),
			From: quantityString(against.Resources.Limits, corev1.ResourceMemory)},
	}
	out := make([]fieldChangeView, 0, len(fields))
	for _, field := range fields {
		field.Changed = field.From != field.To
		out = append(out, field)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Changed && !out[j].Changed })
	return out
}

// diffProcesses compares the two releases' workloads by name. One whose
// command, schedule, port, build, concurrency or capacity moved reads as
// changed; so does one whose own image moved, since a unit ships several and
// each is frozen on the release. The schedule travels because when a job next
// fires is the consequence somebody is most likely to be surprised by.
func diffProcesses(release, against *kitchenv1alpha1.Release) []processChangeView {
	byName := func(procs []kitchenv1alpha1.ProcessSpec) map[string]kitchenv1alpha1.ProcessSpec {
		out := make(map[string]kitchenv1alpha1.ProcessSpec, len(procs))
		for _, p := range procs {
			out[p.Name] = p
		}
		return out
	}
	head := byName(release.Spec.ConfigSnapshot.Processes)
	base := byName(against.Spec.ConfigSnapshot.Processes)

	names := make([]string, 0, len(head)+len(base))
	for name := range head {
		names = append(names, name)
	}
	for name := range base {
		if _, both := head[name]; !both {
			names = append(names, name)
		}
	}

	out := make([]processChangeView, 0, len(names))
	for _, name := range names {
		to, inHead := head[name]
		from, inBase := base[name]
		view := processChangeView{Name: name}
		switch {
		case inHead && !inBase:
			view.Change = changeAdded
		case !inHead && inBase:
			view.Change = changeRemoved
		case reflect.DeepEqual(to, from) && release.ImageFor(name) == against.ImageFor(name):
			view.Change = changeUnchanged
		default:
			view.Change = changeChanged
		}
		// A removed process is described by what it was; everything else by
		// what it becomes.
		described, describing := to, release
		if !inHead {
			described, describing = from, against
		}
		view.Type = string(described.Type)
		view.Schedule = described.Schedule
		if image := describing.ImageFor(name); image != describing.Spec.Image {
			view.Image = image
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := changeRank(out[i].Change), changeRank(out[j].Change); ri != rj {
			return ri < rj
		}
		return out[i].Name < out[j].Name
	})
	return out
}
