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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The three cases #436 is about, plus the one that has to stay a fault. They
// are spelled out as conditions rather than as table lookups so that a change
// to how severity is decided — not only to the table — is caught here.
func TestConditionSeverity(t *testing.T) {
	cases := []struct {
		name string
		cond metav1.Condition
		want conditionSeverity
	}{{
		name: "previews turned off is a setting, not a failure",
		cond: metav1.Condition{
			Type:   controller.ConditionPreviews,
			Status: metav1.ConditionFalse,
			Reason: controller.ReasonPreviewsDisabled,
		},
		want: severityInfo,
	}, {
		name: "a project with no repository cannot have previews at all",
		cond: metav1.Condition{
			Type:   controller.ConditionPreviews,
			Status: metav1.ConditionFalse,
			Reason: controller.ReasonNoRepository,
		},
		want: severityInfo,
	}, {
		name: "an inventory nobody publishes is an answer, not a caution",
		cond: metav1.Condition{
			Type:   controller.ConditionAppConnected,
			Status: metav1.ConditionUnknown,
			Reason: controller.ReasonNotReported,
		},
		want: severityInfo,
	}, {
		name: "an unprotected installation stays a fault",
		cond: metav1.Condition{
			Type:   controller.ConditionBackupReady,
			Status: metav1.ConditionFalse,
			Reason: controller.ReasonNotScheduled,
		},
		want: severityError,
	}, {
		name: "previews that work say nothing",
		cond: metav1.Condition{
			Type:   controller.ConditionPreviews,
			Status: metav1.ConditionTrue,
			Reason: "Enabled",
		},
		want: severityNone,
	}, {
		name: "an unclassified False is a fault",
		cond: metav1.Condition{
			Type:   "Ready",
			Status: metav1.ConditionFalse,
			Reason: "ContainerRefused",
		},
		want: severityError,
	}, {
		name: "an unclassified Unknown is a caution",
		cond: metav1.Condition{
			Type:   "CredentialsValid",
			Status: metav1.ConditionUnknown,
			Reason: "ProviderUnreachable",
		},
		want: severityWarning,
	}, {
		name: "a reason nobody classified on a classified type is a fault",
		cond: metav1.Condition{
			Type:   controller.ConditionPreviews,
			Status: metav1.ConditionFalse,
			Reason: "SomethingNew",
		},
		want: severityError,
	}, {
		name: "an addon this installation did not ask for is not a failed install",
		cond: metav1.Condition{
			Type:   kitchenv1alpha1.AddonReady,
			Status: metav1.ConditionFalse,
			Reason: controller.ReasonAddonNotInstalled,
		},
		want: severityInfo,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := conditionSeverityOf(tc.cond); got != tc.want {
				t.Fatalf("severity of %s/%s = %q, want %q", tc.cond.Type, tc.cond.Reason, got, tc.want)
			}
		})
	}
}

// Every condition the API serves carries a severity, whatever it is: a client
// that has to fall back to reading the status is a client keeping its own list
// of benign reasons, which is the arrangement #436 replaced.
func TestConditionViewsAlwaysCarryASeverity(t *testing.T) {
	views := conditionViews([]metav1.Condition{
		{Type: controller.ConditionPreviews, Status: metav1.ConditionFalse, Reason: controller.ReasonPreviewsDisabled},
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled"},
	})
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	for _, view := range views {
		if view.Severity == "" {
			t.Errorf("condition %s carries no severity", view.Type)
		}
	}
	if views[0].Severity != string(severityInfo) {
		t.Errorf("previews-off severity = %q, want %q", views[0].Severity, severityInfo)
	}
	if views[1].Severity != string(severityNone) {
		t.Errorf("a satisfied condition's severity = %q, want %q", views[1].Severity, severityNone)
	}
}

// The half of the bargain the operator has to keep.
//
// A severity table is only as good as its coverage: a benign reason nobody
// classified is drawn as a failure again, and the dashboard has no way to
// know. So the convention is that a condition reason whose severity is not
// simply its status is exported from internal/controller as a `Reason…`
// constant — and this test refuses one that is not in the table. Adding the
// constant is the step that makes somebody classify it.
//
// It reads the operator's source rather than reflecting over the package
// because Go has no reflection over constants; `ui/src/lib/design.test.ts`
// holds its half of the design guide the same way.
func TestEveryExportedReasonIsClassified(t *testing.T) {
	sources, err := filepath.Glob(filepath.Join("..", "controller", "*.go"))
	if err != nil {
		t.Fatalf("listing the operator's sources: %v", err)
	}
	fset := token.NewFileSet()

	// Every string constant in the package, so that a `Reason…` declared as
	// an exported alias of an unexported one resolves to its value.
	values := map[string]string{}
	exported := map[string]bool{}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, source, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", source, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range spec.Names {
				if i >= len(spec.Values) {
					continue
				}
				switch value := spec.Values[i].(type) {
				case *ast.BasicLit:
					if value.Kind == token.STRING {
						if unquoted, err := strconv.Unquote(value.Value); err == nil {
							values[name.Name] = unquoted
						}
					}
				case *ast.Ident:
					values[name.Name] = "=" + value.Name
				}
				if strings.HasPrefix(name.Name, "Reason") {
					exported[name.Name] = true
				}
			}
			return true
		})
	}
	if len(exported) == 0 {
		t.Fatal("no exported Reason constants found in internal/controller — has the scan broken?")
	}

	classified := map[string]bool{}
	for statement := range conditionSeverities {
		classified[statement.Reason] = true
	}

	for name := range exported {
		value := values[name]
		for strings.HasPrefix(value, "=") {
			value = values[strings.TrimPrefix(value, "=")]
		}
		if value == "" {
			t.Errorf("%s: could not resolve the reason's value; keep it a string constant", name)
			continue
		}
		if !classified[value] {
			t.Errorf("controller.%s (%q) is exported but not classified: give it a severity in "+
				"conditionSeverities (internal/api/conditions.go), or the dashboard will draw it as a "+
				"failure", name, value)
		}
	}
}
