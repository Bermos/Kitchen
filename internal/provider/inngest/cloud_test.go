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

package inngest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/provider/inngest/inngesttest"
)

func cloudAgainstFake(t *testing.T) (*Cloud, *inngesttest.CloudServer) {
	t.Helper()
	fake := inngesttest.NewCloudServer()
	t.Cleanup(fake.Close)
	return &Cloud{APIURL: fake.URL(), Token: "sk-inn-api-test"}, fake
}

// Provisioning creates nothing: it reads production's keys, and the binding
// leaves INNGEST_ENV and INNGEST_BASE_URL empty for Cloud.
func TestProvisionReadsProductionsKeys(t *testing.T) {
	cloud, fake := cloudAgainstFake(t)

	instance, err := cloud.Provision(context.Background(), Requirements{App: "shop-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "shop-worker" || instance.Environment != DefaultEnvironment {
		t.Fatalf("unexpected instance: %+v", instance)
	}
	want := Binding{EventKey: inngesttest.ProductionEventKey, SigningKey: inngesttest.ProductionSigningKey}
	if instance.Binding != want {
		t.Fatalf("unexpected binding: %+v", instance.Binding)
	}
	if auth := fake.LastAuthorization(); auth != "Bearer sk-inn-api-test" {
		t.Fatalf("the API key did not reach the API: %q", auth)
	}
	if env := fake.LastEnvironment(); env != DefaultEnvironment {
		t.Fatalf("the keys were not asked for by environment: %q", env)
	}
	data := instance.Binding.SecretData()
	for _, key := range []string{KeyEventKey, KeySigningKey, KeyEnv, KeyBaseURL} {
		if _, ok := data[key]; !ok {
			t.Errorf("the binding Secret lacks %s: a variable reading it would fail to resolve", key)
		}
	}
	if string(data[KeyBaseURL]) != "" {
		t.Error("INNGEST_BASE_URL must stay empty on Cloud: the SDKs use it for two hosts at once")
	}
}

func TestProvisionReadsACustomEnvironmentsOwnKeys(t *testing.T) {
	cloud, fake := cloudAgainstFake(t)
	fake.AddEnvironment("staging", "signkey-staging", "evkey-staging")

	instance, err := cloud.Provision(context.Background(), Requirements{App: "shop-worker", Environment: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if instance.Binding.SigningKey != "signkey-staging" || instance.Binding.EventKey != "evkey-staging" {
		t.Fatalf("staging's own keys were not read: %+v", instance.Binding)
	}
	if instance.Binding.Env != "" {
		t.Fatal("an environment's own keys select it; INNGEST_ENV is for the shared branch keys alone")
	}
}

// The API cannot mint an event key, so an environment without one is a
// refusal that says where to create it, rather than a binding with a hole.
func TestProvisionRefusesAnEnvironmentWithoutAnEventKey(t *testing.T) {
	cloud, fake := cloudAgainstFake(t)
	fake.RemoveEventKeys("production")

	_, err := cloud.Provision(context.Background(), Requirements{App: "shop-worker"})
	if !errors.Is(err, ErrUnsatisfiable) {
		t.Fatalf("want ErrUnsatisfiable, got %v", err)
	}
	if !strings.Contains(err.Error(), "dashboard") {
		t.Fatalf("the refusal does not say where to create the key: %q", err.Error())
	}
}

func TestProvisionRefusesServeMode(t *testing.T) {
	cloud, _ := cloudAgainstFake(t)
	_, err := cloud.Provision(context.Background(), Requirements{App: "shop-worker", Mode: "serve"})
	if !errors.Is(err, ErrUnsatisfiable) || !strings.Contains(err.Error(), "login page") {
		t.Fatalf("serve mode must be refused with the reason, got %v", err)
	}
}

func TestProvisionRefusesAnEnvironmentTheKeyCannotRead(t *testing.T) {
	cloud, _ := cloudAgainstFake(t)
	_, err := cloud.Provision(context.Background(), Requirements{App: "shop-worker", Environment: "nowhere"})
	if err == nil || strings.Contains(err.Error(), "sk-inn-api-test") {
		t.Fatalf("want an error that does not leak the key, got %v", err)
	}
}

// A preview's branch environment: created by name, found again by name,
// unarchived when Inngest's auto-archive got there first, and bound to the
// account's shared branch keys plus INNGEST_ENV.
func TestBranchLifecycle(t *testing.T) {
	cloud, fake := cloudAgainstFake(t)
	ctx := context.Background()

	branch, err := cloud.CreateBranch(ctx, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	env := fake.EnvNamed("shop-pr-7")
	if env == nil || env.ID != branch.ID {
		t.Fatalf("no branch environment was created, or the ID is not its: %+v %+v", env, branch)
	}
	want := Binding{EventKey: inngesttest.BranchEventKey, SigningKey: inngesttest.BranchSigningKey, Env: "shop-pr-7"}
	if branch.Binding != want {
		t.Fatalf("unexpected branch binding: %+v", branch.Binding)
	}

	again, err := cloud.CreateBranch(ctx, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != branch.ID {
		t.Fatalf("a second CreateBranch made a second environment: %q then %q", branch.ID, again.ID)
	}

	if err := cloud.DeleteBranch(ctx, branch.ID); err != nil {
		t.Fatal(err)
	}
	if env := fake.EnvNamed("shop-pr-7"); env == nil || !env.Archived {
		t.Fatalf("deleting a branch archives its environment rather than deleting it: %+v", env)
	}
	// Archiving an archived environment, or one that is gone, is not an error.
	if err := cloud.DeleteBranch(ctx, branch.ID); err != nil {
		t.Fatal(err)
	}
	if err := cloud.DeleteBranch(ctx, "env-nowhere"); err != nil {
		t.Fatal(err)
	}

	// A preview reopened — or one Inngest auto-archived after three quiet
	// days — gets its environment back rather than a second one.
	back, err := cloud.CreateBranch(ctx, "shop-pr-7")
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != branch.ID {
		t.Fatalf("an archived environment was not found again: %q then %q", branch.ID, back.ID)
	}
	if env := fake.EnvNamed("shop-pr-7"); env.Archived {
		t.Fatal("the environment was found but not unarchived")
	}
}

func TestAppReportsWhetherAWorkerHasConnected(t *testing.T) {
	cloud, fake := cloudAgainstFake(t)
	ctx := context.Background()

	app, err := cloud.App(ctx, "production", "shop-worker")
	if err != nil {
		t.Fatal(err)
	}
	if app.Found {
		t.Fatal("an app no worker has connected as must not be found")
	}

	fake.RegisterApp("production", "shop-worker", "CONNECT", 3)
	app, err = cloud.App(ctx, "production", "shop-worker")
	if err != nil {
		t.Fatal(err)
	}
	if !app.Found || app.Method != "CONNECT" || app.Functions != 3 || app.SDK != "typescript 3.22.0" {
		t.Fatalf("unexpected app report: %+v", app)
	}
	if env := fake.LastEnvironment(); env != "production" {
		t.Fatalf("the app was not asked for by environment: %q", env)
	}
}

func TestProviderErrorsCarryTheAPIsDiagnostic(t *testing.T) {
	cloud, fake := cloudAgainstFake(t)
	fake.FailWith("keys are locked")

	_, err := cloud.Provision(context.Background(), Requirements{App: "shop-worker"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "keys are locked") {
		t.Fatalf("the API's own words are missing from %q", err.Error())
	}
	if strings.Contains(err.Error(), "sk-inn-api-test") {
		t.Fatal("the error leaks the credential")
	}
}

func TestDefaultBuildsCloudFromTheConnection(t *testing.T) {
	conn := connectionNamed(ProviderCloud, `{"apiUrl": "http://fake.local/v2"}`)
	provisioner, err := Default(Options{Connection: conn, Token: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	cloud, ok := provisioner.(*Cloud)
	if !ok || cloud.APIURL != "http://fake.local/v2" || cloud.Token != "sk" {
		t.Fatalf("unexpected provisioner %#v", provisioner)
	}
	if provisioner, err := Default(Options{Connection: connectionNamed(ProviderCloud, "")}); err != nil ||
		provisioner.(*Cloud).APIURL != DefaultCloudAPIURL {
		t.Fatalf("the default API URL must be Inngest Cloud's: %v %#v", err, provisioner)
	}
	if _, err := Default(Options{Connection: connectionNamed("selfhosted", "")}); !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("a provider nothing implements must be refused, got %v", err)
	}
}
