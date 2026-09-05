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
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func secretWith(data map[string]string) *corev1.Secret {
	secret := &corev1.Secret{Data: map[string][]byte{}}
	secret.Namespace, secret.Name = "kitchen-system", "kitchen-postgres"
	for key, value := range data {
		secret.Data[key] = []byte(value)
	}
	return secret
}

// The chart writes `dsn`, and that is what every client reads. A secret
// somebody wrote by hand for an external Postgres may carry the pieces
// instead — and a connection assembled from those must not end up less
// encrypted than the one the chart writes, which is the whole of #382 for
// this store.
func TestADSNAssembledFromPiecesCarriesWhatTheSecretAsksOfTheServer(t *testing.T) {
	dsn, err := DSNFromSecret(secretWith(map[string]string{
		SecretKeyHost:     "postgres.databases.svc",
		SecretKeyDatabase: "kitchen_auth",
		SecretKeyUsername: "kitchen",
		SecretKeyPassword: "hunter2",
		SecretKeySSLMode:  SSLModeVerifyFull,
		SecretKeyCAFile:   "/etc/kitchen/internal-ca/ca.crt",
	}))
	if err != nil {
		t.Fatalf("assembling a DSN: %v", err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parsing %q: %v", dsn, err)
	}
	if got := parsed.Query().Get("sslmode"); got != SSLModeVerifyFull {
		t.Errorf("sslmode is %q, want %q: the connection would not be encrypted at all",
			got, SSLModeVerifyFull)
	}
	if got := parsed.Query().Get("sslrootcert"); got != "/etc/kitchen/internal-ca/ca.crt" {
		t.Errorf("sslrootcert is %q: the connection would encrypt without verifying", got)
	}
}

// And says nothing where the secret says nothing: an external database nobody
// has configured TLS for is reached the way it always was, rather than by a
// mode this package invented.
func TestADSNAssembledFromPiecesInventsNoTLSSettings(t *testing.T) {
	dsn, err := DSNFromSecret(secretWith(map[string]string{
		SecretKeyHost:     "postgres.databases.svc",
		SecretKeyDatabase: "kitchen_auth",
		SecretKeyUsername: "kitchen",
		SecretKeyPassword: "hunter2",
	}))
	if err != nil {
		t.Fatalf("assembling a DSN: %v", err)
	}
	if strings.Contains(dsn, "?") {
		t.Errorf("DSN %q carries settings nobody asked for", dsn)
	}
}

// The `dsn` key wins where it exists, settings and all. It is the chart's own,
// and re-deriving it here would be a second opinion about a connection that
// already has one.
func TestTheWrittenDSNIsUsedAsItStands(t *testing.T) {
	want := "postgres://kitchen:hunter2@kitchen-postgres.kitchen-system.svc:5432/kitchen_auth" +
		"?sslmode=verify-full&sslrootcert=/etc/kitchen/internal-ca/ca.crt"
	dsn, err := DSNFromSecret(secretWith(map[string]string{
		SecretKeyDSN:     want,
		SecretKeyHost:    "somewhere-else.svc",
		SecretKeySSLMode: "disable",
	}))
	if err != nil {
		t.Fatalf("reading the DSN: %v", err)
	}
	if dsn != want {
		t.Errorf("DSN is %q, want %q", dsn, want)
	}
}
