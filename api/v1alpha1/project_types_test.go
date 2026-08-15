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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
