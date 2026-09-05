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
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The keys of an s3 Connection's credentials Secret, matching what the REST
// API writes and the probe reads.
const (
	CredentialKeyAccessKeyID     = "accessKeyId"
	CredentialKeySecretAccessKey = "secretAccessKey"
)

// The keys the chart writes into the bundled store's own Secret beside that
// root credential: where the store answers, on what scheme, what its
// certificate is verified against, and — addressed to the operator rather
// than to any client — the Secret its certificate belongs in.
//
// They are spelled exactly as the telemetry store's are (#382), because one
// controller issues for both: InternalTLSReconciler reads `host` and
// `certificateSecret` out of whichever connection secret the chart wrote, and
// TestEveryStoreSpeaksOneConnectionSecretVocabulary holds the spellings
// together.
const (
	SecretKeyHost   = "host"
	SecretKeyScheme = "scheme"

	// SecretKeyCAFile is where the PEM bundle that must have signed the
	// store's certificate is mounted in the pod reading this. Empty means
	// the host's own roots, which is what an external store with a publicly
	// trusted certificate wants; it never means "do not verify".
	SecretKeyCAFile = "caFile"

	// SecretKeyCertificateSecret names the Secret the store's certificate
	// belongs in, and by naming it asks the operator to fill it from the
	// platform's internal CA. Absent, the operator issues nothing — a
	// bundled store somebody has deliberately left in the clear.
	SecretKeyCertificateSecret = "certificateSecret"
)

// The two schemes the bundled store is reached on.
const (
	SchemeHTTP  = "http"
	SchemeHTTPS = "https"
)

// DefaultRegion is what a store is asked for when the Connection names no
// region. It is what MinIO answers by default and what every S3 client
// accepts as a formality.
const DefaultRegion = "us-east-1"

// Config is the `s3` slice of a Connection's spec.config: where the store
// is and how it is talked to.
type Config struct {
	// Endpoint is the store's URL with its scheme.
	Endpoint string `json:"endpoint"`
	// Region the store's buckets are in; DefaultRegion when empty.
	Region string `json:"region,omitempty"`
	// ForcePathStyle addresses a bucket as a path rather than a host name.
	// MinIO needs it; AWS does not.
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`
	// ScopedCredentials says the platform mints a credential per bucket
	// through the MinIO admin API — a user and a policy scoped to the one
	// bucket — which MinIO speaks and AWS S3 and R2 do not. Nil reads as
	// true: isolation is the default, and a store that cannot give it has
	// to be told so by name, at which point every claim through the
	// Connection is handed the Connection's own credential. A size limit
	// is a quota the same API sets, and is refused without it.
	ScopedCredentials *bool `json:"scopedCredentials,omitempty"`
	// InCluster marks the bundled store: reached at a Service address and
	// nowhere else, so a publicly readable bucket is refused — there is no
	// public to read it. The operator writes it on the Connection it seeds.
	InCluster bool `json:"inCluster,omitempty"`
	// CAFile is the PEM bundle the store's certificate must be signed by,
	// at the path it is mounted in the pod reading this Connection — the
	// operator's own, which is the only process that builds a provisioner.
	// The operator writes it on the Connection it seeds, from the same
	// ConfigMap it publishes the platform's internal CA in.
	//
	// A path rather than the bundle itself, for the reason the telemetry
	// store's connection secret carries one: the bundle belongs to the
	// operator, which mints it, and where a pod sees it is the chart's to
	// decide. Empty verifies against the host's roots, which is what an
	// external store with a publicly trusted certificate wants. There is no
	// value here that means "do not verify".
	CAFile string `json:"caFile,omitempty"`
}

// Scoped is whether the platform mints a credential per bucket.
func (c Config) Scoped() bool { return c.ScopedCredentials == nil || *c.ScopedCredentials }

// Host is the endpoint without its scheme, as the S3 client takes it, and
// whether the scheme was https.
func (c Config) Host() (host string, secure bool, err error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false, fmt.Errorf("endpoint must be an http(s) URL naming the store — "+
			"\"https://s3.eu-central-1.amazonaws.com\" (got %q)", c.Endpoint)
	}
	return u.Host, u.Scheme == "https", nil
}

// Verify builds the transport every client of this store uses, and answers
// the CA bytes it verifies against so that a binding can carry them.
//
// A store reached over https with a CAFile is verified — hostname and chain —
// against that bundle and nothing else: it is the platform's own CA, and the
// host's roots have never heard of it. A store reached over https without one
// is verified against the host's roots, which is the transport the SDK
// already builds, so this answers nil and lets it. There is no third case:
// nothing here turns verification off, and nothing falls back to plaintext.
//
// A bundle that cannot be read is an error rather than a client that carries
// on without it — the connection would then fail at the first request with a
// certificate error naming nothing, where this names the file.
func (c Config) Verify() (*http.Transport, []byte, error) {
	if c.CAFile == "" {
		return nil, nil, nil
	}
	if _, secure, err := c.Host(); err != nil || !secure {
		// A CA against an http endpoint verifies nothing: say so rather than
		// building a transport that is never consulted.
		return nil, nil, nil
	}
	bundle, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("reading the object store's CA bundle %s: %w", c.CAFile, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		return nil, nil, fmt.Errorf("the object store's CA bundle %s holds no certificate", c.CAFile)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, nil, fmt.Errorf("the default HTTP transport is not one this can add a CA to")
	}
	transport = transport.Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	return transport, bundle, nil
}

// ParseConfig reads and checks an s3 Connection's config. It is the one
// reader of the shape, used by the provisioner, the probe and the REST
// API's validation alike, so the three cannot disagree about it.
func ParseConfig(raw []byte) (Config, error) {
	cfg := Config{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("invalid %s config: %w", ProviderS3, err)
		}
	}
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.CAFile = strings.TrimSpace(cfg.CAFile)
	if cfg.Endpoint == "" {
		return Config{}, fmt.Errorf("provider %s needs the store in config.endpoint, e.g. "+
			"{\"config\": {\"endpoint\": \"https://s3.eu-central-1.amazonaws.com\", \"region\": \"eu-central-1\"}}",
			ProviderS3)
	}
	if _, _, err := cfg.Host(); err != nil {
		return Config{}, fmt.Errorf("provider %s: %w", ProviderS3, err)
	}
	if cfg.Region == "" {
		cfg.Region = DefaultRegion
	}
	return cfg, nil
}

// ConfigOf is ParseConfig over a Connection.
func ConfigOf(conn *kitchenv1alpha1.Connection) (Config, error) {
	if conn == nil || conn.Spec.Config == nil {
		return ParseConfig(nil)
	}
	return ParseConfig(conn.Spec.Config.Raw)
}
