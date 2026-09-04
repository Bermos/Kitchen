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

package idp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `/kitchen` prefix is not published, and this is what keeps it that way.
//
// The prefix is the operator's alone: it enumerates every account, mints a CI
// key for any project and rewrites an OAuth client's redirect list — an
// account-takeover primitive against every application holding an oidcClient
// claim — and all of it answers to one header. The auth HTTPRoute sends
// `PathPrefix /` of the issuer's hostname at the auth Service, so for as long
// as one Service answered both the prefix was on the internet, and a leaked
// service key was a leaked service key from anywhere on earth.
//
// The fix is two listeners on two Services, only one of which a route names.
// That is a property of the chart, and it is invisible from Go: nothing in
// the operator fails if a later route grows a rule pointing at the private
// Service, and nothing fails if the private Service quietly starts fronting
// the published port. The failure would be silent in exactly the way the
// original was — everything works, and one more thing works than should.
//
// So the check is textual, over the templates themselves. The alternative is
// rendering the chart, which needs a helm binary this package cannot assume.
func TestThePrivateListenerIsNotPublished(t *testing.T) {
	templates := filepath.Join("..", "..", "charts", "kitchen", "templates")

	// No HTTPRoute may name the private Service. This is the whole rule.
	var routesChecked int
	err := filepath.Walk(templates, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Ext(path) != ".yaml" {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // a path from the walk of a fixed directory
		if err != nil {
			return err
		}
		text := string(body)
		if !strings.Contains(text, "kind: HTTPRoute") {
			return nil
		}
		routesChecked++
		if strings.Contains(text, "kitchen.authInternalFullname") {
			t.Errorf("%s is an HTTPRoute naming the identity provider's private Service: the /kitchen "+
				"prefix enumerates accounts, mints CI keys and rewrites OAuth redirect lists, and it is "+
				"served on that Service precisely so that nothing published reaches it", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the chart templates: %v", err)
	}
	if routesChecked == 0 {
		t.Fatal("no HTTPRoute templates were found: this test is checking nothing")
	}

	// The auth route still publishes the issuer, on the public Service's
	// port. A route that lost its backend would pass the check above by
	// serving nothing at all.
	route := read(t, filepath.Join(templates, "auth", "httproute.yaml"))
	if !strings.Contains(route, `name: {{ include "kitchen.authFullname" . }}`) ||
		!strings.Contains(route, "port: {{ .Values.auth.service.port }}") {
		t.Error("the auth HTTPRoute no longer sends the issuer's hostname at the public auth Service " +
			"and its port; the identity provider is not published")
	}

	// And the private Service fronts the private container port, not the
	// published one. Both Services selecting the same pods, one of them on
	// the wrong port, is the same hole with a second name.
	service := read(t, filepath.Join(templates, "auth", "service-internal.yaml"))
	if !strings.Contains(service, "targetPort: internal") {
		t.Error("the private auth Service does not front the container's `internal` port")
	}
	deployment := read(t, filepath.Join(templates, "auth", "deployment.yaml"))
	if !strings.Contains(deployment, "- name: internal\n              containerPort: {{ .Values.auth.internalPort }}") {
		t.Error("the auth container declares no `internal` port at .Values.auth.internalPort, so the " +
			"private Service fronts nothing")
	}
	if !strings.Contains(deployment, "- name: KITCHEN_AUTH_INTERNAL_PORT") {
		t.Error("the auth container is not told which port to serve the /kitchen prefix on")
	}
}

// TestTheOperatorIsToldWhereTheDirectoryIs: the address the operator reaches
// the prefix at is written by the chart and read by ConfigFromSecret, and the
// two spell the key the same way. Get that wrong and the operator falls back
// to the published listener, which now answers 404 — reported as
// ErrNoDirectory, which reads as "this issuer is federated" and is a long way
// from "the chart and the operator disagree about a secret key".
func TestTheOperatorIsToldWhereTheDirectoryIs(t *testing.T) {
	secret := read(t, filepath.Join("..", "..", "charts", "kitchen", "templates", "auth", "secret.yaml"))
	if !strings.Contains(secret, SecretKeyDirectoryURL+`: {{ include "kitchen.authDirectoryURL" . | quote }}`) {
		t.Errorf("the auth secret does not carry %q as the chart's kitchen.authDirectoryURL", SecretKeyDirectoryURL)
	}

	helpers := read(t, filepath.Join("..", "..", "charts", "kitchen", "templates", "_helpers.tpl"))
	directory := define(helpers, "kitchen.authDirectoryURL")
	if directory == "" {
		t.Fatal(`_helpers.tpl defines no "kitchen.authDirectoryURL"`)
	}
	if !strings.Contains(directory, "kitchen.authInternalFullname") {
		t.Error("kitchen.authDirectoryURL does not point at the private Service, so the operator would " +
			"reach the /kitchen prefix through the one the Gateway publishes")
	}
}

// define returns the body of one named template in _helpers.tpl.
func define(helpers, name string) string {
	start := strings.Index(helpers, `{{- define "`+name+`" -}}`)
	if start < 0 {
		return ""
	}
	rest := helpers[start:]
	end := strings.Index(rest, "{{- end }}")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}
