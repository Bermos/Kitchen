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

package contract

import "testing"

func TestPreviewModes(t *testing.T) {
	for _, mode := range PreviewModes {
		if !mode.Known() {
			t.Errorf("%q is listed and not known", mode)
		}
	}
	if PreviewMode("copy").Known() {
		t.Error("an unlisted mode must not be known")
	}
	if !PreviewBranch.Isolated() || !PreviewFresh.Isolated() {
		t.Error("a branch and a fresh resource are a preview's own")
	}
	if PreviewShared.Isolated() || PreviewNone.Isolated() {
		t.Error("shared and none give a preview nothing of its own")
	}
}
