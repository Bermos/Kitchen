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

package policy

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Where bundles come from. Two sources, both addressed by digest:
//
//   - the built-in default bundle, compiled into the operator;
//   - ConfigMaps in the platform namespace labelled with BundleLabel, each
//     key a file of the bundle — which is how the chart, or an operator with
//     kubectl at bootstrap, distributes a bundle of the institution's own.
//
// An environment's requirements pin a digest, never a name, so a ConfigMap
// edited in place is a *new* bundle with a new digest: nothing that pinned
// the old one moves until its owners repin. The old content survives edits
// and deletions in the decision store (`policy_bundles`), persisted on first
// use, which is what replay reads.

// BundleLabel marks a ConfigMap in the platform namespace as a policy
// bundle. The value must be "true"; each key of the ConfigMap is a file name
// (.rego modules, optionally data.json) and each value its content.
const BundleLabel = "kitchen.bermos.dev/policy-bundle"

// SourceBuiltIn names the compiled-in bundle in listings and stored records.
const SourceBuiltIn = "built-in"

// ErrBundleNotFound reports a digest no available bundle answers to. It is a
// sentinel so callers can distinguish "not there" from "could not look".
var ErrBundleNotFound = errors.New("no policy bundle with that digest is available")

// Info describes one available bundle: what it is, where it came from, and
// which rules it can fire.
type Info struct {
	Digest string
	// Source is SourceBuiltIn or `configmap/<name>`.
	Source string
	Rules  []string
	Bundle Bundle
}

// Resolver finds bundles by digest across every source.
type Resolver struct {
	// Client reads the labelled ConfigMaps. Nil serves the built-in bundle
	// alone, which is what a test without a cluster gets.
	Client client.Client
	// Namespace is the platform namespace the ConfigMaps live in.
	Namespace string
}

// List is every bundle currently available, built-in first and then the
// ConfigMap ones sorted by name. It is what the API's bundle listing serves,
// so an environment owner can discover a digest to require.
func (r *Resolver) List(ctx context.Context) ([]Info, error) {
	builtIn := DefaultBundle()
	infos := []Info{{
		Digest: Digest(builtIn),
		Source: SourceBuiltIn,
		Rules:  RuleIDs(builtIn),
		Bundle: builtIn,
	}}
	if r.Client == nil {
		return infos, nil
	}

	configMaps := &corev1.ConfigMapList{}
	if err := r.Client.List(ctx, configMaps,
		client.InNamespace(r.Namespace), client.MatchingLabels{BundleLabel: "true"}); err != nil {
		return nil, fmt.Errorf("the policy bundle ConfigMaps could not be listed: %w", err)
	}
	sort.Slice(configMaps.Items, func(i, j int) bool {
		return configMaps.Items[i].Name < configMaps.Items[j].Name
	})
	for _, configMap := range configMaps.Items {
		bundle := Bundle{}
		for path, content := range configMap.Data {
			bundle[path] = content
		}
		infos = append(infos, Info{
			Digest: Digest(bundle),
			Source: "configmap/" + configMap.Name,
			Rules:  RuleIDs(bundle),
			Bundle: bundle,
		})
	}
	return infos, nil
}

// Resolve answers the bundle a digest names, or ErrBundleNotFound wrapped in
// an error that says where it looked.
func (r *Resolver) Resolve(ctx context.Context, digest string) (Info, error) {
	infos, err := r.List(ctx)
	if err != nil {
		return Info{}, err
	}
	for _, info := range infos {
		if info.Digest == digest {
			return info, nil
		}
	}
	return Info{}, fmt.Errorf(
		"%w: %s matches neither the built-in bundle nor any ConfigMap in %s labelled %s=true",
		ErrBundleNotFound, digest, r.Namespace, BundleLabel)
}
