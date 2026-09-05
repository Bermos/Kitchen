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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// The real clients: minio-go for the S3 API, which every store speaks, and
// madmin-go for the MinIO admin API, which mints the per-bucket credential.
// Both are kept to the two interfaces in s3.go, so that what the provisioner
// does is tested without a store and what these do is a wrapper each.

// Default resolves the built-in providers.
func Default(opts Options) (Provisioner, error) {
	if opts.Connection == nil {
		return nil, fmt.Errorf("%w: no connection", ErrUnsupportedProvider)
	}
	switch opts.Connection.Spec.Provider {
	case ProviderS3:
		return NewS3(opts, nil)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, opts.Connection.Spec.Provider)
	}
}

// NewS3 builds the provisioner from a Connection and its credential. The
// transport is injectable for tests that stand up a store on httptest.
func NewS3(opts Options, transport http.RoundTripper) (*S3, error) {
	cfg, err := ConfigOf(opts.Connection)
	if err != nil {
		return nil, err
	}
	if opts.AccessKeyID == "" || opts.SecretAccessKey == "" {
		return nil, fmt.Errorf("the %s provider needs both %s and %s in the connection's credentials secret",
			ProviderS3, CredentialKeyAccessKeyID, CredentialKeySecretAccessKey)
	}
	host, secure, err := cfg.Host()
	if err != nil {
		return nil, err
	}
	// What the store's certificate is verified against, where the Connection
	// names a bundle — the bundled store's, signed by the platform's own CA,
	// which no host's roots have heard of. A caller that brought its own
	// transport (the tests, against an httptest store) keeps it: there is
	// nothing here that could be verified anyway.
	//
	// A bundle that cannot be read fails the whole provisioner rather than
	// leaving one that connects unverified. The claim goes Pending naming the
	// file, which is the loud half of never falling back.
	caCert := ""
	if transport == nil {
		verified, bundle, err := cfg.Verify()
		if err != nil {
			return nil, err
		}
		if verified != nil {
			transport, caCert = verified, string(bundle)
		}
	}
	creds := credentials.NewStaticV4(opts.AccessKeyID, opts.SecretAccessKey, "")
	lookup := minio.BucketLookupDNS
	if cfg.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    transport,
	})
	if err != nil {
		return nil, fmt.Errorf("building the S3 client for %s: %w", cfg.Endpoint, err)
	}
	s := &S3{
		Config:          cfg,
		AccessKeyID:     opts.AccessKeyID,
		SecretAccessKey: opts.SecretAccessKey,
		Buckets:         &minioBuckets{client: client},
		CACert:          caCert,
	}
	if cfg.Scoped() {
		admin, err := madmin.NewWithOptions(host, &madmin.Options{Creds: creds, Secure: secure, Transport: transport})
		if err != nil {
			return nil, fmt.Errorf("building the admin client for %s: %w", cfg.Endpoint, err)
		}
		s.Admin = &minioAdmin{client: admin}
	}
	return s, nil
}

// minioBuckets is BucketAPI over minio-go.
type minioBuckets struct{ client *minio.Client }

func (b *minioBuckets) BucketExists(ctx context.Context, bucket string) (bool, error) {
	exists, err := b.client.BucketExists(ctx, bucket)
	return exists, classify(err)
}

func (b *minioBuckets) MakeBucket(ctx context.Context, bucket, region string) error {
	err := b.client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region})
	if isCode(err, "BucketAlreadyOwnedByYou", "BucketAlreadyExists") {
		return nil
	}
	return classify(err)
}

func (b *minioBuckets) SetVersioning(ctx context.Context, bucket string, enabled bool) error {
	if enabled {
		return classify(b.client.EnableVersioning(ctx, bucket))
	}
	return classify(b.client.SuspendVersioning(ctx, bucket))
}

func (b *minioBuckets) Versioning(ctx context.Context, bucket string) (bool, error) {
	cfg, err := b.client.GetBucketVersioning(ctx, bucket)
	if err != nil {
		if isCode(err, "NotImplemented") {
			// A store with no versioning has none to mirror.
			return false, nil
		}
		return false, classify(err)
	}
	return cfg.Enabled(), nil
}

// Tags reads the bucket's tag set. An untagged bucket, and a store that
// implements no tagging at all, both read as no tags rather than as an
// error: the tag set is a record beside the name, and its absence is an
// answer.
func (b *minioBuckets) Tags(ctx context.Context, bucket string) (map[string]string, error) {
	set, err := b.client.GetBucketTagging(ctx, bucket)
	if err != nil {
		if isCode(err, "NoSuchTagSet", "NoSuchTagSetError", "NotImplemented") {
			return map[string]string{}, nil
		}
		return nil, classify(err)
	}
	return set.ToMap(), nil
}

func (b *minioBuckets) SetTags(ctx context.Context, bucket string, values map[string]string) error {
	set, err := tags.MapToBucketTags(values)
	if err != nil {
		return err
	}
	err = b.client.SetBucketTagging(ctx, bucket, set)
	if isCode(err, "NotImplemented") {
		return nil
	}
	return classify(err)
}

func (b *minioBuckets) SetAnonymousRead(ctx context.Context, bucket string, policy []byte) error {
	return classify(b.client.SetBucketPolicy(ctx, bucket, string(policy)))
}

func (b *minioBuckets) RemoveAllObjects(ctx context.Context, bucket string) error {
	objects := b.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true})
	for object := range objects {
		if object.Err != nil {
			return classify(object.Err)
		}
		err := b.client.RemoveObject(ctx, bucket, object.Key, minio.RemoveObjectOptions{
			VersionID:        object.VersionID,
			ForceDelete:      true,
			GovernanceBypass: true,
		})
		if err != nil && !isCode(err, "NoSuchKey", "NoSuchVersion") {
			return classify(err)
		}
	}
	return nil
}

func (b *minioBuckets) RemoveBucket(ctx context.Context, bucket string) error {
	err := b.client.RemoveBucket(ctx, bucket)
	if isCode(err, "NoSuchBucket") {
		return nil
	}
	return classify(err)
}

// minioAdmin is AdminAPI over madmin-go.
type minioAdmin struct{ client *madmin.AdminClient }

func (a *minioAdmin) PutUser(ctx context.Context, accessKey, secretKey string) error {
	return classify(a.client.AddUser(ctx, accessKey, secretKey))
}

func (a *minioAdmin) RemoveUser(ctx context.Context, accessKey string) error {
	err := a.client.RemoveUser(ctx, accessKey)
	if isAdminCode(err, "XMinioAdminNoSuchUser") {
		return nil
	}
	return classify(err)
}

func (a *minioAdmin) PutPolicy(ctx context.Context, name string, document []byte) error {
	return classify(a.client.AddCannedPolicy(ctx, name, document))
}

func (a *minioAdmin) RemovePolicy(ctx context.Context, name string) error {
	err := a.client.RemoveCannedPolicy(ctx, name)
	if isAdminCode(err, "XMinioAdminNoSuchPolicy") {
		return nil
	}
	return classify(err)
}

func (a *minioAdmin) AttachPolicy(ctx context.Context, policy, accessKey string) error {
	_, err := a.client.AttachPolicy(ctx, madmin.PolicyAssociationReq{Policies: []string{policy}, User: accessKey})
	if isAdminCode(err, "XMinioAdminPolicyChangeAlreadyApplied") {
		return nil
	}
	return classify(err)
}

func (a *minioAdmin) SetQuota(ctx context.Context, bucket string, bytes uint64) error {
	// Quota is the field older servers read and Size the one newer ones
	// do; both are set so the limit lands whichever the store is.
	return classify(a.client.SetBucketQuota(ctx, bucket, &madmin.BucketQuota{
		Quota: bytes, Size: bytes, Type: madmin.HardQuota,
	}))
}

// classify turns a wire error into the contract's vocabulary: a store that
// cannot be reached is ErrNotReady, an operation the store does not
// implement is ErrUnsatisfiable, and everything else is itself.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if isCode(err, "NotImplemented") || isAdminCode(err, "NotImplemented") {
		return fmt.Errorf("%w: the store does not implement this: %v", ErrUnsatisfiable, err)
	}
	var urlErr *url.Error
	var netErr net.Error
	if errors.As(err, &urlErr) || errors.As(err, &netErr) {
		return fmt.Errorf("%w: the store is not answering: %v", ErrNotReady, err)
	}
	return err
}

func isCode(err error, codes ...string) bool {
	var resp minio.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	for _, code := range codes {
		if resp.Code == code {
			return true
		}
	}
	return false
}

func isAdminCode(err error, codes ...string) bool {
	var resp madmin.ErrorResponse
	if !errors.As(err, &resp) {
		return false
	}
	for _, code := range codes {
		if resp.Code == code {
			return true
		}
	}
	return false
}
