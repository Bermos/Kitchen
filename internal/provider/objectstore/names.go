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

package objectstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// A bucket's name is what it is found again by, so the names here are
// deterministic functions of the claim and the preview, kept inside what S3
// takes — 3 to 63 characters of lowercase letters, digits and hyphens — by
// internal/provider/naming, which is also where a claim's bucket gets the
// project in its name and where the refusal to adopt another project's is
// written.

const (
	// maxBucketName is S3's own limit.
	maxBucketName = 63

	// maxAccessKey is MinIO's limit on an access key's length, which is
	// why a scoped credential's access key is a digest rather than the
	// bucket's name.
	maxAccessKey = 20
)

// BranchBucketName is a preview's own bucket beside its parent's — the
// parent's name with the environment's appended, each trimmed so the pair
// fits, with the environment half keeping the most room because it is what
// makes it unique.
func BranchBucketName(parent, environment string) string {
	return naming.Join(parent, maxBucketName/2, environment, maxBucketName)
}

// AccessKeyFor is the access key of the credential scoped to a bucket:
// derived from the bucket's name, so that a lost binding is re-issued to the
// same user rather than leaving one behind per reconcile.
func AccessKeyFor(bucket string) string {
	sum := sha256.Sum256([]byte(bucket))
	return "kc-" + hex.EncodeToString(sum[:])[:maxAccessKey-3]
}

// PolicyName is the name of the policy scoped to a bucket at a MinIO.
func PolicyName(bucket string) string { return bucket }

// secretKeyAlphabet is what a minted secret access key is drawn from: the
// characters every S3 client and every shell accept without quoting.
const secretKeyAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// secretKeyLength is 40, which is what AWS issues and MinIO's maximum.
const secretKeyLength = 40

// newSecretKey mints a secret access key.
func newSecretKey() (string, error) {
	out := make([]byte, secretKeyLength)
	limit := big.NewInt(int64(len(secretKeyAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = secretKeyAlphabet[n.Int64()]
	}
	return string(out), nil
}
