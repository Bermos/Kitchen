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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// A project's own secrets: the credentials Kitchen did not mint.
//
// Everything here is about one property — a value goes in and never comes back
// out, by any route, in any response, or into the audit log — plus the two
// facts that make the value usable: it reaches the platform namespace under
// the name the reconciler mirrors from, and the reference the list answers is
// the one an environment variable is written with.

const (
	secretsPath = "/api/v1/projects/" + feedProject + "/secrets"
	// theValue is the credential every test here writes. It is a distinctive
	// string so that "does this response, or this record, contain it" is a
	// substring search over the whole thing rather than a walk of fields.
	theValue = "hunter2-correct-horse-battery-staple"
	// theSecret is the name most of these write it under.
	theSecret = "SMTP_PASSWORD"
)

// storedSecrets reads the Secret the API writes a project's secrets into.
func storedSecrets(t *testing.T, h *harness, project string) map[string][]byte {
	t.Helper()
	secret, err := getSecret(t, h, controller.ProjectSecretsSourceName(project))
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return secret.Data
}

func TestSettingAProjectSecretStoresItAndNeverAnswersIt(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPut, secretsPath+"/"+theSecret, `{"value": "`+theValue+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), theValue) {
		t.Fatalf("the response carried the value back: %s", recorder.Body.String())
	}
	view := decode[projectSecretView](t, recorder)
	// The reference is the whole point of answering anything at all: it is
	// what an environment variable is written with, and the caller should not
	// have to know the name of the object the platform keeps secrets in.
	if view.Name != theSecret ||
		view.Reference.Name != controller.ProjectSecretsName ||
		view.Reference.Key != theSecret {
		t.Fatalf("the reference does not name the secret: %+v", view)
	}

	// The value reached the Secret the ProjectReconciler mirrors from — which
	// is what makes it reach the application.
	if got := string(storedSecrets(t, h, feedProject)[theSecret]); got != theValue {
		t.Fatalf("the platform namespace holds %q, want the value that was written", got)
	}

	listing := h.do(t, http.MethodGet, secretsPath, "")
	if listing.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", listing.Code, listing.Body.String())
	}
	if strings.Contains(listing.Body.String(), theValue) {
		t.Fatalf("the listing carried the value: %s", listing.Body.String())
	}
	items := decode[listBody[projectSecretView]](t, listing).Items
	if len(items) != 1 || items[0].Name != theSecret {
		t.Fatalf("the listing does not name the secret: %+v", items)
	}
}

func TestRotatingAProjectSecretReplacesTheValue(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPut, secretsPath+"/API_KEY",
		`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The same path a second time is a rotation, and answers 200 rather than
	// 201: nothing new exists, and a caller that had to know which it was
	// doing would have to be told whether a value is already there.
	recorder := h.do(t, http.MethodPut, secretsPath+"/API_KEY", `{"value": "rotated-value"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200 on a rotation, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := string(storedSecrets(t, h, feedProject)["API_KEY"]); got != "rotated-value" {
		t.Fatalf("the stored value is %q, want the rotated one", got)
	}
}

func TestASecondProjectSecretJoinsTheFirst(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, name := range []string{"B_SECOND", "A_FIRST"} {
		if recorder := h.do(t, http.MethodPut, secretsPath+"/"+name,
			`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
			t.Fatalf("setting %s: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}

	items := decode[listBody[projectSecretView]](t, h.do(t, http.MethodGet, secretsPath, "")).Items
	// Sorted, so two reads of an unchanged project are the same list rather
	// than whatever order the map happened to be walked in.
	if len(items) != 2 || items[0].Name != "A_FIRST" || items[1].Name != "B_SECOND" {
		t.Fatalf("want both secrets in name order, got %+v", items)
	}
}

func TestDeletingTheLastProjectSecretRemovesTheObject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPut, secretsPath+"/ONLY_ONE",
		`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder := h.do(t, http.MethodDelete, secretsPath+"/ONLY_ONE", "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// Empty rather than absent would leave the reconciler mirroring an empty
	// Secret into the application namespace, where a variable referencing a
	// deleted secret would read as an empty value rather than as a container
	// that cannot start.
	if data := storedSecrets(t, h, feedProject); data != nil {
		t.Fatalf("the Secret survived its last key: %v", data)
	}
	if recorder := h.do(t, http.MethodDelete, secretsPath+"/ONLY_ONE", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("deleting it twice answered %d, want 404", recorder.Code)
	}
}

func TestDeletingAProjectSecretAVariableReadsIsRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPut, secretsPath+"/DATABASE_PASSWORD",
		`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPatch, envPath, `{"env": [
		{"name": "DB_PASS", "fromSecret": {"name": "`+controller.ProjectSecretsName+`", "key": "DATABASE_PASSWORD"}}
	]}`); recorder.Code != http.StatusOK {
		t.Fatalf("pointing a variable at the secret: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodDelete, secretsPath+"/DATABASE_PASSWORD", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The refusal is the undo list: the variables to point somewhere else.
	if !strings.Contains(recorder.Body.String(), "DB_PASS") {
		t.Fatalf("the refusal does not name what still reads it: %s", recorder.Body.String())
	}
}

func TestAProjectSecretRefusesAnEmptyOrOversizedValue(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, spec := range []struct{ name, body string }{
		{"empty", `{"value": ""}`},
		{"missing", `{}`},
		{"oversized", `{"value": "` + strings.Repeat("x", projectSecretValueLimit+1) + `"}`},
	} {
		t.Run(spec.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPut, secretsPath+"/SOME_SECRET", spec.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	if data := storedSecrets(t, h, feedProject); data != nil {
		t.Fatalf("a refused write stored something: %v", data)
	}
}

func TestAProjectSecretNameHasToBeAKeyAVariableCanReference(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPut, secretsPath+"/not%20a%20key", `{"value": "`+theValue+`"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The Secret the API writes belongs to its Project, in the sense Kubernetes
// means: same namespace, controller reference, so nothing has to remember it
// exists in order for it to go when the project does. (The project finalizer
// deletes it as well — see internal/controller — because the finalizer is what
// makes the teardown deterministic.)
func TestAProjectsSecretsAreOwnedByTheProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPut, secretsPath+"/OWNED",
		`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	secret, err := getSecret(t, h, controller.ProjectSecretsSourceName(feedProject))
	if err != nil {
		t.Fatal(err)
	}
	owner := metav1.GetControllerOf(secret)
	if owner == nil || owner.Kind != "Project" || owner.Name != feedProject {
		t.Fatalf("the secret is not owned by its project: %+v", secret.OwnerReferences)
	}
	if secret.Labels[managedByLabelKey] != managedByLabelValue {
		t.Fatalf("the secret is not marked as the platform's: %v", secret.Labels)
	}
}

// The property the whole feature rests on: the log records *that* a value
// changed, and never the value. It is checked over the marshalled transition
// rather than field by field, because a field added later would otherwise be
// a leak nobody notices.
func TestTheAuditRecordOfASecretCarriesNoValue(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: feedProject, Namespace: testNamespace},
	}
	for _, operation := range []string{clickhouse.AuditCreate, clickhouse.AuditUpdate, clickhouse.AuditDelete} {
		t.Run(operation, func(t *testing.T) {
			transition := projectSecretTransition(project, theSecret, operation)
			if transition.Kind != "ProjectSecret" || transition.Project != feedProject {
				t.Fatalf("the record is not about the project's secrets: %+v", transition)
			}
			// A credential write, so it is in the same `?privileged=true`
			// view as a connection's rotation.
			if transition.Privileged != "credential" {
				t.Fatalf("the record is not classified as a credential write: %q", transition.Privileged)
			}
			encoded, err := json.Marshal(map[string]any{
				"reason":  transition.Reason,
				"details": transition.Details,
				"from":    transition.From,
				"to":      transition.To,
			})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), theValue) {
				t.Fatalf("the record carries the value: %s", encoded)
			}
			if !strings.Contains(string(encoded), theSecret) {
				t.Fatalf("the record does not say which secret changed: %s", encoded)
			}
		})
	}
}

// Reading is a viewer's, writing is a developer's — the same pair the
// environment variables next door take, because a project secret is the value
// a variable was carrying in cleartext until this existed.
func TestProjectSecretsTakeTheRolesTheVariablesTake(t *testing.T) {
	for _, spec := range []struct {
		role    kitchenv1alpha1.AccessRole
		list    int
		write   int
		refusal string
	}{
		{kitchenv1alpha1.AccessRoleViewer, http.StatusOK, http.StatusForbidden, "developer"},
		{kitchenv1alpha1.AccessRoleDeveloper, http.StatusOK, http.StatusCreated, ""},
		{kitchenv1alpha1.AccessRoleAdmin, http.StatusOK, http.StatusCreated, ""},
	} {
		t.Run(string(spec.role), func(t *testing.T) {
			h := asMember(t, spec.role)

			if recorder := h.do(t, http.MethodGet, secretsPath, ""); recorder.Code != spec.list {
				t.Fatalf("listing answered %d, want %d: %s", recorder.Code, spec.list, recorder.Body.String())
			}
			recorder := h.do(t, http.MethodPut, secretsPath+"/API_KEY", `{"value": "`+theValue+`"}`)
			if recorder.Code != spec.write {
				t.Fatalf("writing answered %d, want %d: %s", recorder.Code, spec.write, recorder.Body.String())
			}
			if spec.refusal != "" && !strings.Contains(recorder.Body.String(), spec.refusal) {
				t.Fatalf("the refusal does not name the role it wanted: %s", recorder.Body.String())
			}
		})
	}
}

// A caller with no role on the project is told it does not exist, which is
// what every other project-scoped route answers them.
func TestAProjectSecretIsInvisibleToSomebodyWithNoRole(t *testing.T) {
	h := asMember(t, "")

	for _, spec := range []struct{ method, path, body string }{
		{http.MethodGet, secretsPath, ""},
		{http.MethodPut, secretsPath + "/API_KEY", `{"value": "` + theValue + `"}`},
		{http.MethodDelete, secretsPath + "/API_KEY", ""},
	} {
		recorder := h.do(t, spec.method, spec.path, spec.body)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s %s answered %d, want 404: %s",
				spec.method, spec.path, recorder.Code, recorder.Body.String())
		}
	}
	if data := storedSecrets(t, h, feedProject); data != nil {
		t.Fatalf("a refused caller wrote a secret: %v", data)
	}
}

// One project's secrets are not another's: the name of the Secret is derived
// from the project, so two projects cannot collide in the platform namespace.
func TestTwoProjectsKeepTheirSecretsApart(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), blogFixtures()...)...)

	if recorder := h.do(t, http.MethodPut, secretsPath+"/SHARED_NAME",
		`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPut, "/api/v1/projects/"+otherProject+"/secrets/SHARED_NAME",
		`{"value": "the-other-one"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	if got := string(storedSecrets(t, h, feedProject)["SHARED_NAME"]); got != theValue {
		t.Fatalf("%s holds %q", feedProject, got)
	}
	if got := string(storedSecrets(t, h, otherProject)["SHARED_NAME"]); got != "the-other-one" {
		t.Fatalf("%s holds %q", otherProject, got)
	}
}

// The API and the reconciler have to agree about where the source is, and the
// only thing holding them together is the one function that spells it.
func TestTheSourceSecretIsWhereTheReconcilerLooks(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPut, secretsPath+"/SOMEWHERE",
		`{"value": "`+theValue+`"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	secret, err := getSecret(t, h, controller.ProjectSecretsSourceName(feedProject))
	if err != nil {
		t.Fatalf("the reconciler's key does not resolve: %v", err)
	}
	if secret.Labels[controller.LabelProject] != feedProject {
		t.Fatalf("the source is not labelled with its project: %v", secret.Labels)
	}
}

// The config diff needed no change for this feature, and this is the check
// that it still needs none: a variable whose value comes from a project secret
// is compared by the reference it names, and neither snapshot's value is read
// in order to compare them — which is what lets two releases be diffed by
// somebody who may not read either value.
func TestAProjectSecretBackedVariableStillDiffsByItsReference(t *testing.T) {
	reading := func(key string) kitchenv1alpha1.EnvVar {
		return kitchenv1alpha1.EnvVar{
			Name:      "DB_PASS",
			SecretRef: &kitchenv1alpha1.SecretKeySelector{Name: controller.ProjectSecretsName, Key: key},
		}
	}

	same := diffEnv([]kitchenv1alpha1.EnvVar{reading("DATABASE_PASSWORD")},
		[]kitchenv1alpha1.EnvVar{reading("DATABASE_PASSWORD")})
	if len(same) != 1 || same[0].Change != changeUnchanged || same[0].Source != sourceSecret {
		t.Fatalf("the same reference read as a change: %+v", same)
	}
	// Rotating the value behind that reference is deliberately not a change
	// to the configuration: the release snapshot holds the reference, and the
	// value is not in it. The rotation is the audit log's to report.
	moved := diffEnv([]kitchenv1alpha1.EnvVar{reading("DATABASE_PASSWORD")},
		[]kitchenv1alpha1.EnvVar{reading("OLD_PASSWORD")})
	if len(moved) != 1 || moved[0].Change != changeChanged {
		t.Fatalf("a variable pointed at another secret did not read as changed: %+v", moved)
	}
	if moved[0].Ref == nil || moved[0].Ref.Key != "DATABASE_PASSWORD" ||
		moved[0].AgainstRef == nil || moved[0].AgainstRef.Key != "OLD_PASSWORD" {
		t.Fatalf("the diff does not carry both references: %+v", moved[0])
	}
}
