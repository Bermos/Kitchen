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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
)

// What a CI key promises: it is a member of one project, it can do that
// project's day job and nothing on any other, creating one writes both halves
// or neither, and revoking one takes the grant with it.

const (
	// ciKeyName is the key these tests issue, and ciKeySubject the machine
	// account the identity provider creates to own it.
	ciKeyName    = "nightly"
	ciKeySubject = "user_ci"
	// keysPath is the path both project-scoped key routes answer on.
	keysPath = "/api/v1/projects/" + feedProject + "/keys"
)

// ciKeyEmail is the machine account's address: the project and the key name,
// under the reserved domain that says it is not a person.
var ciKeyEmail = fmt.Sprintf("%s.%s@%s", feedProject, ciKeyName, idp.MachineAccountDomain)

// The key half of the stub identity provider: it stores keys the way the real
// one does — one machine account per key, addressed by project and name.
func (d *stubDirectory) Keys(_ context.Context, project string) ([]idp.Key, error) {
	if d.keysErr != nil {
		return nil, d.keysErr
	}
	return append([]idp.Key(nil), d.keys[project]...), nil
}

func (d *stubDirectory) CreateKey(_ context.Context, project, name string) (*idp.IssuedKey, error) {
	if d.createErr != nil {
		return nil, d.createErr
	}
	for _, existing := range d.keys[project] {
		if existing.Name == name {
			return nil, idp.ErrKeyExists
		}
	}
	key := idp.Key{
		Name:    name,
		Project: project,
		Subject: ciKeySubject,
		Email:   fmt.Sprintf("%s.%s@%s", project, name, idp.MachineAccountDomain),
		Prefix:  "abc123",
		Created: time.Unix(1, 0).UTC(),
	}
	if d.keys == nil {
		d.keys = map[string][]idp.Key{}
	}
	d.keys[project] = append(d.keys[project], key)
	return &idp.IssuedKey{Key: key, Secret: "the-key-value"}, nil
}

func (d *stubDirectory) DeleteKey(_ context.Context, project, name string) (*idp.Key, error) {
	if d.deleteErr != nil {
		return nil, d.deleteErr
	}
	for i, existing := range d.keys[project] {
		if existing.Name == name {
			d.keys[project] = append(d.keys[project][:i], d.keys[project][i+1:]...)
			d.deleted = append(d.deleted, name)
			return &existing, nil
		}
	}
	return nil, idp.ErrKeyNotFound
}

// grantTo puts one grant on a project, which is how these tests write the
// membership a key would have been created with.
func (h *harness) grantTo(t *testing.T, project, subject, email string, role kitchenv1alpha1.AccessRole) {
	t.Helper()
	obj := &kitchenv1alpha1.Project{}
	key := types.NamespacedName{Namespace: testNamespace, Name: project}
	if err := h.server.Client.Get(context.Background(), key, obj); err != nil {
		t.Fatal(err)
	}
	obj.Spec.Access = append(obj.Spec.Access, kitchenv1alpha1.AccessGrant{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: subject, Email: email},
		Role:          role,
	})
	if err := h.server.Client.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

// storedAccess is the project's grants as they are actually written down.
func (h *harness) storedAccess(t *testing.T, project string) []kitchenv1alpha1.AccessGrant {
	t.Helper()
	obj := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), project, obj); err != nil {
		t.Fatal(err)
	}
	return obj.Spec.Access
}

// The whole point of the feature: a key is a member of one project. It can do
// that project's day job, it cannot see another project at all, and it cannot
// touch the platform — which is the sentence issue #61 asked for, "a CI key
// that can trigger a build should not also be able to change the base domain".
func TestACIKeyIsAMemberOfExactlyOneProject(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), blogFixtures()...)...)
	h.demoteCaller(t)
	h.grantTo(t, feedProject, ciKeySubject, ciKeyEmail, kitchenv1alpha1.AccessRoleDeveloper)

	// The token a key is exchanged for carries the machine account's `sub`,
	// which is the subject the grant above names. Nothing about it says it is
	// a key: it is an ordinary platform token.
	asKey := h.issuer.tokenFor(t, ciKeySubject, ciKeyEmail)

	if recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/builds", "", asKey); recorder.Code !=
		http.StatusCreated {
		t.Fatalf("a key must be able to build its own project: %d %s", recorder.Code, recorder.Body.String())
	}

	// Another project is not refused, it is invisible: a caller with no role
	// on `blog` is told there is no such project.
	if recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+otherProject+"/builds", "", asKey); recorder.Code !=
		http.StatusNotFound {
		t.Fatalf("a key must not be able to build another project: %d %s", recorder.Code, recorder.Body.String())
	}

	// And the platform's own surface stays the operator's.
	recorder := h.do(t, http.MethodPatch, "/api/v1/settings", `{"builds":{"timeoutMinutes":10}}`, asKey)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a key must not be able to change the platform's settings: %d %s",
			recorder.Code, recorder.Body.String())
	}

	// The key holds developer, not admin, so it cannot issue itself another.
	if recorder := h.do(t, http.MethodPost, keysPath, `{"name":"second"}`, asKey); recorder.Code !=
		http.StatusForbidden {
		t.Fatalf("a developer key must not be able to issue keys: %d %s", recorder.Code, recorder.Body.String())
	}
}

// Creating writes both things: the credential at the issuer, and the grant
// that makes it useful.
func TestIssuingAKeyWritesTheGrantWithIt(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	directory := h.withDirectory()

	recorder := h.do(t, http.MethodPost, keysPath, `{"name":"`+ciKeyName+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	issued := decode[issuedKeyView](t, recorder)
	if issued.Key == "" {
		t.Fatal("the key value is answered exactly once, and this was that once")
	}
	if issued.Role != string(kitchenv1alpha1.AccessRoleDeveloper) {
		t.Fatalf("a key defaults to developer, got %q", issued.Role)
	}
	if issued.Subject != ciKeySubject || issued.Email != ciKeyEmail {
		t.Fatalf("want the machine account's identity, got %+v", issued)
	}

	// The grant is on the project, naming the machine account's subject.
	found := false
	for _, grant := range h.storedAccess(t, feedProject) {
		if grant.Subject == ciKeySubject {
			found = true
			if grant.Role != kitchenv1alpha1.AccessRoleDeveloper || grant.Email != ciKeyEmail {
				t.Fatalf("want a developer grant carrying the machine address, got %+v", grant)
			}
		}
	}
	if !found {
		t.Fatal("a key with no grant authenticates and can do nothing: the grant must be written with it")
	}
	if len(directory.keys[feedProject]) != 1 {
		t.Fatalf("want one key at the issuer, got %+v", directory.keys[feedProject])
	}

	// And no read ever answers with the value again.
	listed := decode[listBody[keyView]](t, h.do(t, http.MethodGet, keysPath, ""))
	if len(listed.Items) != 1 || listed.Items[0].Name != ciKeyName {
		t.Fatalf("want the key listed, got %+v", listed.Items)
	}
	if listed.Items[0].Role != string(kitchenv1alpha1.AccessRoleDeveloper) {
		t.Fatalf("the listing reports the grant the project holds, got %q", listed.Items[0].Role)
	}
	if body := h.do(t, http.MethodGet, keysPath, "").Body.String(); strings.Contains(body, issued.Key) {
		t.Fatalf("a key value must never be readable back: %s", body)
	}
}

// The failure the issue names, and the one this has to make impossible: a
// credential that exists at the issuer with nothing granted to it.
func TestAKeyWhoseGrantCannotBeWrittenDoesNotSurvive(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	directory := h.withDirectory()

	// The cluster refuses the grant, after the key has already been issued.
	refusePatches(t, h)

	recorder := h.do(t, http.MethodPost, keysPath, `{"name":"`+ciKeyName+`"}`)
	if recorder.Code == http.StatusCreated {
		t.Fatalf("a key whose grant did not land must not be reported as created: %s", recorder.Body.String())
	}
	if len(directory.keys[feedProject]) != 0 {
		t.Fatalf("the key must have been taken back at the issuer, got %+v", directory.keys[feedProject])
	}
	if len(directory.deleted) != 1 || directory.deleted[0] != ciKeyName {
		t.Fatalf("want the key revoked, got %v", directory.deleted)
	}
}

// And when it cannot be taken back either, the refusal says so rather than
// leaving a credential nobody knows about.
func TestAKeyThatCannotBeTakenBackIsReported(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	directory := h.withDirectory()
	directory.deleteErr = errors.New("the issuer is unreachable")

	refusePatches(t, h)

	recorder := h.do(t, http.MethodPost, keysPath, `{"name":"`+ciKeyName+`"}`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "could not be taken back") {
		t.Fatalf("the error must say a credential is lying around: %s", recorder.Body.String())
	}
}

// Revoking takes the grant with it, so `spec.access` does not accumulate
// subjects that no longer exist.
func TestRevokingAKeyRemovesItsGrant(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	directory := h.withDirectory()

	if recorder := h.do(t, http.MethodPost, keysPath, `{"name":"`+ciKeyName+`"}`); recorder.Code !=
		http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodDelete, keysPath+"/"+ciKeyName, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(directory.keys[feedProject]) != 0 {
		t.Fatalf("the key must be gone at the issuer, got %+v", directory.keys[feedProject])
	}
	for _, grant := range h.storedAccess(t, feedProject) {
		if grant.Subject == ciKeySubject {
			t.Fatalf("the grant must go with the key, still found %+v", grant)
		}
	}

	// A name the project has no key for is a plain not-found.
	if recorder := h.do(t, http.MethodDelete, keysPath+"/"+ciKeyName, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// Keys and people are one list, so a key has to read as one in it.
func TestAKeysGrantIsLegibleInTheMembersList(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	if recorder := h.do(t, http.MethodPost, keysPath, `{"name":"`+ciKeyName+`"}`); recorder.Code !=
		http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	var key *memberView
	for _, member := range h.members(t) {
		if member.Subject == ciKeySubject {
			found := member
			key = &found
		}
	}
	if key == nil {
		t.Fatal("a key is a member of the project, so it belongs in the members list")
	}
	if key.Kind != memberKindKey || key.Name != ciKeyName {
		t.Fatalf("want the entry to read as the key %q, got %+v", ciKeyName, *key)
	}
	if key.Email != ciKeyEmail {
		t.Fatalf("want the machine account's address beside the subject, got %+v", *key)
	}

	// A person's grant is unaffected: nothing about it says "key".
	for _, member := range h.members(t) {
		if member.Subject == testSubject && member.Kind != memberKindAccount {
			t.Fatalf("a person's grant must read as an account, got %+v", member)
		}
	}
}

// The one knob, and the API validates it.
func TestTheRoleAKeyIsGivenIsValidated(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	h.withDirectory()

	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"a key may read only", `{"name":"reader","role":"viewer"}`, http.StatusCreated},
		{"a key may not be an admin", `{"name":"boss","role":"admin"}`, http.StatusBadRequest},
		{"an unknown role is refused", `{"name":"odd","role":"deployer"}`, http.StatusBadRequest},
		{"a name that is not a label is refused", `{"name":"Nightly Build"}`, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, keysPath, c.body)
			if recorder.Code != c.want {
				t.Fatalf("want %d, got %d: %s", c.want, recorder.Code, recorder.Body.String())
			}
		})
	}

	// The same name twice is a conflict rather than a second credential
	// behind one grant, which would make "revoke that key" ambiguous.
	if recorder := h.do(t, http.MethodPost, keysPath, `{"name":"reader"}`); recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// An installation federated to an issuer of its own has no key endpoints, and
// says so rather than failing in a way that looks like a fault.
func TestAnIssuerThatIssuesNoKeysSaysSo(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleAdmin)
	directory := h.withDirectory()
	directory.keysErr = idp.ErrNoKeyDirectory

	recorder := h.do(t, http.MethodGet, keysPath, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// refusePatches makes every write to the cluster fail, which is how these
// tests get at what happens between the key being issued and the grant
// landing.
func refusePatches(t *testing.T, h *harness) {
	t.Helper()
	base, ok := h.server.Client.(client.WithWatch)
	if !ok {
		t.Fatalf("the harness's client cannot be intercepted: %T", h.server.Client)
	}
	h.server.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return errors.New("the api server said no")
		},
	})
}
