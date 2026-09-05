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
	"errors"
	"fmt"
	"net/http"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// S3Probe checks an s3 Connection the way an application would use it:
// list the buckets the credential can see. Any S3-compatible store answers
// that, which is what makes it the right question — a store that is down
// and a credential that is wrong read differently, and neither needs the
// store to be a MinIO.
//
// Whether the store is a MinIO matters afterwards: a Connection that keeps
// scopedCredentials (the default) needs the admin API to mint a credential
// per bucket, so the probe asks for it too and, where the store does not
// answer, warns rather than fails — the credential works, the claims will
// not, and the warning names the flag that reconciles the two.
type S3Probe struct {
	// Config is the Connection's: endpoint, region, path style, scoping.
	Config          objectstore.Config
	AccessKeyID     string
	SecretAccessKey string
	// Transport defaults to the package's; tests point it at httptest.
	Transport http.RoundTripper
}

// newS3Probe builds the probe from the Connection's config and the
// two-key Secret internal/api writes for the s3 provider.
func newS3Probe(conn *kitchenv1alpha1.Connection, creds *corev1.Secret) (*S3Probe, error) {
	cfg, err := objectstore.ConfigOf(conn)
	if err != nil {
		return nil, err
	}
	accessKey := string(creds.Data[objectstore.CredentialKeyAccessKeyID])
	secretKey := string(creds.Data[objectstore.CredentialKeySecretAccessKey])
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("credentials secret %q needs both %q and %q", creds.Name,
			objectstore.CredentialKeyAccessKeyID, objectstore.CredentialKeySecretAccessKey)
	}
	return &S3Probe{Config: cfg, AccessKeyID: accessKey, SecretAccessKey: secretKey}, nil
}

// Probe lists buckets, and then — for a Connection that mints credentials —
// asks the admin API whether it is there.
func (p *S3Probe) Probe(ctx context.Context) Result {
	host, secure, err := p.Config.Host()
	if err != nil {
		return unreachableBecause(err.Error())
	}
	// The bundled store's certificate comes from the platform's own CA, which
	// the host's roots have never heard of — so the probe verifies against
	// the bundle the Connection names, exactly as the provisioner does. A
	// probe that trusted less than the provisioner would report a connection
	// healthy that every claim through it then fails on.
	transport := p.Transport
	if transport == nil {
		verified, _, err := p.Config.Verify()
		if err != nil {
			return unreachableBecause(err.Error())
		}
		if verified != nil {
			transport = verified
		}
	}
	creds := credentials.NewStaticV4(p.AccessKeyID, p.SecretAccessKey, "")
	lookup := minio.BucketLookupDNS
	if p.Config.ForcePathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds: creds, Secure: secure, Region: p.Config.Region, BucketLookup: lookup, Transport: transport,
	})
	if err != nil {
		return unreachableBecause(err.Error())
	}

	buckets, err := client.ListBuckets(ctx)
	if err != nil {
		var resp minio.ErrorResponse
		if !errors.As(err, &resp) {
			return unreachable(err)
		}
		switch resp.Code {
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return rejected(fmt.Sprintf("%s rejected the credential: %s", host, resp.Message))
		default:
			return unjudged(fmt.Sprintf("%s answered %s: %s", host, resp.Code, resp.Message))
		}
	}
	result := accepted(fmt.Sprintf("%s accepted the credential; %d bucket(s) visible to it", host, len(buckets)))
	if !p.Config.Scoped() {
		return result
	}

	admin, err := madmin.NewWithOptions(host, &madmin.Options{Creds: creds, Secure: secure, Transport: transport})
	if err != nil {
		return result.withWarnings(fmt.Sprintf("the admin client could not be built: %s", err.Error()))
	}
	if _, err := admin.ServerInfo(ctx); err != nil {
		return result.withWarnings(fmt.Sprintf(
			"%s did not answer the MinIO admin API (%s). The platform mints a credential per bucket through "+
				"it, so a claim through this connection will fail until either the credential is given admin "+
				"rights at the store or config.scopedCredentials is set to false, which hands every claim "+
				"this connection's own credential instead", host, err.Error()))
	}
	return result
}
