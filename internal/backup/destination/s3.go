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

package destination

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Config is a bucket at an S3-compatible store, as the spec describes it
// plus the credential the spec only points at.
type S3Config struct {
	Bucket   string
	Prefix   string
	Region   string
	Endpoint string
	// ForcePathStyle addresses the bucket as <endpoint>/<bucket>. Every store
	// reached by IP address, or by a name with no wildcard certificate behind
	// it, needs it — MinIO in a cluster is the usual one.
	ForcePathStyle bool
	// ServerSideEncryption is "AES256" or "aws:kms"; KMSKeyID names the key
	// for the latter.
	ServerSideEncryption string
	KMSKeyID             string

	// CABundleFile is a PEM bundle added to the host's own roots for this
	// destination, for a store whose certificate no public authority signed
	// — the object store this platform bundles, served on a `.svc` name from
	// the platform's internal CA (#382). Empty is the host's roots alone,
	// which is every destination on the internet.
	//
	// It is added to the system pool rather than replacing it: one client
	// reaches whichever store the destination names, and a pool holding only
	// a private CA would refuse AWS.
	CABundleFile string

	// AccessKeyID and SecretAccessKey are the static credential, where there
	// is one. Both empty is the ambient chain — IRSA, EKS Pod Identity, an
	// instance role — which is the better answer where it is available,
	// because there is then no long-lived key anywhere to leak. Reaching for
	// the SDK's credential chain rather than signing requests by hand is the
	// whole reason this package takes the dependency.
	AccessKeyID     string
	SecretAccessKey string
}

// S3 is a Destination at any S3-compatible store.
type S3 struct {
	config S3Config
	client *s3.Client
}

var _ Destination = (*S3)(nil)

// NewS3 builds the client. It resolves credentials once, here, so that a
// destination which cannot authenticate says so when it is configured rather
// than at 02:00 on the night it was first needed — which is also what
// POST /platform/backup/runs exists to let somebody find out on purpose.
func NewS3(ctx context.Context, config S3Config) (*S3, error) {
	if strings.TrimSpace(config.Bucket) == "" {
		return nil, fmt.Errorf("this destination names no bucket")
	}

	options := []func(*awsconfig.LoadOptions) error{}
	if config.Region != "" {
		options = append(options, awsconfig.WithRegion(config.Region))
	} else {
		// Every S3-compatible store insists on a region even where it means
		// nothing, and a client with none refuses to sign at all. us-east-1
		// is the conventional answer, and it is what the bundled MinIO
		// reports for its own buckets.
		options = append(options, awsconfig.WithRegion("us-east-1"))
	}
	if config.AccessKeyID != "" || config.SecretAccessKey != "" {
		options = append(options, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(config.AccessKeyID, config.SecretAccessKey, "")))
	}
	if config.CABundleFile != "" {
		transport, err := trustBundle(config.CABundleFile)
		if err != nil {
			return nil, err
		}
		options = append(options, awsconfig.WithHTTPClient(&http.Client{Transport: transport}))
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("the destination's credentials could not be resolved: %w", err)
	}

	client := s3.NewFromConfig(loaded, func(o *s3.Options) {
		if config.Endpoint != "" {
			o.BaseEndpoint = aws.String(config.Endpoint)
		}
		o.UsePathStyle = config.ForcePathStyle
		// Only send a checksum where the operation requires one. The default
		// adds a trailing CRC32 to every upload, in an aws-chunked encoding
		// that several S3-compatible stores answer 501 to — and the point of
		// the endpoint override is that those stores are the same code path.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return &S3{config: config, client: client}, nil
}

// trustBundle builds the transport that verifies this destination: the host's
// own roots plus the bundle named, which is how a store inside the cluster is
// verified without the client losing every store outside it.
//
// A bundle that cannot be read is an error naming the file. The alternative
// — carrying on with the host's roots — is an upload that fails at 02:00 with
// a certificate error and nothing to say which file was missing.
func trustBundle(file string) (*http.Transport, error) {
	pem, err := os.ReadFile(file) //nolint:gosec // the path is the platform's own, not a request's
	if err != nil {
		return nil, fmt.Errorf("reading the destination's CA bundle %s: %w", file, err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the destination's CA bundle %s holds no certificate", file)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("the default HTTP transport is not one a CA can be added to")
	}
	transport = transport.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return transport, nil
}

// Put uploads one archive.
func (s *S3) Put(ctx context.Context, name string, size int64, body io.Reader) (Object, error) {
	key := s.key(name)
	input := &s3.PutObjectInput{
		Bucket:        aws.String(s.config.Bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String("application/gzip"),
	}
	if s.config.ServerSideEncryption != "" {
		input.ServerSideEncryption = types.ServerSideEncryption(s.config.ServerSideEncryption)
		if s.config.KMSKeyID != "" {
			input.SSEKMSKeyId = aws.String(s.config.KMSKeyID)
		}
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return Object{}, fmt.Errorf("uploading %s to %s: %w", key, s, err)
	}
	return Object{Key: key, Size: size}, nil
}

// List answers everything under the prefix.
func (s *S3) List(ctx context.Context) ([]Object, error) {
	var objects []Object
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.config.Bucket),
		Prefix: aws.String(s.prefix()),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", s, err)
		}
		for _, item := range page.Contents {
			object := Object{Key: aws.ToString(item.Key), Size: aws.ToInt64(item.Size)}
			if item.LastModified != nil {
				object.Modified = item.LastModified.UTC()
			}
			objects = append(objects, object)
		}
	}
	return objects, nil
}

// Get reads at most limit bytes of one object, as one range request.
func (s *S3) Get(ctx context.Context, key string, limit int64) (io.ReadCloser, error) {
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(key),
	}
	if limit > 0 {
		input.Range = aws.String(fmt.Sprintf("bytes=0-%d", limit-1))
	}
	output, err := s.client.GetObject(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("reading %s back from %s: %w", key, s, err)
	}
	return output.Body, nil
}

// Delete removes one object.
func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting %s from %s: %w", key, s, err)
	}
	return nil
}

// String is the destination as a person reads it, and never its credential.
func (s *S3) String() string {
	return "s3://" + s.config.Bucket + "/" + s.prefix()
}

// prefix is the configured prefix as a key prefix: no leading slash, one
// trailing slash unless it is empty.
func (s *S3) prefix() string {
	trimmed := strings.Trim(strings.TrimSpace(s.config.Prefix), "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/"
}

// key is where one archive goes.
func (s *S3) key(name string) string {
	return s.prefix() + name
}
