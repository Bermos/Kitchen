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

package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The endpoint check is a CEL rule on the CRD, and this is the backstop it
// needs: an object written before that rule existed reconciles into a run, and
// a run that uploaded every credential this platform holds over plain HTTP
// would be the finding all over again with a rule sitting next to it.
func TestAnEndpointThatIsNotHTTPSIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		s3       *kitchenv1alpha1.S3Destination
		accepted bool
	}{
		{"the AWS endpoint", &kitchenv1alpha1.S3Destination{Bucket: "b"}, true},
		{"a store on the internet", &kitchenv1alpha1.S3Destination{
			Bucket: "b", Endpoint: "https://s3.example.com"}, true},
		// Not the in-cluster `.svc` endpoint: that one is accepted here and
		// then reaches for the internal CA bundle, which is a file in a pod
		// rather than in a test. InCluster is what covers it.
		{"plain http", &kitchenv1alpha1.S3Destination{
			Bucket: "b", Endpoint: "http://minio.internal:9000"}, false},
		{"no scheme at all", &kitchenv1alpha1.S3Destination{
			Bucket: "b", Endpoint: "minio.internal:9000"}, false},
		{"plain http, on purpose", &kitchenv1alpha1.S3Destination{
			Bucket: "b", Endpoint: "http://minio.internal:9000", AllowInsecureEndpoint: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := &kitchenv1alpha1.BackupDestination{
				Type: kitchenv1alpha1.BackupDestinationS3, S3: tc.s3,
			}
			err := CheckEndpoint(destination)
			if tc.accepted && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.accepted {
				if err == nil {
					t.Fatal("the archive would have travelled in the clear")
				}
				if !strings.Contains(err.Error(), "allowInsecureEndpoint") {
					t.Errorf("the refusal does not name the way out: %v", err)
				}
			}

			// And nothing reaches the store either way: Open refuses before
			// it builds a client, so every caller inherits the rule.
			_, openErr := Open(context.Background(), newClient(t), testNamespace, destination)
			if tc.accepted != (openErr == nil) {
				t.Errorf("Open answered %v where the check answered %v", openErr, err)
			}
		})
	}
}

// Encryption is what an installation gets without saying anything, and a run
// with no key does not fall back to uploading the platform's whole credential
// store in the clear — it fails, naming both ways out.
func TestTheEncryptionKeyIsRequiredUnlessItIsTurnedOff(t *testing.T) {
	key := NewEncryptionKey()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: EncryptionKeySecretName, Namespace: testNamespace},
		Data:       map[string][]byte{EncryptionKeySecretKey: []byte(base64.StdEncoding.EncodeToString(key))},
	}

	// The key is there and is read.
	read, err := EncryptionKey(context.Background(), newClient(t, secret), testNamespace,
		kitchenv1alpha1.BackupSpec{})
	if err != nil {
		t.Fatalf("the key was not read: %v", err)
	}
	if !bytes.Equal(read, key) {
		t.Error("the key that came back is not the one that was stored")
	}

	// It is not, and an unset mode still means encrypted.
	if _, err := EncryptionKey(context.Background(), newClient(t), testNamespace,
		kitchenv1alpha1.BackupSpec{}); err == nil {
		t.Fatal("a platform with no key was given none and no error, which is an unencrypted upload")
	} else if !strings.Contains(err.Error(), "encryption.mode to none") {
		t.Errorf("the refusal does not name both ways out: %v", err)
	}

	// The opt-out, which is the one way to get no key and no error.
	none, err := EncryptionKey(context.Background(), newClient(t), testNamespace,
		kitchenv1alpha1.BackupSpec{Encryption: kitchenv1alpha1.BackupEncryptionSpec{
			Mode: kitchenv1alpha1.BackupEncryptionNone,
		}})
	if err != nil {
		t.Fatalf("an installation that opted out was refused anyway: %v", err)
	}
	if none != nil {
		t.Error("an installation that opted out was handed a key")
	}
}
