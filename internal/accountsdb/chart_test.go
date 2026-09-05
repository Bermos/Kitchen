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

package accountsdb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The accounts database's half of #382 is a property of the chart, and it is
// invisible from Go: nothing in this package fails if the StatefulSet quietly
// goes back to serving plaintext, or if the identity provider's DSN loses its
// sslmode. The failure would be silent in exactly the way the finding was —
// everything keeps working, and one more thing works than should.
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

// `ssl = on` alone only *offers* TLS. The stock image's rules end in
// `host all all all scram-sha-256`, which serves a plaintext client exactly as
// happily as an encrypted one — so the rules have to be replaced, not added
// to, and `hostssl` with no `host` under it is what refuses the first.
func TestTheAccountsDatabaseRefusesPlaintextRatherThanMerelyOfferingTLS(t *testing.T) {
	hba := chartFile(t, "templates", "postgres", "tls-configmap.yaml")

	if !strings.Contains(hba, "hostssl all") {
		t.Error("the host-based rules admit no encrypted connection at all")
	}
	for _, line := range strings.Split(hba, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "host" {
			t.Errorf("the host-based rules still carry a plaintext rule (%q): every client "+
				"would keep working, which is how this goes unnoticed", strings.TrimSpace(line))
		}
	}
	// The socket is the one connection that is not on the wire, and the
	// image's entrypoint initialises the cluster over it before there is a
	// password to give — as do all three of the pod's probes.
	if !strings.Contains(hba, "local   all") {
		t.Error("the host-based rules do not admit the Unix socket, so the image's entrypoint " +
			"cannot initialise the cluster and pg_isready never answers")
	}
}

// The rules only apply if the server is told to read them, and the file it
// reads by default belongs to the data volume — where an installation that
// already exists would keep the rules it was created with for ever.
func TestTheServerIsToldToServeTLSAndWhereToReadItsRulesFrom(t *testing.T) {
	statefulSet := chartFile(t, "templates", "postgres", "statefulset.yaml")

	for _, argument := range []string{"ssl=on", "ssl_cert_file=", "ssl_key_file=", "hba_file="} {
		if !strings.Contains(statefulSet, argument) {
			t.Errorf("the server is started without %s", argument)
		}
	}
	if !strings.Contains(statefulSet, "- postgres\n") {
		t.Error("the arguments replace the image's command without repeating it, so the " +
			"entrypoint never starts a server at all")
	}
	// Postgres refuses to start on a private key its group can write or the
	// world can read. A Secret's files arrive owned by root and by the pod's
	// fsGroup, and the kubelet widens a read-only volume to at least 0440
	// where there is one — so 0600 here is 0640 root:fsGroup on the node,
	// which is the mode the server accepts from a root-owned file.
	if !strings.Contains(statefulSet, "defaultMode: 0600") {
		t.Error("the certificate is mounted world- or group-writable, and Postgres refuses to " +
			"start on a key like that")
	}
}

// What every client is told. The DSN is the one place it is said, and all four
// clients — the identity provider, the operator, a backup run and a restore —
// read it from this same secret.
func TestEveryClientIsToldToVerifyTheAccountsDatabase(t *testing.T) {
	secret := chartFile(t, "templates", "postgres", "secret.yaml")

	if !strings.Contains(secret, "sslmode=%s") || !strings.Contains(secret, "sslrootcert=%s") {
		t.Error("the DSN carries no sslmode or no CA, so the identity provider connects " +
			"unencrypted or encrypts without verifying")
	}
	if !strings.Contains(secret, `include "kitchen.postgresTLSSecretName"`) {
		t.Error("nothing asks the operator to issue the database a certificate, so its pod " +
			"waits forever for a Secret nothing writes")
	}

	// `verify-full` and nothing weaker, because the two drivers in front of
	// this database do not agree on anything weaker: node-postgres verifies
	// on `require`, libpq does not.
	helpers := chartFile(t, "templates", "_helpers.tpl")
	if !strings.Contains(helpers, `{{- "verify-full" }}`) {
		t.Error("the bundled database is not asked for verify-full, which is the only mode " +
			"that means the same thing to the identity provider and to the operator")
	}
}

// A verified connection needs the CA in the pod, and the pod holding until it
// is there. An optional mount would hand node-postgres an empty directory,
// which is a crash loop whose backoff doubles to five minutes — far longer
// than the seconds it is waiting for.
func TestTheIdentityProviderWaitsForTheCARatherThanStartingWithoutIt(t *testing.T) {
	deployment := chartFile(t, "templates", "auth", "deployment.yaml")

	if !strings.Contains(deployment, `include "kitchen.internalCAConfigMapName"`) {
		t.Fatal("the identity provider mounts no CA bundle, so it cannot verify the database " +
			"it keeps every session in")
	}
	mount := deployment[strings.Index(deployment, `include "kitchen.internalCAConfigMapName"`):]
	if strings.Contains(mount, "optional: true") {
		t.Error("the CA mount is optional, so the pod starts with an empty directory and " +
			"crash-loops instead of waiting the seconds the ConfigMap takes to appear")
	}
}
