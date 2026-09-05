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
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
	"github.com/Bermos/Kitchen/internal/provider/objectstore/objectstoretest"
)

// The bundled store serves a certificate the platform's own CA signed, and no
// host's roots have heard of that authority (#382). So every client of it is
// built from a Connection that names the bundle, and the one thing none of
// these paths may do is carry on unverified.

// caFile writes a PEM bundle to a file the way the operator's pod sees one,
// and answers the path.
func caFile(t *testing.T, pemBytes []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("writing the CA bundle: %v", err)
	}
	return path
}

// anotherAuthority mints a certificate authority of its own, so that a bundle
// naming it vouches for nothing the test's stores serve. httptest's own
// certificate is one fixed certificate shared by every server it starts, so
// two servers cannot be told apart by it.
func anotherAuthority(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("minting a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "somebody else's CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("minting a certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// connection is an s3 Connection carrying this config, as the operator seeds
// one for the bundled store.
func connection(t *testing.T, cfg string) *kitchenv1alpha1.Connection {
	t.Helper()
	return &kitchenv1alpha1.Connection{
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider: objectstore.ProviderS3,
			Config:   &runtime.RawExtension{Raw: []byte(cfg)},
		},
	}
}

// A store on `https://` with a bundle behind it is verified against that
// bundle and nothing else, and the bytes come back so that a binding can
// carry them to an application that cannot mount the platform's ConfigMap.
func TestAStoreWithABundleIsVerifiedAgainstIt(t *testing.T) {
	store := httptest.NewTLSServer(nil)
	defer store.Close()
	authority := pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: store.Certificate().Raw,
	})

	cfg := objectstore.Config{Endpoint: store.URL, CAFile: caFile(t, authority)}
	transport, bundle, err := cfg.Verify()
	if err != nil {
		t.Fatalf("building the verified transport: %v", err)
	}
	if transport == nil {
		t.Fatal("a store reached over https with a CA is verified against it; got the SDK's " +
			"own transport, which verifies against the host's roots and fails")
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Error("the transport carries no root pool, so the platform's CA is not in the path")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("the transport skips verification: there is nothing about a CA the platform " +
			"mints itself that its own clients cannot verify")
	}
	if string(bundle) != string(authority) {
		t.Error("the CA bytes are not answered, so no binding can carry them and no " +
			"application can verify the store it was handed")
	}
}

// The two shapes that are the host's roots' business, where a transport of
// the platform's own would be wrong rather than merely unnecessary: a store
// on the internet with a publicly trusted certificate, and an endpoint that
// is not TLS at all — where a CA verifies nothing and saying so is better
// than building a transport nothing consults.
func TestAStoreWithoutABundleKeepsTheSDKsOwnTransport(t *testing.T) {
	for _, tc := range []struct{ what, endpoint, ca string }{
		{"a store on the internet", "https://s3.eu-central-1.amazonaws.com", ""},
		{"a store reached in the clear", "http://kitchen-objectstore.kitchen-system.svc:9000", "/etc/kitchen/internal-ca/ca.crt"},
	} {
		cfg := objectstore.Config{Endpoint: tc.endpoint, CAFile: tc.ca}
		transport, bundle, err := cfg.Verify()
		if err != nil {
			t.Errorf("%s: %v", tc.what, err)
		}
		if transport != nil || bundle != nil {
			t.Errorf("%s: a transport of the platform's own was built for it", tc.what)
		}
	}
}

// A bundle that is not there fails the client rather than leaving one that
// connects to the store unverified. The claim goes Pending naming the file,
// which is the loud half of never falling back.
func TestAMissingBundleIsAFailureAndNotAFallback(t *testing.T) {
	cfg := objectstore.Config{
		Endpoint: "https://kitchen-objectstore.kitchen-system.svc:9000",
		CAFile:   filepath.Join(t.TempDir(), "never-written.crt"),
	}
	if _, _, err := cfg.Verify(); err == nil {
		t.Fatal("a CA bundle that cannot be read built a client anyway, which is a store " +
			"reached unverified and nothing said about it")
	} else if !strings.Contains(err.Error(), "never-written.crt") {
		t.Errorf("the failure does not name the file it could not read: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty.crt")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.CAFile = empty
	if _, _, err := cfg.Verify(); err == nil {
		t.Error("a bundle holding no certificate built a client with an empty root pool, " +
			"which verifies nothing and says nothing")
	}
}

// The whole point, end to end: a client built for a store that serves TLS
// will not talk to one that answers in the clear. Nothing in this path
// downgrades, and a store that lost its certificate is a loud failure rather
// than a quiet plaintext upload.
func TestAPlaintextStoreIsRefusedByAClientBuiltForTLS(t *testing.T) {
	plaintext := httptest.NewServer(nil)
	defer plaintext.Close()

	trusted := httptest.NewTLSServer(nil)
	defer trusted.Close()
	authority := caFile(t, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: trusted.Certificate().Raw,
	}))

	// The plaintext store, addressed as the platform addresses a store that
	// says it serves TLS. The certificate the client asks for is never
	// offered, so the handshake is where this ends.
	endpoint := strings.Replace(plaintext.URL, "http://", "https://", 1)
	s3, err := objectstore.NewS3(objectstore.Options{
		Connection:      connection(t, `{"endpoint":"`+endpoint+`","forcePathStyle":true,"caFile":"`+authority+`"}`),
		AccessKeyID:     "root",
		SecretAccessKey: "hunter2hunter2",
	}, nil)
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	if _, err := s3.Buckets.BucketExists(context.Background(), "kitchen-shop-uploads"); err == nil {
		t.Fatal("a client built for a store that serves TLS reached one answering in the " +
			"clear; the platform would upload every object unencrypted and say nothing")
	}
}

// And the certificate has to be the one the bundle vouches for: a store
// serving somebody else's certificate is refused, which is the difference
// between an encrypted connection and a verified one.
func TestAStoreServingAnUnknownCertificateIsRefused(t *testing.T) {
	store := httptest.NewTLSServer(nil)
	defer store.Close()

	authority := caFile(t, anotherAuthority(t))
	s3, err := objectstore.NewS3(objectstore.Options{
		Connection: connection(t,
			`{"endpoint":"`+store.URL+`","forcePathStyle":true,"caFile":"`+authority+`"}`),
		AccessKeyID:     "root",
		SecretAccessKey: "hunter2hunter2",
	}, nil)
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	// A deadline of its own: the SDK retries a refused handshake, and what is
	// being asserted is that it never succeeds, not how long it tries.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s3.Buckets.BucketExists(ctx, "kitchen-shop-uploads"); err == nil {
		t.Fatal("the client accepted a certificate the platform's CA did not sign")
	}
}

// An application cannot mount the platform's CA: the ConfigMap is in
// kitchen-system, its pod is not, and no image the platform did not build
// carries a private root in its trust store. So the certificate travels in
// the binding, or the application verifies nothing.
func TestTheBindingCarriesTheAuthorityAnApplicationCannotOtherwiseHave(t *testing.T) {
	store := objectstoretest.New()
	s3 := &objectstore.S3{
		Config: objectstore.Config{
			Endpoint:       "https://kitchen-objectstore.kitchen-system.svc.cluster.local:9000",
			Region:         "us-east-1",
			ForcePathStyle: true,
			InCluster:      true,
		},
		AccessKeyID:     "root",
		SecretAccessKey: "hunter2hunter2",
		Buckets:         store,
		Admin:           store,
		CACert:          "-- the platform's CA --",
	}

	instance, err := s3.Provision(context.Background(), shopUploads)
	if err != nil {
		t.Fatal(err)
	}
	if instance.Binding.CACert != "-- the platform's CA --" {
		t.Errorf("the binding carries no CA, so the application cannot verify the store it "+
			"was handed: %+v", instance.Binding)
	}
	if got := string(instance.Binding.Data()[objectstore.BindingKeyCACert]); got != "-- the platform's CA --" {
		t.Errorf("the Secret's %s key is %q", objectstore.BindingKeyCACert, got)
	}
	if !strings.HasPrefix(string(instance.Binding.Data()[objectstore.BindingKeyEndpoint]), "https://") {
		t.Error("the binding still hands the application an http:// endpoint")
	}

	// And it is absent rather than empty for a store with a publicly trusted
	// certificate. A key that is present and empty reads to an application as
	// "verify against nothing", which is the one thing this must not say.
	public := *s3
	public.CACert = ""
	instance, err = public.Provision(context.Background(), shopCDN)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := instance.Binding.Data()[objectstore.BindingKeyCACert]; ok {
		t.Error("a store with no CA of the platform's still writes the key, which reads as " +
			"an authority that vouches for nothing")
	}
}

// The store's half of a binding is written over an existing Secret when it
// changes — the bundled store gaining a certificate is exactly that — and the
// credential is left alone, because reissuing it to carry an address would
// roll every pod for a change that is not theirs.
func TestTheAddressIsTheHalfOfABindingThatCanChangeUnderIt(t *testing.T) {
	s3 := &objectstore.S3{Config: objectstore.Config{
		Endpoint:       "https://kitchen-objectstore.kitchen-system.svc.cluster.local:9000",
		Region:         "us-east-1",
		ForcePathStyle: true,
	}, CACert: "-- the platform's CA --"}

	data := s3.Address().Data()
	for key, want := range map[string]string{
		objectstore.BindingKeyEndpoint:       s3.Config.Endpoint,
		objectstore.BindingKeyRegion:         "us-east-1",
		objectstore.BindingKeyForcePathStyle: "true",
		objectstore.BindingKeyCACert:         "-- the platform's CA --",
	} {
		if got := string(data[key]); got != want {
			t.Errorf("the address's %s is %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{
		objectstore.BindingKeyBucket,
		objectstore.BindingKeyAccessKeyID,
		objectstore.BindingKeySecretAccessKey,
	} {
		if _, ok := data[key]; ok {
			t.Errorf("the address carries %s, so keeping a binding's address in step would "+
				"rewrite the bucket's own credential", key)
		}
	}

	// The absence has to travel too: a store that stopped serving a
	// certificate the platform issued leaves an application verifying
	// against an authority that no longer signs anything.
	plain := &objectstore.S3{Config: s3.Config}
	if _, ok := plain.Address().Data()[objectstore.BindingKeyCACert]; !ok {
		t.Error("an address with no CA does not write the key at all, so a stale one is " +
			"never cleared from a binding that already carries it")
	}
}
