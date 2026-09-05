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

package clickhouse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The store's half of #382 is a property of the chart, and it is invisible
// from Go: nothing in this package fails if the ClickHouse StatefulSet quietly
// keeps its plaintext listeners, or if the telemetry agent is pointed back at
// the native port without `secure=true`. The failure would be silent in
// exactly the way the finding was — everything works, and one more thing works
// than should.
//
// So the checks are textual, over the templates themselves. Rendering them
// would need a helm binary this package cannot assume; `.github/workflows/helm.yml`
// does that half, against a real render and a real cluster.

func chartFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "charts", "kitchen"}, parts...)...)
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// Adding the secure listeners is the easy half and it protects nothing on its
// own: ClickHouse's own config.xml declares http_port 8123 and tcp_port 9000,
// so a file that only added 8443 and 9440 would leave both plaintext ports
// answering beside them — and every client would keep working, which is how
// this would go unnoticed.
func TestTheStoresPlaintextListenersAreRemovedRatherThanMovedAside(t *testing.T) {
	config := chartFile(t, "templates", "clickhouse", "tls-configmap.yaml")

	for _, port := range []string{"http_port", "tcp_port"} {
		if !strings.Contains(config, "<"+port+` remove="remove"/>`) {
			t.Errorf("the TLS configuration does not remove <%s>; the store still answers in "+
				"plaintext on it, which is the whole of the finding", port)
		}
	}
	if !strings.Contains(config, "<https_port>") || !strings.Contains(config, "<tcp_port_secure>") {
		t.Error("the TLS configuration declares no secure listener, so the store answers nothing")
	}
	if !strings.Contains(config, "<certificateFile>") || !strings.Contains(config, "<privateKeyFile>") {
		t.Error("the TLS configuration names no certificate, so ClickHouse refuses to serve TLS at all")
	}

	// The file is mounted one file at a time. A whole-directory mount over
	// config.d hides the image's own docker_related_config.xml, which is
	// where `listen_host` comes from — and a ClickHouse listening on
	// localhost alone is a store nothing in the cluster can reach, on a pod
	// whose every probe still passes.
	statefulSet := chartFile(t, "templates", "clickhouse", "statefulset.yaml")
	if !strings.Contains(statefulSet, "subPath: kitchen-tls.xml") {
		t.Error("the TLS configuration is not mounted by subPath; a directory mount over " +
			"config.d hides the image's listen_host and the store binds localhost only")
	}
	if !strings.Contains(statefulSet, "scheme: HTTPS") {
		t.Error("the store's probes still ask over plain HTTP, so a pod serving TLS never " +
			"becomes ready")
	}
}

// What the platform's own clients are told about the store. The scheme is what
// stops a client dialling plaintext; the CA bundle is the whole of the
// difference between an encrypted connection and a verified one.
func TestTheConnectionSecretSaysHowTheStoreIsVerified(t *testing.T) {
	secret := chartFile(t, "templates", "clickhouse", "secret.yaml")

	for _, key := range []string{"scheme:", "caFile:", "certificateSecret:"} {
		if !strings.Contains(secret, key) {
			t.Errorf("the connection secret carries no %s key, so every client of the store "+
				"falls back to the plaintext defaults", strings.TrimSuffix(key, ":"))
		}
	}
	if !strings.Contains(secret, `ternary "?secure=true" "" $secure`) {
		t.Error("the secret's DSN does not ask for TLS when the store serves it; anything " +
			"reaching for that string connects in the clear")
	}
}

// The telemetry agent writes more into the store than everything else put
// together, over the native protocol rather than the HTTP interface — so it is
// the one client whose TLS is configured somewhere other than
// internal/clickhouse, and the one that could be left behind by a change to
// this package.
func TestTheTelemetryAgentVerifiesTheStoreItWritesTo(t *testing.T) {
	whole := chartFile(t, "templates", "collector", "configmap.yaml")
	// The exporter block alone. The kubelet receiver above it skips
	// verification on purpose — the kubelet serves a certificate for its node
	// name and is dialled by address — and that is not this.
	exporters := strings.Index(whole, "\n    exporters:")
	if exporters < 0 {
		t.Fatal("the agent's configuration has no exporters block")
	}
	service := strings.Index(whole, "\n    service:")
	if service < exporters {
		t.Fatal("the agent's configuration has no service block after its exporters")
	}
	config := whole[exporters:service]

	if !strings.Contains(config, `ternary "&secure=true" "" $secure`) {
		t.Error("the agent's exporter endpoint never asks for TLS: the exporter infers it from " +
			"a https:// scheme alone, which on this exporter means the HTTP interface")
	}
	if !strings.Contains(config, "ca_file:") {
		t.Error("the agent's exporter names no CA, so its connection is encrypted and " +
			"unverified — `require`, where the platform's own components can carry the " +
			"CA and get `verify-full`")
	}
	// The spelling matters more than it looks. `server_name` is what the
	// setting reads as, and it is not a key configtls.ClientConfig has — the
	// tag is `server_name_override` — and the collector refuses a
	// configuration carrying a key it does not know rather than ignoring it.
	// So the obvious spelling is not a looser check, it is a collector that
	// exits at startup on every node, which is what it did.
	if !strings.Contains(config, "server_name_override:") {
		t.Error("the agent's exporter does not set server_name_override, so nothing pins " +
			"the name its certificate is checked against")
	}
	if strings.Contains(config, "\n          server_name:") {
		t.Error("the agent's exporter sets `server_name`, which configtls has no key for; " +
			"the collector refuses the whole configuration and never starts")
	}
	// The settings themselves, not the words: the block says in prose that
	// neither is there, and a test that matched the prose would fail on the
	// comment explaining why.
	for _, weakening := range []string{"insecure_skip_verify: true", "insecure: true"} {
		if strings.Contains(config, weakening) {
			t.Errorf("the agent's exporter carries %q; there is nothing about the platform's "+
				"own CA that a client of it cannot verify", weakening)
		}
	}

	daemonSet := chartFile(t, "templates", "collector", "daemonset.yaml")

	if !strings.Contains(daemonSet, "internalCAMountPath") {
		t.Error("the agent does not mount the CA bundle its exporter reads, so every export " +
			"fails on a file that is not there")
	}
	// And it is *not* optional, which is the opposite of the obvious answer:
	// an optional mount that is empty makes the exporter fail to load a CA
	// file that is not there, and the collector exits — a CrashLoopBackOff
	// whose delay doubles to five minutes. A required mount waits in
	// ContainerCreating and starts within seconds of the ConfigMap appearing.
	if strings.Contains(daemonSet, "internalCAConfigMapName") &&
		strings.Contains(daemonSet, "optional: true") {
		t.Error("the agent's CA mount is optional: on a first install that is an empty " +
			"directory, an exporter that cannot start, and a crash loop that backs off " +
			"long past the moment the CA arrives")
	}
}
