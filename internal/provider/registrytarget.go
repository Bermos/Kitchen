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
	"encoding/json"
	"fmt"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// RegistryTarget is where a Project's builds push and what they authenticate
// against, read off the Connection its registry points at.
//
// The registry prefix was parsed in two places before — the credential probe
// and the build reconciler — each with its own idea of which part of
// "harbor.example.com/kitchen" is the host builds log in to. They are the same
// question and now have one answer.
type RegistryTarget struct {
	// Prefix is what an image is named under, e.g.
	// "harbor.example.com/kitchen". The build appends the project name and
	// the tag.
	Prefix string
	// Server is the host a build authenticates against, and the key of the
	// entry in the docker config it authenticates with.
	Server string
	// BaseURL is Server with the scheme it is reached over — https unless the
	// configured URL asked for plaintext.
	BaseURL string
}

// Registry resolves where a Project's builds push. Naming a Connection of any
// other provider is an error rather than an empty target: the capability
// matcher should never have handed one over, and a build that pushed nowhere
// would be much harder to read than one that says so.
func Registry(conn *kitchenv1alpha1.Connection) (RegistryTarget, error) {
	if conn.Spec.Provider != registryProvider {
		return RegistryTarget{}, fmt.Errorf(
			"connection %q has provider %q, which stores no images", conn.Name, conn.Spec.Provider)
	}
	var cfg struct {
		URL string `json:"url"`
	}
	if conn.Spec.Config != nil {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			return RegistryTarget{}, fmt.Errorf("invalid dockerRegistry config: %w", err)
		}
	}
	if strings.TrimSpace(cfg.URL) == "" {
		return RegistryTarget{}, fmt.Errorf("dockerRegistry connection %q has no config.url", conn.Name)
	}
	return parseRegistryURL(cfg.URL)
}

// registryProvider is the one provider that stores images today.
const registryProvider = "dockerRegistry"

// parseRegistryURL splits the prefix images are pushed under into the parts a
// build needs: "harbor.example.com/kitchen" authenticates against
// harbor.example.com over https. An explicit http:// prefix is honoured — a
// plaintext registry is unusual but real, and it is what tests use.
func parseRegistryURL(raw string) (RegistryTarget, error) {
	raw = strings.TrimSpace(raw)
	scheme := "https"
	if strings.HasPrefix(raw, "http://") {
		scheme = "http"
	}
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.Trim(raw, "/")

	server, _, _ := strings.Cut(raw, "/")
	if server == "" {
		return RegistryTarget{}, fmt.Errorf("registry url %q names no host", raw)
	}
	return RegistryTarget{Prefix: raw, Server: server, BaseURL: scheme + "://" + server}, nil
}

// RegistrySignatureConfig is what a dockerRegistry Connection declares about
// the signatures on the images pulled through it (#309).
//
// It lives in the Connection's free-form config rather than in a typed field
// because that is where every other registry setting lives, and because it is
// a property of the *registry account* rather than of any one image: a
// Connection that reaches one vendor's registry has one expected signer, and
// saying so once is better than repeating it on every image source that pulls
// through it. What an image declares for itself wins over this.
type RegistrySignatureConfig struct {
	// Identity the signature must name, empty for any signer.
	Identity string `json:"identity,omitempty"`
	// Issuer narrows that identity to one OIDC issuer.
	Issuer string `json:"issuer,omitempty"`
	// PublicKeySecret names a Secret in the platform namespace holding the
	// verification key under `public.pem`. It is the only thing that can
	// make a signature `verified` — see attestation.VerifyUpstream.
	PublicKeySecret string `json:"publicKeySecret,omitempty"`
}

// RegistrySignature reads that declaration off a Connection. A Connection of
// another provider, or one that declares nothing, answers the zero value —
// which is "look, record what you find, verify nothing", the honest default.
func RegistrySignature(conn *kitchenv1alpha1.Connection) RegistrySignatureConfig {
	if conn == nil || conn.Spec.Provider != registryProvider || conn.Spec.Config == nil {
		return RegistrySignatureConfig{}
	}
	var cfg struct {
		Signature RegistrySignatureConfig `json:"signature"`
	}
	if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
		return RegistrySignatureConfig{}
	}
	return cfg.Signature
}
