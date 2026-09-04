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

package controller

import (
	"os"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// What the manager is allowed to do, which no other test in this package can
// see.
//
// Every case here runs against an envtest API server as an administrator, so
// a reconciler may delete anything and a verb the ClusterRole never granted
// costs nothing until the operator is in a real cluster. There it is a
// reconcile that ends on `is forbidden` before it writes a Deployment, on
// every pass, for every environment — the environment's ConfigMap of plain
// configuration files was deleted whenever a release declared no plain file,
// which is most of them, and the marker said only `create`.
//
// So this reads the generated role instead. It is deliberately not a list of
// every grant: what it holds is the removals, because a missing `create` is
// caught by the first cluster test to deploy anything and a missing `delete`
// hides behind an object that happens not to exist yet.
func TestTheManagerMayRemoveTheObjectsItsReconcilersDeleteByName(t *testing.T) {
	rules := managerRules(t)

	// Each of these is deleted by name by a reconciler in this package: an
	// environment's configuration files when its release stops declaring one
	// and when the environment goes, and a project's mirrored secrets when
	// their source goes and when the project does.
	for _, object := range []struct{ group, resource, why string }{
		{"", "configmaps", "an environment's plain configuration files"},
		{"", "secrets", "a project's mirrored secrets and secret files"},
	} {
		if !grants(rules, object.group, object.resource, "delete") {
			t.Errorf("the manager cannot delete %q: %s is removed by name, so the "+
				"reconciler ends every pass on a forbidden delete",
				object.resource, object.why)
		}
	}
}

// managerRules reads the role controller-gen generates from the markers in
// this package.
func managerRules(t *testing.T) []rbacv1.PolicyRule {
	t.Helper()
	raw, err := os.ReadFile("../../config/rbac/role.yaml")
	if err != nil {
		t.Fatalf("reading the manager's role: %v", err)
	}
	role := &rbacv1.ClusterRole{}
	if err := yaml.Unmarshal(raw, role); err != nil {
		t.Fatalf("parsing the manager's role: %v", err)
	}
	if len(role.Rules) == 0 {
		t.Fatal("the manager's role has no rules at all")
	}
	return role.Rules
}

// grants answers whether the role permits one verb on one resource.
func grants(rules []rbacv1.PolicyRule, group, resource, verb string) bool {
	has := func(list []string, want string) bool {
		return slices.Contains(list, want) || slices.Contains(list, "*")
	}
	for _, rule := range rules {
		if has(rule.APIGroups, group) && has(rule.Resources, resource) && has(rule.Verbs, verb) {
			return true
		}
	}
	return false
}
