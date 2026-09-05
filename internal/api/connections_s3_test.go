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
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/backup"
)

// `caFile` is a path inside the operator's own pod rather than a value about
// somebody's store, and the platform is what puts a bundle there — the CA it
// mints for the store it bundles (#382). So the one path it may name is that
// one: any other would have the operator read a file of the caller's choosing
// and answer whether it parses as a certificate.
func TestTheOnlyCABundleAConnectionMayNameIsThePlatformsOwn(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		config := map[string]any{"endpoint": "https://minio.example.com", "forcePathStyle": true}
		for key, value := range extra {
			config[key] = value
		}
		return config
	}

	// The seeded Connection read back and written again is not a refusal:
	// that is the shape the dashboard and `kitchen api` both round-trip.
	if err := validS3Config(base(map[string]any{"caFile": backup.InternalCAFile})); err != nil {
		t.Errorf("the platform's own bundle was refused: %v", err)
	}
	// And a store of somebody's own, verified against the host's roots, is
	// what leaving it out asks for.
	if err := validS3Config(base(nil)); err != nil {
		t.Errorf("a connection naming no bundle was refused: %v", err)
	}
	if err := validS3Config(base(map[string]any{"caFile": "  "})); err != nil {
		t.Errorf("an empty bundle was refused: %v", err)
	}

	for _, value := range []any{"/etc/passwd", "/var/run/secrets/kubernetes.io/serviceaccount/token", 7} {
		err := validS3Config(base(map[string]any{"caFile": value}))
		if err == nil {
			t.Errorf("config.caFile %v was accepted: the operator would read a file the "+
				"request chose and say whether it holds a certificate", value)
			continue
		}
		if !strings.Contains(err.Error(), backup.InternalCAFile) {
			t.Errorf("the refusal does not say which bundle is the platform's: %v", err)
		}
	}
}
