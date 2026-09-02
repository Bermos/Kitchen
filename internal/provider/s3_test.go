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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

const listBucketsXML = `<?xml version="1.0" encoding="UTF-8"?>
<ListAllMyBucketsResult><Owner><ID>root</ID></Owner><Buckets>
<Bucket><Name>kitchen-shop-uploads</Name><CreationDate>2026-01-01T00:00:00.000Z</CreationDate></Bucket>
</Buckets></ListAllMyBucketsResult>`

// s3Stub is enough of an S3 store to answer ListBuckets, and enough of a
// MinIO to answer (or refuse) the admin API's info call.
func s3Stub(t *testing.T, listStatus int, adminStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/minio/admin/") {
			w.WriteHeader(adminStatus)
			if adminStatus == http.StatusOK {
				_, _ = w.Write([]byte(`{"mode": "online"}`))
			} else {
				_, _ = w.Write([]byte(`{"Code": "AccessDenied", "Message": "Access Denied."}`))
			}
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(listStatus)
		if listStatus == http.StatusOK {
			_, _ = w.Write([]byte(listBucketsXML))
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidAccessKeyId</Code>` +
			`<Message>The Access Key Id you provided does not exist in our records.</Message></Error>`))
	}))
}

func s3Connection(endpoint string, config string) *kitchenv1alpha1.Connection {
	raw := `{"endpoint": "` + endpoint + `", "forcePathStyle": true` + config + `}`
	return &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "store"},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider: objectstore.ProviderS3,
			Config:   &runtime.RawExtension{Raw: []byte(raw)},
		},
	}
}

func s3Creds() *corev1.Secret {
	return &corev1.Secret{Data: map[string][]byte{
		objectstore.CredentialKeyAccessKeyID:     []byte("root"),
		objectstore.CredentialKeySecretAccessKey: []byte("hunter2hunter2"),
	}}
}

func TestS3ProbeAcceptsACredentialThatCanListBuckets(t *testing.T) {
	server := s3Stub(t, http.StatusOK, http.StatusOK)
	defer server.Close()

	probe, err := Default(s3Connection(server.URL, ""), s3Creds())
	if err != nil {
		t.Fatal(err)
	}
	result := probe.Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || !result.CredentialValid {
		t.Fatalf("want accepted, got %+v", result)
	}
	if !strings.Contains(result.Message, "1 bucket(s)") {
		t.Errorf("the message says what the credential can see: %q", result.Message)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("a MinIO that answers the admin API earns no warning: %v", result.Warnings)
	}
}

func TestS3ProbeRejectsAWrongCredential(t *testing.T) {
	server := s3Stub(t, http.StatusForbidden, http.StatusForbidden)
	defer server.Close()

	probe, err := Default(s3Connection(server.URL, ""), s3Creds())
	if err != nil {
		t.Fatal(err)
	}
	result := probe.Probe(context.Background())
	if !result.Reachable || !result.CredentialChecked || result.CredentialValid {
		t.Fatalf("want rejected, got %+v", result)
	}
	if !strings.Contains(result.Message, "rejected") {
		t.Errorf("message: %q", result.Message)
	}
}

func TestS3ProbeWarnsWhenScopedCredentialsHaveNoAdminAPI(t *testing.T) {
	server := s3Stub(t, http.StatusOK, http.StatusForbidden)
	defer server.Close()

	probe, err := Default(s3Connection(server.URL, ""), s3Creds())
	if err != nil {
		t.Fatal(err)
	}
	result := probe.Probe(context.Background())
	if !result.CredentialValid {
		t.Fatalf("the credential itself is fine: %+v", result)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "scopedCredentials") {
		t.Errorf("the warning names the flag that reconciles the two: %v", result.Warnings)
	}

	// Told the store has no admin API, the probe does not ask for one.
	probe, err = Default(s3Connection(server.URL, `, "scopedCredentials": false`), s3Creds())
	if err != nil {
		t.Fatal(err)
	}
	if result := probe.Probe(context.Background()); len(result.Warnings) != 0 {
		t.Errorf("no warning for a connection that asked for no scoping: %v", result.Warnings)
	}
}

func TestS3ProbeReportsAnUnreachableStore(t *testing.T) {
	server := s3Stub(t, http.StatusOK, http.StatusOK)
	server.Close()

	probe, err := Default(s3Connection(server.URL, ""), s3Creds())
	if err != nil {
		t.Fatal(err)
	}
	if result := probe.Probe(context.Background()); result.Reachable || result.CredentialChecked {
		t.Errorf("a store that is down is neither reachable nor judged: %+v", result)
	}
}

func TestS3ProbeNeedsBothHalvesOfTheCredential(t *testing.T) {
	creds := &corev1.Secret{Data: map[string][]byte{objectstore.CredentialKeyAccessKeyID: []byte("root")}}
	if _, err := Default(s3Connection("http://minio:9000", ""), creds); err == nil {
		t.Error("a secret missing the secret key must not build a probe")
	}
	if _, err := Default(&kitchenv1alpha1.Connection{Spec: kitchenv1alpha1.ConnectionSpec{Provider: "s3"}}, s3Creds()); err == nil {
		t.Error("a connection without an endpoint must not build a probe")
	}
	if got := Capabilities(objectstore.ProviderS3); len(got) != 1 || got[0] != kitchenv1alpha1.CapabilityObjectStore {
		t.Errorf("s3 provides objectStore and nothing else, got %v", got)
	}
}
