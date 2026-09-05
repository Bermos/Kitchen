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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The security review's finding (#323, and #382 after it) was that the
// telemetry store answers in plaintext inside the platform namespace: every
// log line, every query and the store's own password travel between pods in
// the clear, and a pod that lands in `kitchen-system` — or the node under it
// — reads all of it.
//
// The half of the fix that lives in Go is this client. What follows pins the
// three properties it has to keep, because each of them fails silently: a
// downgrade to plaintext looks like a working connection, an unverified
// certificate looks like a working connection, and a CA bundle that never got
// mounted looks like a working connection right up until it is somebody
// else's certificate on the other end.

// caPEM writes one certificate out as a PEM file and returns its path.
func caPEM(t *testing.T, der []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("writing the test CA bundle: %v", err)
	}
	return path
}

// unrelatedCA is a real, well-formed CA certificate that signed nothing in
// this test. httptest's own certificate cannot play that part: every
// httptest.NewTLSServer in the process serves the same built-in certificate,
// so a second server's is the first server's and the check would pass on a
// client that verified nothing at all.
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating the unrelated CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "not the telemetry store's CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("signing the unrelated CA: %v", err)
	}
	return der
}

// hostPort splits a test server's URL the way ConfigFromSecret's two keys
// carry it.
func hostPort(t *testing.T, raw string) (string, string) {
	t.Helper()
	endpoint, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing the test server URL %q: %v", raw, err)
	}
	return endpoint.Hostname(), endpoint.Port()
}

// A store the chart says speaks TLS is reached over TLS or not at all. The
// failure this pins is the one that would otherwise be invisible: a client
// that quietly retried in the clear would keep every query working and put
// the password back on the wire.
func TestAPlaintextStoreIsRefusedWhenTheSchemeIsHTTPS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("1\n"))
	}))
	defer server.Close()

	host, port := hostPort(t, server.URL)
	client := New(Config{
		Host: host, HTTPPort: port, Database: testDatabase,
		Username: testUsername, Password: testPassword, Scheme: SchemeHTTPS,
	})

	if _, err := client.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("a plaintext store answered a https client: the connection was downgraded, " +
			"which is the finding this scheme exists to close")
	}
}

// The CA bundle is the whole of the difference between `require` and
// `verify-full`. Without this test, mounting the wrong ConfigMap — or none —
// would still connect, to whatever holds the address.
func TestACertificateFromAnotherCAIsRefused(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("1\n"))
	}))
	defer server.Close()

	host, port := hostPort(t, server.URL)
	client := New(Config{
		Host: host, HTTPPort: port, Database: testDatabase,
		Username: testUsername, Password: testPassword,
		// A CA bundle that is a real, well-formed certificate and simply did
		// not sign this server's: the failure has to be verification rather
		// than a bundle that would not parse.
		Scheme: SchemeHTTPS, CAFile: caPEM(t, unrelatedCA(t)),
	})

	_, err := client.Query(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("a certificate signed by nothing in the CA bundle was accepted")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the connection failed for something other than verification: %v", err)
	}
}

// And the positive half, so the two above cannot be passed by a client that
// refuses everything: the store's own CA verifies the store.
func TestTheStoresOwnCAVerifiesIt(t *testing.T) {
	var asked string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.Header.Get("X-ClickHouse-User")
		_, _ = w.Write([]byte("1\n"))
	}))
	defer server.Close()

	host, port := hostPort(t, server.URL)
	client := New(Config{
		Host: host, HTTPPort: port, Database: testDatabase,
		Username: testUsername, Password: testPassword,
		Scheme: SchemeHTTPS, CAFile: caPEM(t, server.Certificate().Raw),
	})

	answer, err := client.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("the store's own CA did not verify it: %v", err)
	}
	if answer != "1" {
		t.Fatalf("answer %q, want 1", answer)
	}
	if asked != testUsername {
		t.Fatalf("the store was asked as %q, want %q", asked, testUsername)
	}
}

// A CA bundle the pod never received is the characteristic failure of this
// design: the ConfigMap the operator publishes is mounted optional, so on the
// first boot of a fresh install the file is simply not there. That has to be
// an error naming the file, not a connection that verifies against the host's
// roots and fails somewhere further down with something unreadable.
func TestAMissingCABundleFailsEveryQueryByName(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ca.crt")
	client := New(Config{
		Host: "kitchen-clickhouse.kitchen-system.svc", HTTPPort: "8443",
		Database: testDatabase, Username: testUsername, Password: testPassword,
		Scheme: SchemeHTTPS, CAFile: missing,
	})

	_, err := client.Query(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("a client with no CA bundle connected anyway")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("the error does not name the bundle it could not read: %v", err)
	}
}

// The connection secret is the chart's statement about the store, and the
// scheme is the part of it that decides whether anything above happens at
// all. An absent key is `http`, because a chart old enough not to write it is
// describing a store that really does answer in the clear; anything else is
// refused rather than guessed at.
func TestTheSchemeIsReadFromTheConnectionSecret(t *testing.T) {
	base := map[string][]byte{
		SecretKeyHost:     []byte("kitchen-clickhouse.kitchen-system.svc"),
		SecretKeyHTTPPort: []byte("8443"),
		SecretKeyDatabase: []byte("kitchen"),
		SecretKeyUsername: []byte("kitchen"),
		SecretKeyPassword: []byte("hunter2"),
	}
	secret := func(extra map[string][]byte) *corev1.Secret {
		data := map[string][]byte{}
		for key, value := range base {
			data[key] = value
		}
		for key, value := range extra {
			data[key] = value
		}
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kitchen-system", Name: "kitchen-clickhouse"},
			Data:       data,
		}
	}

	plain, err := ConfigFromSecret(secret(nil))
	if err != nil {
		t.Fatalf("a secret without a scheme was refused: %v", err)
	}
	if plain.Scheme != SchemeHTTP {
		t.Errorf("scheme %q for a secret that names none, want %q", plain.Scheme, SchemeHTTP)
	}
	if got, want := plain.endpoint(), "http://kitchen-clickhouse.kitchen-system.svc:8443/"; got != want {
		t.Errorf("endpoint %q, want %q", got, want)
	}

	secured, err := ConfigFromSecret(secret(map[string][]byte{
		SecretKeyScheme: []byte(SchemeHTTPS),
		SecretKeyCAFile: []byte("/etc/kitchen/internal-ca/ca.crt"),
	}))
	if err != nil {
		t.Fatalf("a secret naming https was refused: %v", err)
	}
	if got, want := secured.endpoint(), "https://kitchen-clickhouse.kitchen-system.svc:8443/"; got != want {
		t.Errorf("endpoint %q, want %q", got, want)
	}
	if secured.CAFile != "/etc/kitchen/internal-ca/ca.crt" {
		t.Errorf("CA bundle %q, want the mounted path", secured.CAFile)
	}

	if _, err := ConfigFromSecret(secret(map[string][]byte{
		SecretKeyScheme: []byte("tcp"),
	})); err == nil {
		t.Error("a secret naming an unknown scheme was accepted; the client would have " +
			"built an address nothing can dial")
	}
}
