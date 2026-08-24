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

package provider

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func connection(providerName string, config string) *kitchenv1alpha1.Connection {
	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "conn", Namespace: "kitchen-system"},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             providerName,
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "creds"},
		},
	}
	if config != "" {
		conn.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
	}
	return conn
}

func tokenSecret(token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "kitchen-system"},
		Data:       map[string][]byte{"token": []byte(token)},
	}
}

const testToken = "tok"

func TestDefaultResolvesImplementedProviders(t *testing.T) {
	probe, err := Default(connection("github", `{"apiUrl": "https://ghe.internal/api/v3"}`), tokenSecret(testToken))
	if err != nil {
		t.Fatal(err)
	}
	gh, ok := probe.(*GitHubProbe)
	if !ok || gh.APIURL != "https://ghe.internal/api/v3" || gh.Token != testToken {
		t.Errorf("unexpected github probe %#v", probe)
	}

	probe, err = Default(connection("gitlab", `{"apiUrl": "https://gitlab.internal/api/v4"}`), tokenSecret(testToken))
	if err != nil {
		t.Fatal(err)
	}
	gl, ok := probe.(*GitLabProbe)
	if !ok || gl.APIURL != "https://gitlab.internal/api/v4" || gl.Token != testToken {
		t.Errorf("unexpected gitlab probe %#v", probe)
	}

	probe, err = Default(connection("gitea", `{"apiUrl": "https://git.internal/api/v1"}`), tokenSecret(testToken))
	if err != nil {
		t.Fatal(err)
	}
	gt, ok := probe.(*GiteaProbe)
	if !ok || gt.APIURL != "https://git.internal/api/v1" || gt.Token != testToken {
		t.Errorf("unexpected gitea probe %#v", probe)
	}

	probe, err = Default(connection("neon", ""), tokenSecret("key"))
	if err != nil {
		t.Fatal(err)
	}
	neon, ok := probe.(*NeonProbe)
	if !ok || neon.APIURL != "https://console.neon.tech/api/v2" || neon.Token != "key" {
		t.Errorf("unexpected neon probe %#v", probe)
	}
}

func TestDefaultRefusesUnimplementedProviders(t *testing.T) {
	for _, name := range []string{"bitbucket", "svn"} {
		_, err := Default(connection(name, ""), tokenSecret(testToken))
		if !errors.Is(err, ErrNotImplemented) {
			t.Errorf("expected ErrNotImplemented for %s, got %v", name, err)
		}
	}
}

func TestDefaultRejectsMissingToken(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "creds"}}
	_, err := Default(connection("github", ""), secret)
	if err == nil || !strings.Contains(err.Error(), `no "token" key`) {
		t.Errorf("expected a missing-token error, got %v", err)
	}
}

func TestRegistryProbeFromDockerConfig(t *testing.T) {
	dockerConfig := `{"auths": {"harbor.example.com": {"username": "` + probeUser + `", "password": "` + probePassword + `"}}}`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(dockerConfig)},
	}
	probe, err := Default(connection("dockerRegistry", `{"url": "harbor.example.com/kitchen"}`), secret)
	if err != nil {
		t.Fatal(err)
	}
	registry, ok := probe.(*RegistryProbe)
	if !ok || registry.BaseURL != "https://harbor.example.com" ||
		registry.Username != probeUser || registry.Password != probePassword {
		t.Errorf("unexpected registry probe %#v", probe)
	}
}

func TestRegistryProbeDecodesAuthEntry(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte(probeUser + ":" + probePassword))
	dockerConfig := `{"auths": {"harbor.example.com": {"auth": "` + auth + `"}}}`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(dockerConfig)},
	}
	probe, err := Default(connection("dockerRegistry", `{"url": "https://harbor.example.com"}`), secret)
	if err != nil {
		t.Fatal(err)
	}
	registry := probe.(*RegistryProbe)
	if registry.Username != probeUser || registry.Password != probePassword {
		t.Errorf("expected the auth entry to decode, got %#v", registry)
	}
}

func TestRegistryProbeNeedsURLAndMatchingAuth(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds"},
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths": {}}`)},
	}
	if _, err := Default(connection("dockerRegistry", ""), secret); err == nil ||
		!strings.Contains(err.Error(), "config.url") {
		t.Errorf("expected a missing-url error, got %v", err)
	}
	if _, err := Default(connection("dockerRegistry", `{"url": "harbor.example.com"}`), secret); err == nil ||
		!strings.Contains(err.Error(), "no auth entry") {
		t.Errorf("expected a missing-auth error, got %v", err)
	}
}

func TestRegistryResolvesThePrefixIntoItsParts(t *testing.T) {
	cases := map[string]struct{ url, prefix, server, baseURL string }{
		"a prefix under a host": {
			"harbor.example.com/kitchen", "harbor.example.com/kitchen", "harbor.example.com", "https://harbor.example.com",
		},
		"a bare host": {
			"harbor.example.com", "harbor.example.com", "harbor.example.com", "https://harbor.example.com",
		},
		"an explicit scheme is stripped from the prefix but kept for the probe": {
			"https://harbor.example.com/kitchen", "harbor.example.com/kitchen", "harbor.example.com", "https://harbor.example.com",
		},
		"a plaintext registry keeps its scheme": {
			"http://registry.internal:5000/kitchen", "registry.internal:5000/kitchen", "registry.internal:5000", "http://registry.internal:5000",
		},
		"a trailing slash is not a path": {
			"harbor.example.com/kitchen/", "harbor.example.com/kitchen", "harbor.example.com", "https://harbor.example.com",
		},
	}
	for name, tc := range cases {
		target, err := Registry(connection("dockerRegistry", `{"url": "`+tc.url+`"}`))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if target.Prefix != tc.prefix || target.Server != tc.server || target.BaseURL != tc.baseURL {
			t.Errorf("%s: unexpected target %#v", name, target)
		}
	}
}

func TestRegistryRefusesWhatItCannotResolve(t *testing.T) {
	if _, err := Registry(connection("dockerRegistry", "")); err == nil ||
		!strings.Contains(err.Error(), "config.url") {
		t.Errorf("expected a missing-url error, got %v", err)
	}
	if _, err := Registry(connection("dockerRegistry", `{"url": "https://"}`)); err == nil ||
		!strings.Contains(err.Error(), "no host") {
		t.Errorf("expected a missing-host error, got %v", err)
	}
	// The capability matcher should never hand one of these over, and a build
	// pushing nowhere would be harder to read than one that says so.
	if _, err := Registry(connection("github", "")); err == nil ||
		!strings.Contains(err.Error(), "stores no images") {
		t.Errorf("expected a wrong-provider error, got %v", err)
	}
}

func TestCapabilitiesMatchWhatThePlatformImplements(t *testing.T) {
	if got := Capabilities("github"); len(got) != 2 ||
		got[0] != kitchenv1alpha1.CapabilityGitSource || got[1] != kitchenv1alpha1.CapabilityStatusChecks {
		t.Errorf("unexpected github capabilities %v", got)
	}
	if got := Capabilities("dockerRegistry"); len(got) != 1 || got[0] != kitchenv1alpha1.CapabilityImageStore {
		t.Errorf("unexpected dockerRegistry capabilities %v", got)
	}
	if got := Capabilities("neon"); len(got) != 1 || got[0] != kitchenv1alpha1.CapabilityDatabase {
		t.Errorf("unexpected neon capabilities %v", got)
	}
	// Every forge is both halves. What differs between them is the
	// deployment record, which is not a capability — it is a narrower
	// interface the operator asks each provider for.
	for _, name := range []string{"gitlab", "gitea"} {
		if got := Capabilities(name); len(got) != 2 ||
			got[0] != kitchenv1alpha1.CapabilityGitSource || got[1] != kitchenv1alpha1.CapabilityStatusChecks {
			t.Errorf("unexpected %s capabilities %v", name, got)
		}
	}
}
