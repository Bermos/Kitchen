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

// Package objectstore is the contract of the objectStore claim type: a
// bucket an application can put a file in that it did not build into its
// image — user uploads, generated exports, anything it writes and expects to
// read back — from an objectStore-capable Connection.
//
// It is shaped after internal/provider/database on purpose: a Binding whose
// fields become the keys of the claim's Secret, typed Requirements a
// provisioner refuses before creating anything when it cannot honour them,
// and a Provisioner whose identifiers are opaque strings, so that the
// implementation does not have to be a cloud API. It is the most
// substitutable contract the platform has, because MinIO, AWS S3, Cloudflare
// R2, Ceph RGW and Backblaze all speak the same API: an application written
// against the binding moves between them without changing.
//
// One implementation ships, and it serves both halves of the issue: the S3
// provisioner talks to whatever the Connection's endpoint is — the MinIO the
// chart runs in this cluster, or a store somebody else runs. What differs
// between the two is who created the Connection, and the one thing the
// bundled store cannot do, which is publish a bucket to the internet.
//
// Isolation is a bucket per claim and, where the store speaks the MinIO
// admin API, a user and a policy per bucket: never a prefix in a shared
// bucket, because a prefix is not an isolation boundary.
package objectstore

import (
	"context"
	"errors"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// ErrUnsupportedProvider is returned by Default for a provider this package
// has no provisioner for.
var ErrUnsupportedProvider = errors.New("unsupported object store provider")

// ErrUnsatisfiable marks a claim the provisioner cannot honour as asked: a
// publicly readable bucket in a store nothing outside the cluster can reach,
// versioning at a store that does not implement it, a size limit where there
// is no admin API to set one. Nothing was created and retrying without
// changing the claim refuses again, so the reconciler lands it on the claim
// as a failure with the message attached.
var ErrUnsatisfiable = errors.New("claim cannot be satisfied")

// ErrNotReady marks a store that exists and is not answering yet — the
// bundled one while its pod is still starting. Nothing is wrong; the
// reconciler holds the claim Pending and looks again.
var ErrNotReady = errors.New("not ready yet")

// DataProvenance is the provisioner's declaration of what the objects in a
// bucket derive from, on the same terms as the database contract's: the
// platform records it, attests it, and the policy engine enforces it.
type DataProvenance string

const (
	// ProvenanceProduction: the bucket is production's own.
	ProvenanceProduction DataProvenance = "production"
	// ProvenanceSynthetic: a fresh, empty bucket that never held production
	// objects — every preview's.
	ProvenanceSynthetic DataProvenance = "synthetic"
)

// Binding is everything an application needs to reach its bucket. The fields
// become the keys of the claim's binding Secret verbatim.
type Binding struct {
	// Endpoint is the store's URL including the scheme —
	// http://kitchen-objectstore.kitchen-system.svc.cluster.local:9000, or
	// https://s3.eu-central-1.amazonaws.com.
	Endpoint string
	// Bucket the credential is scoped to.
	Bucket string
	// Region the store reports; a formality every S3 client insists on.
	Region string
	// AccessKeyID and SecretAccessKey authenticate the application.
	AccessKeyID     string
	SecretAccessKey string
	// ForcePathStyle says whether the bucket goes in the path rather than
	// the host name. It is not decoration: MinIO needs it and AWS does not,
	// and an application that guesses wrong fails on every request.
	ForcePathStyle bool
}

// The keys of the binding Secret, spelled once.
const (
	BindingKeyEndpoint        = "endpoint"
	BindingKeyBucket          = "bucket"
	BindingKeyRegion          = "region"
	BindingKeyAccessKeyID     = "accessKeyId"
	BindingKeySecretAccessKey = "secretAccessKey"
	BindingKeyForcePathStyle  = "forcePathStyle"
)

// Data is the binding as the Secret carries it.
func (b Binding) Data() map[string][]byte {
	pathStyle := "false"
	if b.ForcePathStyle {
		pathStyle = "true"
	}
	return map[string][]byte{
		BindingKeyEndpoint:        []byte(b.Endpoint),
		BindingKeyBucket:          []byte(b.Bucket),
		BindingKeyRegion:          []byte(b.Region),
		BindingKeyAccessKeyID:     []byte(b.AccessKeyID),
		BindingKeySecretAccessKey: []byte(b.SecretAccessKey),
		BindingKeyForcePathStyle:  []byte(pathStyle),
	}
}

// Instance is one provisioned bucket with its credential.
type Instance struct {
	// ID is what the other operations address the bucket by — the bucket's
	// name at the store, since a bucket is found again by nothing else.
	ID string
	// Name is what the provisioner called the bucket, recorded on the claim
	// so that a bucket made under one naming rule keeps its name when the
	// rule changes; see internal/provider/naming.
	Name string
	// Binding reaches the bucket.
	Binding Binding
	// Provenance declares what the bucket's objects derive from.
	Provenance DataProvenance
	// Region is where the store reports the bucket to be.
	Region string
}

// Branch is a preview's own bucket: empty, shaped like the parent, torn down
// with the preview.
type Branch struct {
	ID         string
	Binding    Binding
	Provenance DataProvenance
}

// Provisioner is an object store bound to one Connection.
//
// All operations are idempotent by name or tolerant of absence: Provision and
// CreateBranch find the bucket under the name when one is already there and
// re-issue its credential, and the two Delete operations treat already-absent
// as success. Provision and CreateBranch may answer ErrNotReady while the
// store is coming up.
type Provisioner interface {
	// Provision creates (or finds) the claim's bucket and returns its
	// binding. The provisioner names it — naming.Resolve out of the claim's
	// project and S3's own 63 characters — rather than being told what to
	// call it.
	Provision(ctx context.Context, res naming.Resource) (Instance, error)
	// Deprovision removes the bucket, everything in it, and the credential
	// scoped to it.
	Deprovision(ctx context.Context, instanceID string) error
	// CreateBranch creates (or finds) a preview's own bucket beside the
	// instance's and returns its binding.
	CreateBranch(ctx context.Context, instanceID, name string) (Branch, error)
	// DeleteBranch removes a preview's bucket, its objects and its
	// credential.
	DeleteBranch(ctx context.Context, instanceID, branchID string) error
}

// Requirements are what a claim asks of its bucket beyond a name. They are
// typed rather than a bag of strings because the typing is what buys the
// refusal: a provisioner reads each one and either honours it or answers
// ErrUnsatisfiable before it has created anything.
type Requirements struct {
	// Versioning keeps every version of an object rather than the latest.
	Versioning bool
	// PublicRead lets anyone read the bucket's objects without a credential.
	PublicRead bool
	// Size is a Kubernetes quantity ("50Gi") the bucket may not grow past;
	// empty asks for no limit.
	Size string
}

// Empty reports whether the claim asked for nothing in particular.
func (r Requirements) Empty() bool {
	return !r.Versioning && !r.PublicRead && r.Size == ""
}

// CapableProvisioner is a Provisioner that can be asked for requirements. A
// provisioner that does not implement it is asked nothing, and a claim that
// names requirements it cannot carry is refused by the reconciler rather
// than provisioned as though they had not been written down.
type CapableProvisioner interface {
	Provisioner
	// ProvisionWith is Provision with the claim's requirements applied,
	// refusing with an error wrapping ErrUnsatisfiable before creating
	// anything when one cannot be honoured.
	ProvisionWith(ctx context.Context, res naming.Resource, req Requirements) (Instance, error)
}

// The provider names the Connection enum admits for an object store.
const (
	// ProviderS3 is any S3-compatible store: the MinIO the chart runs in
	// this cluster, a MinIO a team already runs, AWS S3, Cloudflare R2. One
	// name, because the API is one API — what differs is what the store
	// behind the endpoint can do, and the provisioner asks the Connection's
	// config rather than assuming from the name.
	ProviderS3 = "s3"
)

// Declarations is what the object store provider says about itself before
// it has provisioned anything, next to Default so that a provider and its
// declaration are added together.
var Declarations = map[string]contract.Declaration{
	ProviderS3: {
		Preview: contract.PreviewFresh,
		PreviewNote: "a new, empty bucket of the preview's own with its own credential, versioned when " +
			"production's is and torn down with the preview: the branch declares provenance synthetic",
		IdleNote: "a bucket is storage and no compute, so there is nothing to park: an idle preview's " +
			"bucket costs what its objects cost and not a byte more",
	},
}

// Options is what a Provisioner is built from.
type Options struct {
	// Connection the claim provisions through; its config names the
	// endpoint, the region and how the bucket is addressed.
	Connection *kitchenv1alpha1.Connection
	// AccessKeyID and SecretAccessKey are the Connection's own credential,
	// read from its Secret. Against a MinIO they are what mints every
	// claim's scoped credential; against a store with no admin API they are
	// what every claim is handed.
	AccessKeyID     string
	SecretAccessKey string
}

// Factory builds a Provisioner for a Connection.
type Factory func(opts Options) (Provisioner, error)
