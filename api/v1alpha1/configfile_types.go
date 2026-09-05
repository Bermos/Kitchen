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

// Software the platform did not build is configured by a file (#311).
//
// Kitchen configures an application three ways — environment variables, a
// secret a variable reads, and a volume it can write to — and twelve-factor
// configuration is a decision the platform may assume of code written to be
// deployed by it. It cannot assume it of code somebody else wrote: Home
// Assistant is configured by `configuration.yaml`, Gitea by `app.ini`, and a
// large share of the vendored estate by a file at a fixed path.
//
// A config file is **configuration, not storage**. It is small, it changes
// with a deploy, and it is frozen into the Release beside the variables — so
// a rollback restores the file that release ran with, which is the whole of
// why it does not live in a claimed volume written into by hand.

// ConfigFileContentLimit bounds one file. A ConfigMap is capped at 1MiB in
// total by the API server and a Secret likewise, so the per-file cap is well
// under it: the refusal then names the limit rather than surfacing as the API
// server's complaint about an object that is already too big to write.
const ConfigFileContentLimit = 128 << 10

// ConfigFile is one file of a project's deployable configuration: what is in
// it, where it is mounted, and which of the unit's workloads read it.
//
// **Secrecy is a flag on this one object rather than a second list**, and the
// reasoning is in docs/api/files.md: a file that becomes secret keeps its
// path, its workloads and its place in the list, so splitting the declaration
// in two would mean moving it house to change one property of its content.
// What *is* split is the writing: a plain file's content travels on
// `PATCH /projects/{name}` with the rest of the declaration, and a secret
// file's content has a route of its own that no response reads back.
//
// **A file is never templated from the environment.** It is stored and
// mounted exactly as written. Substituting variables into it would make the
// platform a template engine, which is the first step towards the thing
// #285's compiled catalogue exists to refuse — and the moment a file is
// rendered per environment it stops being the thing the Release froze.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.secret) && self.secret && has(self.content))",message="a secret file's content is not written here: declare it with `secret: true` and send the content to PUT /projects/{name}/files/{file}, which no response reads back"
type ConfigFile struct {
	// Name identifies the file within the project — it is the key the
	// platform stores the content under and the name every other surface
	// refers to it by, so it is a key rather than a path: `configuration`,
	// `app-ini`. The path is separate because two workloads of one unit may
	// want the same file in different places one day, and because a path is
	// not a usable key.
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Path is where the file appears inside the container: absolute, and
	// naming the file itself rather than the directory it is in. Only that
	// one path is replaced — the rest of the directory is the image's, which
	// is what a config file dropped beside an application's own files needs.
	//
	// **It is optional, and a file with no path is placed in no container.**
	// Such a file exists to be seeded into a volume by a workload's `init`
	// (#348), and it has to be able to have no path because a mounted config
	// file is read-only: mounted at the place the seed would write, it would
	// shadow the volume's own copy for ever and the application could never
	// rewrite what it was given. A file that neither names a path nor is
	// seeded anywhere is inert, which is a declaration that does nothing
	// rather than one that does something surprising.
	// +kubebuilder:validation:Pattern=`^/([^/]+/)*[^/]+$`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Path string `json:"path,omitempty"`

	// Content is the file, verbatim. It is empty for a secret file, whose
	// content the platform holds where nothing reads it back, and may be
	// empty for a plain one — a file that has to exist and say nothing is a
	// thing some software wants.
	// +kubebuilder:validation:MaxLength=131072
	// +optional
	Content string `json:"content,omitempty"`

	// Secret says the content is a credential: it is held in a Secret rather
	// than in this object, written through its own route, and never answered
	// by any response on the API. The declaration — the path, the workloads,
	// the fact of it — stays here and is read by anyone who may read the
	// project, because that is not the secret part.
	// +optional
	Secret bool `json:"secret,omitempty"`

	// Workloads are the workloads of this unit that mount the file, by name
	// — `web` for the web process, and a process's own name for anything in
	// `spec.processes`. An empty list is every workload of the unit, which
	// is what a vendored application's single config file wants and is
	// therefore the default.
	//
	// It is a list of names rather than a per-workload declaration because
	// one file reaching three workloads is one fact: a unit is one
	// application, and its configuration file is the application's.
	// +optional
	// +listType=atomic
	Workloads []string `json:"workloads,omitempty"`
}

// ReachesWorkload reports whether the named workload mounts this file. A file
// that named no workload reaches all of them.
func (f ConfigFile) ReachesWorkload(workload string) bool {
	return len(f.Workloads) == 0 || slices.Contains(f.Workloads, workload)
}

// ConfigFilesFor is the subset of a release's files one workload mounts, in
// the order they were declared — which is the order they were written in, so
// two reconciles of an unchanged Release build the same pod spec.
func ConfigFilesFor(files []ConfigFile, workload string) []ConfigFile {
	var mounted []ConfigFile
	for _, file := range files {
		if file.ReachesWorkload(workload) {
			mounted = append(mounted, file)
		}
	}
	return mounted
}
