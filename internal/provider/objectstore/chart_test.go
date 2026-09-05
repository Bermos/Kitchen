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

package objectstore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// The store's half of #382 is a property of the chart, and it is invisible
// from Go: nothing in this package fails if the MinIO StatefulSet quietly
// stops being handed a certificate, or if the connection secret goes back to
// saying `http`. Every client here would keep working, in the clear, which is
// exactly how the finding went unnoticed in the first place.
//
// So the checks are textual, over the templates themselves. Rendering them
// would need a helm binary this package cannot assume; the chart-install job
// in `.github/workflows/helm.yml` does that half, against a real render and a
// real cluster, on both its legs.

func chartFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", "..", "charts", "kitchen"}, parts...)...)
	body, err := os.ReadFile(path) //nolint:gosec // a fixed path inside the repository
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(body)
}

// MinIO serves one port. A certificate in the directory `--certs-dir` names is
// the whole of what makes that port TLS, so there is no plaintext listener
// left beside it to remove — but there is also nothing to fall back to if the
// certificate never arrives, which is why the pod mounts it rather than
// starting without it.
func TestTheStoreIsHandedACertificateAndWaitsForIt(t *testing.T) {
	statefulSet := chartFile(t, "templates", "objectstore", "statefulset.yaml")

	if !strings.Contains(statefulSet, "--certs-dir") {
		t.Error("the store is started without --certs-dir, so MinIO looks for a certificate " +
			"in $HOME/.minio/certs, finds none, and answers every request in the clear")
	}
	// cert-manager writes `tls.crt` and `tls.key`; MinIO reads `public.crt`
	// and `private.key` and nothing else. Mounting the Secret as it stands is
	// a directory with the right bytes under the wrong names, which is a
	// store that serves plaintext and says nothing.
	for _, projection := range []string{"path: public.crt", "path: private.key"} {
		if !strings.Contains(statefulSet, projection) {
			t.Errorf("the certificate volume does not project %s; MinIO reads those two "+
				"names alone and serves plaintext without them", projection)
		}
	}
	// Not optional, and it must not become so: an optional mount that is
	// empty is a MinIO that starts, finds no certificate and serves the
	// store in the clear — which is the failure this whole change exists to
	// remove. A required mount waits in ContainerCreating instead, and
	// starts within seconds of the certificate arriving.
	if strings.Contains(statefulSet, "secretName: {{ include \"kitchen.objectStoreTLSSecretName\" . }}") &&
		strings.Contains(statefulSet, "optional: true") {
		t.Error("the store's certificate is mounted optional, so a first install brings up a " +
			"store that answers in the clear instead of one that waits")
	}
	if !strings.Contains(statefulSet, "scheme: {{ $probeScheme }}") {
		t.Error("the store's probes still ask over plain HTTP, so a pod serving TLS never " +
			"becomes ready and the install never finishes")
	}
}

// What the platform's own clients are told about the store. The scheme is what
// stops a client dialling plaintext; the CA bundle is the whole of the
// difference between an encrypted connection and a verified one; and
// `certificateSecret` is the only thing that asks for a certificate at all —
// InternalTLSReconciler issues from the secret, not from a value.
func TestTheStoresSecretSaysWhereItIsAndHowItIsVerified(t *testing.T) {
	secret := chartFile(t, "templates", "objectstore", "secret.yaml")

	for _, key := range []string{"host:", "scheme:", "caFile:", "certificateSecret:"} {
		if !strings.Contains(secret, key) {
			t.Errorf("the store's secret carries no %s key, so every client of it falls back "+
				"to the plaintext defaults", strings.TrimSuffix(key, ":"))
		}
	}
	// The spellings this package and the operator read. A rename on one side
	// alone is a store that is never issued for, or one every client reaches
	// in the clear, and nothing that says so.
	for _, key := range []string{
		objectstore.SecretKeyHost,
		objectstore.SecretKeyScheme,
		objectstore.SecretKeyCAFile,
		objectstore.SecretKeyCertificateSecret,
	} {
		if !strings.Contains(secret, "\n  "+key+":") {
			t.Errorf("the store's secret does not spell the key %q the way its clients read "+
				"it", key)
		}
	}
}

// The one thing the operator has to be able to do that no application can:
// verify the store against the platform's own CA. Its pod mounts the bundle,
// and the mount is gated on any bundled store serving TLS rather than on the
// telemetry store alone — the operator is a client of both.
func TestTheOperatorMountsTheCAItVerifiesTheStoreAgainst(t *testing.T) {
	deployment := chartFile(t, "templates", "deployment.yaml")

	if !strings.Contains(deployment, `include "kitchen.internalTLSEnabled" .`) {
		t.Error("the operator's CA mount is gated on one store's TLS rather than on any of " +
			"them, so an installation with only the object store encrypted provisions every " +
			"bucket against a file that is not mounted")
	}
	if !strings.Contains(deployment, `include "kitchen.internalCAMountPath" .`) {
		t.Error("the operator does not mount the CA bundle, so every request to the store " +
			"fails on a file that is not there")
	}
	// And it is optional, which is the opposite of the store's own mount: the
	// operator is the process that creates the CA, so a required mount would
	// be the pod that mints the bundle refusing to start until the bundle is
	// there.
	if !strings.Contains(deployment, "optional: true") {
		t.Error("the operator's CA mount is required: on a first install that is a deadlock, " +
			"since the ConfigMap does not exist until this pod has run once")
	}
}

// The NetworkPolicy admits application namespaces to the store's port, and
// serving TLS does not move it — unlike the telemetry store, which changes
// both of its ports. A rule left on a port nothing listens on is an object
// store no application can reach, on a cluster where every claim still reads
// as bound.
func TestServingTLSDoesNotMoveThePortApplicationsAreAdmittedTo(t *testing.T) {
	policy := chartFile(t, "templates", "networkpolicy.yaml")
	statefulSet := chartFile(t, "templates", "objectstore", "statefulset.yaml")

	const port = "{{ int .Values.objectStore.service.port }}"
	if !strings.Contains(policy, "port: "+port) {
		t.Error("the policy no longer admits application namespaces to " +
			"objectStore.service.port, which is the port the store listens on")
	}
	if !strings.Contains(statefulSet, "containerPort: "+port) {
		t.Error("the store no longer listens on objectStore.service.port, so the one rule " +
			"that admits an application namespace opens a port nothing answers")
	}
}
