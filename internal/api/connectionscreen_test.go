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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every provider this API admits can be chosen on the connections screen.
//
// "Abstracting the cluster away is the product": a feature is done when the
// operation exists in the REST API *and* the dashboard, and a provider the
// API accepts but no screen offers is exactly the half-finished shape that
// rule exists to catch. It is also invisible — the CRD admits it, the API
// accepts it, the docs describe it, and the only thing missing is the one
// place somebody would go to use it.
//
// That is not hypothetical either: valkey and redis shipped with a claim
// type, a contract, an endpoint section and no way to create the connection
// they bind through, and nothing failed.
//
// The check is textual because the options in the modal are literals. It
// deliberately reads the *screen* rather than a generated file: what is being
// asserted is that somebody can pick it.
func TestEveryProviderCanBeChosenOnTheScreen(t *testing.T) {
	modal, err := os.ReadFile(filepath.Join("..", "..", "ui", "src", "components", "ConnectionModal.vue"))
	if err != nil {
		t.Fatalf("reading the connections screen: %v", err)
	}
	offered := string(modal)

	for _, name := range providerNames {
		if !strings.Contains(offered, `value: "`+name+`"`) {
			t.Errorf("the API admits the %q provider but ConnectionModal.vue offers no way to choose it: "+
				"a connection nobody can create from the dashboard is a feature that only exists in the API",
				name)
		}
	}
}
