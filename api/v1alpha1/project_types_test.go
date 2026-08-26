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

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// A plain bool with omitempty cannot say "off": false is the zero value, so
// the field is dropped from the serialized object, and a CRD default of true
// then fills the gap the client meant to leave empty. Both of PreviewsSpec's
// switches are pointers for that reason, and this is the assertion that says
// so — it fails the moment either goes back to being a bool.
func TestPreviewsSpecCanSayOff(t *testing.T) {
	off, err := json.Marshal(PreviewsSpec{Enabled: ptr.To(false), Protected: ptr.To(false)})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"enabled":false`, `"protected":false`} {
		if !strings.Contains(string(off), want) {
			t.Errorf("want %s in the serialized spec, got %s", want, off)
		}
	}

	// And an unset spec still says nothing at all, so the API server's
	// defaults are what decide it.
	unset, err := json.Marshal(PreviewsSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if string(unset) != "{}" {
		t.Errorf("want an unset spec to serialize to {}, got %s", unset)
	}
}

// An absent value reads as on, which is what the CRD's default would have
// made of it anyway — so a Project written before the field existed, or one
// read from a client that dropped it, gets previews rather than losing them.
func TestPreviewsSpecDefaultsToOn(t *testing.T) {
	var unset PreviewsSpec
	if !unset.IsEnabled() {
		t.Error("want an unset previews spec to be enabled")
	}
	if !unset.IsProtected() {
		t.Error("want an unset previews spec to be protected")
	}
	if (PreviewsSpec{Enabled: ptr.To(false)}).IsEnabled() {
		t.Error("want an explicit false to turn previews off")
	}
	if !(PreviewsSpec{Enabled: ptr.To(true)}).IsEnabled() {
		t.Error("want an explicit true to turn previews on")
	}
}

func TestScaleToZeroPolicyCovers(t *testing.T) {
	cases := []struct {
		name       string
		policy     ScaleToZeroPolicy
		preview    bool
		production bool
	}{
		{
			// A Project written before the field existed. The whole feature
			// does nothing unless the platform turns it on, and a platform
			// that has is asking for idle previews.
			name:    "unset idles previews alone",
			policy:  ScaleToZeroPolicy{},
			preview: true,
		},
		{
			name:    "previews idles previews alone",
			policy:  ScaleToZeroPolicy{Mode: ScaleToZeroPreviews},
			preview: true,
		},
		{
			name:       "always idles production too",
			policy:     ScaleToZeroPolicy{Mode: ScaleToZeroAlways},
			preview:    true,
			production: true,
		},
		{
			name:   "never idles nothing",
			policy: ScaleToZeroPolicy{Mode: ScaleToZeroNever},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Covers(EnvironmentPreview); got != tc.preview {
				t.Errorf("preview: want %t, got %t", tc.preview, got)
			}
			if got := tc.policy.Covers(EnvironmentProduction); got != tc.production {
				t.Errorf("production: want %t, got %t", tc.production, got)
			}
		})
	}
}

func TestScaleToZeroPolicyDefaults(t *testing.T) {
	var unset ScaleToZeroPolicy
	if got := unset.IdleAfterOrDefault(); got != DefaultIdleAfter {
		t.Errorf("want the default idle window %s, got %s", DefaultIdleAfter, got)
	}
	if got := unset.MaxReplicasOrDefault(); got != DefaultMaxReplicas {
		t.Errorf("want the default ceiling %d, got %d", DefaultMaxReplicas, got)
	}

	// A zero on either is a value nothing could work with: an environment that
	// idles the instant it is quiet, or one allowed no pods at all even under
	// load. Both read as "not set".
	zeroed := ScaleToZeroPolicy{
		IdleAfter:   &metav1.Duration{},
		MaxReplicas: new(int32),
	}
	if got := zeroed.IdleAfterOrDefault(); got != DefaultIdleAfter {
		t.Errorf("want a zero duration to fall back, got %s", got)
	}
	if got := zeroed.MaxReplicasOrDefault(); got != DefaultMaxReplicas {
		t.Errorf("want a zero ceiling to fall back, got %d", got)
	}

	set := ScaleToZeroPolicy{
		IdleAfter:   &metav1.Duration{Duration: 30 * time.Second},
		MaxReplicas: func() *int32 { r := int32(12); return &r }(),
	}
	if got := set.IdleAfterOrDefault(); got != 30*time.Second {
		t.Errorf("want the configured idle window, got %s", got)
	}
	if got := set.MaxReplicasOrDefault(); got != 12 {
		t.Errorf("want the configured ceiling, got %d", got)
	}
}
