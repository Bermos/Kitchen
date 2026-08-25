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

package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	built := func(module string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Version: module}}, true
		}
	}
	unavailable := func() (*debug.BuildInfo, bool) { return nil, false }

	for _, tc := range []struct {
		name   string
		linked string
		read   func() (*debug.BuildInfo, bool)
		want   string
	}{
		{
			name:   "the linker wins over the build information",
			linked: "0.15.0",
			read:   built("v0.14.0"),
			want:   "0.15.0",
		},
		{
			name:   "an installed binary reports the module version it was built from",
			linked: dev,
			read:   built("v0.15.0"),
			want:   "0.15.0",
		},
		{
			name:   "a prerelease keeps its suffix",
			linked: dev,
			read:   built("v0.16.0-rc.1"),
			want:   "0.16.0-rc.1",
		},
		{
			name:   "a build from a working directory stays dev",
			linked: dev,
			read:   built("(devel)"),
			want:   dev,
		},
		{
			name:   "an empty module version stays dev",
			linked: dev,
			read:   built(""),
			want:   dev,
		},
		{
			name:   "no build information at all stays dev",
			linked: dev,
			read:   unavailable,
			want:   dev,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.linked, tc.read); got != tc.want {
				t.Errorf("resolve(%q) = %q, want %q", tc.linked, got, tc.want)
			}
		})
	}
}
