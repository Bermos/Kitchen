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
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// BucketAPI is the S3 half of what the provisioner does: the calls every
// S3-compatible store answers. It is an interface so the provisioner can be
// tested against an in-memory store (objectstoretest) rather than a
// stand-in for one; the real one is a thin wrapper over minio-go.
//
// Every method takes the bucket by name and treats "already so" as success.
// A transport error — the store not answering — is returned wrapping
// ErrNotReady, and a NotImplemented from the store wrapping ErrUnsatisfiable,
// so that the provisioner does not have to know what a wire error looks like.
type BucketAPI interface {
	BucketExists(ctx context.Context, bucket string) (bool, error)
	MakeBucket(ctx context.Context, bucket, region string) error
	SetVersioning(ctx context.Context, bucket string, enabled bool) error
	Versioning(ctx context.Context, bucket string) (bool, error)
	SetAnonymousRead(ctx context.Context, bucket string, policy []byte) error
	RemoveAllObjects(ctx context.Context, bucket string) error
	RemoveBucket(ctx context.Context, bucket string) error
	// Tags reads a bucket's tag set; a bucket with none reads as an empty
	// map and no error, the way an absent tag set is not a failure.
	Tags(ctx context.Context, bucket string) (map[string]string, error)
	// SetTags replaces a bucket's tag set. A store that refuses tagging
	// answers an error wrapping ErrUnsatisfiable, which the provisioner
	// takes as "this store records nothing" rather than as a failure.
	SetTags(ctx context.Context, bucket string, tags map[string]string) error
}

// AdminAPI is the MinIO half: users, policies and quotas, which the S3 API
// has no words for. Nil on a provisioner whose Connection says the store has
// none, in which case every claim is handed the Connection's own credential.
type AdminAPI interface {
	// PutUser creates the user or resets its secret key — the same call at
	// MinIO, which is what makes a lost binding recoverable.
	PutUser(ctx context.Context, accessKey, secretKey string) error
	RemoveUser(ctx context.Context, accessKey string) error
	PutPolicy(ctx context.Context, name string, document []byte) error
	RemovePolicy(ctx context.Context, name string) error
	AttachPolicy(ctx context.Context, policy, accessKey string) error
	SetQuota(ctx context.Context, bucket string, bytes uint64) error
}

// S3 provisions buckets at one S3-compatible store.
type S3 struct {
	// Config is the Connection's: where the store is and how it is talked
	// to.
	Config Config
	// AccessKeyID and SecretAccessKey are the Connection's own credential.
	AccessKeyID     string
	SecretAccessKey string
	// Buckets is the S3 API; Admin the MinIO admin API, nil when the
	// Connection says the store speaks none.
	Buckets BucketAPI
	Admin   AdminAPI
	// CACert is the PEM certificate this client verified the store against,
	// where that was the platform's own CA rather than the host's roots. It
	// is carried into every binding, because an application pod has no way
	// to be told about a private authority otherwise.
	CACert string
}

var _ CapableProvisioner = (*S3)(nil)
var _ Addressable = (*S3)(nil)

// Address is where this store answers and what vouches for it — the half of
// a binding that belongs to the store rather than to a bucket.
func (s *S3) Address() Address {
	return Address{
		Endpoint:       s.Config.Endpoint,
		Region:         s.Config.Region,
		ForcePathStyle: s.Config.ForcePathStyle,
		CACert:         s.CACert,
	}
}

// Provision creates or finds the claim's bucket and issues its credential.
func (s *S3) Provision(ctx context.Context, res naming.Resource) (Instance, error) {
	return s.ProvisionWith(ctx, res, Requirements{})
}

// ProvisionWith is Provision with the claim's requirements applied. Every
// requirement is checked against what this store can do *before* the first
// call to it, so a refusal leaves nothing behind.
func (s *S3) ProvisionWith(ctx context.Context, res naming.Resource, req Requirements) (Instance, error) {
	quota, err := s.refuseUnsatisfiable(req)
	if err != nil {
		return Instance{}, err
	}
	bucket, err := naming.Resolve(ctx, res, naming.Provider{
		Kind: "bucket", Limit: maxBucketName, Lookup: s.owner,
	})
	if err != nil {
		return Instance{}, err
	}
	binding, err := s.ensureBucket(ctx, bucket, req.Versioning)
	if err != nil {
		return Instance{}, err
	}
	s.recordProject(ctx, bucket, res.Project)
	if req.PublicRead {
		if err := s.Buckets.SetAnonymousRead(ctx, bucket, anonymousReadPolicy(bucket)); err != nil {
			return Instance{}, fmt.Errorf("making bucket %s publicly readable: %w", bucket, err)
		}
	}
	if quota > 0 {
		if err := s.Admin.SetQuota(ctx, bucket, quota); err != nil {
			return Instance{}, fmt.Errorf("setting the quota on bucket %s: %w", bucket, err)
		}
	}
	return Instance{
		ID:         bucket,
		Name:       bucket,
		Binding:    binding,
		Provenance: ProvenanceProduction,
		Region:     s.Config.Region,
	}, nil
}

// owner answers naming.Lookup: whether a bucket of that name is at the store
// and which project Kitchen tagged it for. A store that will not answer
// GetBucketTagging — S3 is not obliged to, and not every compatible store
// implements it — reports the bucket with no project, which is the honest
// answer and the one that refuses rather than adopts.
func (s *S3) owner(ctx context.Context, bucket string) (naming.Owner, error) {
	exists, err := s.Buckets.BucketExists(ctx, bucket)
	if err != nil {
		return naming.Owner{}, fmt.Errorf("looking for bucket %s: %w", bucket, err)
	}
	if !exists {
		return naming.Owner{}, nil
	}
	tags, err := s.Buckets.Tags(ctx, bucket)
	if err != nil {
		return naming.Owner{Found: true}, nil
	}
	return naming.Owner{Found: true, Project: tags[naming.LabelProject]}, nil
}

// recordProject tags a bucket with the project it belongs to, where it does
// not already carry one — a bucket an operator has just handed over records
// whose it is from now on.
//
// It answers no error on purpose. Tagging is a *record*: the boundary is the
// bucket's name, which carries the project whether or not the store will
// keep a tag set, and S3 does not oblige a compatible store to implement
// tagging at all. A store that refuses is a store where the name is the
// whole of the record — not a claim that fails.
func (s *S3) recordProject(ctx context.Context, bucket, project string) {
	if project == "" {
		return
	}
	tags, err := s.Buckets.Tags(ctx, bucket)
	if err != nil {
		return
	}
	if tags[naming.LabelProject] == project {
		return
	}
	if tags == nil {
		tags = map[string]string{}
	}
	tags[naming.LabelProject] = project
	_ = s.Buckets.SetTags(ctx, bucket, tags)
}

// refuseUnsatisfiable is the check every requirement goes through first,
// answering the quota in bytes where a size was asked for.
func (s *S3) refuseUnsatisfiable(req Requirements) (uint64, error) {
	if req.PublicRead && s.Config.InCluster {
		return 0, fmt.Errorf("%w: the bundled object store is reached at a Service address inside the cluster "+
			"and nowhere else, so a publicly readable bucket would publish nothing — serve the objects through "+
			"the application, or claim through an s3 connection to a store that is on the internet",
			ErrUnsatisfiable)
	}
	if req.Size == "" {
		return 0, nil
	}
	if s.Admin == nil {
		return 0, fmt.Errorf("%w: a size limit is a bucket quota, which only the MinIO admin API sets, and this "+
			"connection says its store has none (scopedCredentials: false) — drop objectStore.size, or claim "+
			"through a connection to a MinIO", ErrUnsatisfiable)
	}
	quantity, err := resource.ParseQuantity(req.Size)
	if err != nil || quantity.Sign() <= 0 {
		return 0, fmt.Errorf("%w: objectStore.size is a Kubernetes quantity such as \"50Gi\" (got %q)",
			ErrUnsatisfiable, req.Size)
	}
	return uint64(quantity.Value()), nil //nolint:gosec // Sign() > 0 was checked above
}

// CreateBranch creates or finds a preview's own bucket beside the parent's:
// empty, versioned when the parent is, with a credential of its own.
func (s *S3) CreateBranch(ctx context.Context, instanceID, name string) (Branch, error) {
	bucket := BranchBucketName(instanceID, name)
	versioned, err := s.Buckets.Versioning(ctx, instanceID)
	if err != nil {
		return Branch{}, fmt.Errorf("reading versioning on bucket %s: %w", instanceID, err)
	}
	binding, err := s.ensureBucket(ctx, bucket, versioned)
	if err != nil {
		return Branch{}, err
	}
	return Branch{ID: bucket, Binding: binding, Provenance: ProvenanceSynthetic}, nil
}

// Deprovision removes the bucket's credential, its objects — every version
// of them — and the bucket itself. Absent is success at every step.
func (s *S3) Deprovision(ctx context.Context, instanceID string) error {
	if s.Admin != nil {
		if err := s.Admin.RemoveUser(ctx, AccessKeyFor(instanceID)); err != nil {
			return fmt.Errorf("removing the user of bucket %s: %w", instanceID, err)
		}
		if err := s.Admin.RemovePolicy(ctx, PolicyName(instanceID)); err != nil {
			return fmt.Errorf("removing the policy of bucket %s: %w", instanceID, err)
		}
	}
	exists, err := s.Buckets.BucketExists(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("looking for bucket %s: %w", instanceID, err)
	}
	if !exists {
		return nil
	}
	if err := s.Buckets.RemoveAllObjects(ctx, instanceID); err != nil {
		return fmt.Errorf("emptying bucket %s: %w", instanceID, err)
	}
	if err := s.Buckets.RemoveBucket(ctx, instanceID); err != nil {
		return fmt.Errorf("removing bucket %s: %w", instanceID, err)
	}
	return nil
}

// DeleteBranch removes a preview's bucket; it is a bucket like any other.
func (s *S3) DeleteBranch(ctx context.Context, _, branchID string) error {
	return s.Deprovision(ctx, branchID)
}

// ensureBucket is the shared half of Provision and CreateBranch: the bucket,
// its versioning, and the credential the binding carries.
func (s *S3) ensureBucket(ctx context.Context, bucket string, versioning bool) (Binding, error) {
	exists, err := s.Buckets.BucketExists(ctx, bucket)
	if err != nil {
		return Binding{}, fmt.Errorf("looking for bucket %s: %w", bucket, err)
	}
	if !exists {
		if err := s.Buckets.MakeBucket(ctx, bucket, s.Config.Region); err != nil {
			return Binding{}, fmt.Errorf("creating bucket %s: %w", bucket, err)
		}
	}
	if versioning {
		if err := s.Buckets.SetVersioning(ctx, bucket, true); err != nil {
			return Binding{}, fmt.Errorf("enabling versioning on bucket %s: %w", bucket, err)
		}
	}
	binding := Binding{
		Endpoint:        s.Config.Endpoint,
		Bucket:          bucket,
		Region:          s.Config.Region,
		AccessKeyID:     s.AccessKeyID,
		SecretAccessKey: s.SecretAccessKey,
		ForcePathStyle:  s.Config.ForcePathStyle,
		CACert:          s.CACert,
	}
	if s.Admin == nil {
		// The Connection says the store mints no credentials: the bucket is
		// the isolation, and the application gets the Connection's own.
		return binding, nil
	}
	accessKey := AccessKeyFor(bucket)
	secretKey, err := newSecretKey()
	if err != nil {
		return Binding{}, err
	}
	if err := s.Admin.PutPolicy(ctx, PolicyName(bucket), bucketPolicy(bucket)); err != nil {
		return Binding{}, fmt.Errorf("writing the policy of bucket %s: %w", bucket, err)
	}
	if err := s.Admin.PutUser(ctx, accessKey, secretKey); err != nil {
		return Binding{}, fmt.Errorf("issuing the credential of bucket %s: %w", bucket, err)
	}
	if err := s.Admin.AttachPolicy(ctx, PolicyName(bucket), accessKey); err != nil {
		return Binding{}, fmt.Errorf("scoping the credential of bucket %s: %w", bucket, err)
	}
	binding.AccessKeyID = accessKey
	binding.SecretAccessKey = secretKey
	return binding, nil
}

// policyDocument is an IAM policy as MinIO and S3 read it.
type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

type policyStatement struct {
	Effect    string           `json:"Effect"`
	Principal *policyPrincipal `json:"Principal,omitempty"`
	Action    []string         `json:"Action"`
	Resource  []string         `json:"Resource"`
}

type policyPrincipal struct {
	AWS []string `json:"AWS"`
}

// bucketPolicy is what a claim's credential may do: everything an
// application does with objects in its own bucket, and nothing to the
// bucket itself — not delete it, not change its policy, not read any
// other. The list is written out rather than s3:* so that the boundary is
// legible.
func bucketPolicy(bucket string) []byte {
	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{
			{
				Effect: "Allow",
				Action: []string{
					"s3:GetBucketLocation",
					"s3:GetBucketVersioning",
					"s3:ListBucket",
					"s3:ListBucketVersions",
					"s3:ListBucketMultipartUploads",
				},
				Resource: []string{"arn:aws:s3:::" + bucket},
			},
			{
				Effect: "Allow",
				Action: []string{
					"s3:GetObject",
					"s3:GetObjectVersion",
					"s3:PutObject",
					"s3:DeleteObject",
					"s3:DeleteObjectVersion",
					"s3:AbortMultipartUpload",
					"s3:ListMultipartUploadParts",
				},
				Resource: []string{"arn:aws:s3:::" + bucket + "/*"},
			},
		},
	}
	raw, _ := json.Marshal(doc)
	return raw
}

// anonymousReadPolicy lets anyone read the bucket's objects, and nothing
// else: no listing, no writing.
func anonymousReadPolicy(bucket string) []byte {
	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{{
			Effect:    "Allow",
			Principal: &policyPrincipal{AWS: []string{"*"}},
			Action:    []string{"s3:GetObject"},
			Resource:  []string{"arn:aws:s3:::" + bucket + "/*"},
		}},
	}
	raw, _ := json.Marshal(doc)
	return raw
}
