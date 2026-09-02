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
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The keys of an s3 Connection's credentials Secret, matching what the REST
// API writes and the probe reads.
const (
	CredentialKeyAccessKeyID     = "accessKeyId"
	CredentialKeySecretAccessKey = "secretAccessKey"
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
