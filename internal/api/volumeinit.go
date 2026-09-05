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
	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/appconfig"
)

// What a workload prepares inside its volumes before it starts, over the API
// (#348).
//
// It is a field of the workload rather than a route of its own, and rides the
// two bodies that already carry what a workload is: `runtime.init` on
// PATCH /projects/{name} for the web process, and `init` on each entry of
// that body's `processes` for the rest. There is no new route because there
// is no new thing — this is one more property of a workload, the way its
// health check and its security posture are, and a second route for it would
// be a second place to look for what a project runs.
//
// It reads back exactly as it was written. Nothing here is a credential and
// nothing is resolved: the steps are the project's declaration, and a client
// that reads the list to edit one entry has to be able to send back what it
// did not touch.

// volumeInitRequest and the validation behind it live in internal/appconfig,
// which is the one implementation of what a request may say — so a kitchen.json
// and a settings PATCH cannot come to disagree about what a step is.
type volumeInitRequest = appconfig.VolumeInit

// volumeInitView is one volume's preparation read back.
type volumeInitView struct {
	Volume      string                    `json:"volume"`
	Directories []volumeInitDirectoryView `json:"directories,omitempty"`
	Seed        []volumeInitSeedView      `json:"seed,omitempty"`
}

// volumeInitDirectoryView is one directory created if it is absent.
type volumeInitDirectoryView struct {
	Path string `json:"path"`
	// Mode is octal as a string, absent where the step named none and the
	// platform's 0755 applies.
	Mode string `json:"mode,omitempty"`
}

// volumeInitSeedView is one configuration file copied in once.
type volumeInitSeedView struct {
	File string `json:"file"`
	Path string `json:"path"`
	Mode string `json:"mode,omitempty"`
}

// newVolumeInitViews reads one workload's whole declaration back, in the
// order it was declared — which is the order the steps run in.
func newVolumeInitViews(inits []kitchenv1alpha1.VolumeInit) []volumeInitView {
	if len(inits) == 0 {
		return nil
	}
	views := make([]volumeInitView, 0, len(inits))
	for _, init := range inits {
		view := volumeInitView{Volume: init.Volume}
		for _, dir := range init.Directories {
			view.Directories = append(view.Directories,
				volumeInitDirectoryView{Path: dir.Path, Mode: dir.Mode})
		}
		for _, seed := range init.Seed {
			view.Seed = append(view.Seed,
				volumeInitSeedView{File: seed.File, Path: seed.Path, Mode: seed.Mode})
		}
		views = append(views, view)
	}
	return views
}
