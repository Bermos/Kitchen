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

package platformhost

import (
	"strings"
	"testing"
)

func TestAReservedNameIsRefusedAndSaysWhichHostnameIsTaken(t *testing.T) {
	for _, name := range Reserved() {
		err := CheckProjectName(name, "apps.example.com")
		if err == nil {
			t.Fatalf("%q is a hostname the platform serves and was accepted as a project name", name)
		}
		// The refusal is only useful if it names the address that is taken:
		// "auth is reserved" explains nothing, "auth.apps.example.com is the
		// identity provider" explains all of it.
		if want := name + ".apps.example.com"; !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal of %q does not name %s: %s", name, want, err)
		}
	}
}

func TestAReservedNameIsRefusedWithoutABaseDomainToNameItWith(t *testing.T) {
	err := CheckProjectName("registry", "")
	if err == nil {
		t.Fatal("the rule does not depend on the installation having a base domain")
	}
	if !strings.Contains(err.Error(), "registry.<the platform's base domain>") {
		t.Errorf("the refusal should still say where the collision would be: %s", err)
	}
}

// Project `shop-pr-7` publishes shop-pr-7.<base>, which is also where project
// `shop` publishes pull request 7. Neither hostname is reserved and both are
// generated, so nothing downstream can tell them apart.
func TestAPreviewShapedNameIsRefused(t *testing.T) {
	for _, name := range []string{"shop-pr-7", "shop-pr-0", "a-pr-1", "shop-pr-12345", "a-pr-1-pr-2"} {
		if err := CheckProjectName(name, "apps.example.com"); err == nil {
			t.Errorf("%q collides with another project's preview and was accepted", name)
		}
	}
}

func TestAnOrdinaryNameIsAccepted(t *testing.T) {
	// The near misses matter as much as the plain ones: the rule has to be
	// narrow enough that it refuses collisions and nothing else.
	for _, name := range []string{
		"shop", "blog", "kitchen-sink", "auth-service", "registry2", "previews-ui",
		"pr-7",           // no project in front of it, so it collides with nothing
		"shop-pr",        // no number
		"shop-pr-",       // still no number
		"shop-pr-x",      // not a number
		"shop-pr-1-beta", // the number is not the end of the name
	} {
		if err := CheckProjectName(name, "apps.example.com"); err != nil {
			t.Errorf("%q is an ordinary name and was refused: %s", name, err)
		}
	}
}

func TestReservedHostsAreTheReservedLabelsUnderTheBaseDomain(t *testing.T) {
	hosts := Hosts("apps.example.com")
	if len(hosts) != len(Reserved()) {
		t.Fatalf("every reserved label is a hostname: %v against %v", hosts, Reserved())
	}
	for _, host := range hosts {
		if !IsReservedHost(host, "apps.example.com") {
			t.Errorf("%s is one of the platform's own hostnames and was not recognised", host)
		}
	}
	for _, host := range []string{
		"shop.apps.example.com",
		"auth.example.com",
		"auth.apps.example.com.evil.test",
		"apps.example.com",
	} {
		if IsReservedHost(host, "apps.example.com") {
			t.Errorf("%s is not one of the platform's own hostnames", host)
		}
	}
	// Hostnames are case-insensitive and may carry a port; the answer must
	// not depend on either.
	if !IsReservedHost("AUTH.Apps.Example.Com:8443", "apps.example.com") {
		t.Error("a hostname with a port and mixed case is still the same hostname")
	}
	if Hosts("") != nil || Host(API, "") != "" {
		t.Error("an installation with no base domain publishes nothing")
	}
}

func TestEveryReservedLabelIsUsable(t *testing.T) {
	// The labels go into hostnames, so each has to be one — and each has to
	// be distinct, or two of the platform's own names would collide with each
	// other.
	seen := map[string]bool{}
	for _, label := range Reserved() {
		if label == "" || label != strings.ToLower(label) || strings.Contains(label, ".") {
			t.Errorf("%q is not a hostname label", label)
		}
		if seen[label] {
			t.Errorf("%q is reserved twice", label)
		}
		seen[label] = true
		if !IsReserved(strings.ToUpper(label)) {
			t.Errorf("%q should be recognised whatever its case", label)
		}
	}
}
