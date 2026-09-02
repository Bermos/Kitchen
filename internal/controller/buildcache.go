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

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/provider"
)

// Where a build's layers live between builds.
//
// The cache is a manifest in the registry the project already pushes to,
// beside the image and under the same credential — which is the whole reason
// this needs no infrastructure and is on by default. BuildKit writes a cache
// manifest, the buildpacks lifecycle writes a cache image; the two formats
// have nothing to say to each other, so they are kept under different tags and
// a project that changes strategy starts cold once rather than reading
// something it cannot use.

const (
	// cacheTagPrefix names every tag this file writes, so that a cache tag is
	// recognisable as one in a registry listing next to the image tags, which
	// are commit shas.
	cacheTagPrefix = "buildcache"

	// cnbCacheTagInfix separates the buildpacks lifecycle's cache image from
	// BuildKit's cache manifest. They are different formats under the same
	// scope, and a lifecycle handed BuildKit's cache fails to read it.
	cnbCacheTagInfix = "cnb"

	// cacheSlugLimit bounds the branch part of a scoped cache tag. A tag may
	// be 128 characters; a branch name has no such bound, and one truncated
	// to fit carries a hash of the whole so two long branches that share a
	// prefix do not share a cache.
	cacheSlugLimit = 40
)

// CacheProbe answers whether a cache exists in the registry. It is an
// interface for the same reason ArtifactAttester is: the reconciler is tested
// against no registry at all, and a probe that has to reach one would make
// every build test a network call.
type CacheProbe interface {
	// Exists reports whether a manifest is published under ref. A registry
	// that cannot be reached is an error, which is not the same answer as
	// "no cache": the first means nothing is known, the second that the next
	// build will be the one to fill it.
	Exists(ctx context.Context, ref string) (bool, error)
}

// CacheProbeFactory builds the probe for one registry out of the docker config
// that registry is pushed to with — the build's own credential, because a
// cache the build cannot write is not a cache it has.
type CacheProbeFactory func(dockerConfig []byte, target provider.RegistryTarget) (CacheProbe, error)

// registryCacheProbe asks the real registry.
type registryCacheProbe struct {
	target provider.RegistryTarget
	auth   authn.Authenticator
}

func (p *registryCacheProbe) Exists(ctx context.Context, ref string) (bool, error) {
	options := []name.Option{}
	if strings.HasPrefix(p.target.BaseURL, "http://") {
		options = append(options, name.Insecure)
	}
	reference, err := name.ParseReference(ref, options...)
	if err != nil {
		return false, fmt.Errorf("%q is not a registry reference: %w", ref, err)
	}
	_, err = remote.Head(reference, remote.WithContext(ctx), remote.WithAuth(p.auth))
	if err == nil {
		return true, nil
	}
	// A cache that is not there is the ordinary case — every project has a
	// first build — and it is not an error to report upwards.
	var status *transport.Error
	if errors.As(err, &status) && (status.StatusCode == http.StatusNotFound || status.StatusCode == http.StatusForbidden) {
		return false, nil
	}
	return false, err
}

// defaultCacheProbe talks to the real registry with the build's credential.
func defaultCacheProbe(dockerConfig []byte, target provider.RegistryTarget) (CacheProbe, error) {
	auth, err := attestation.AuthFromDockerConfig(dockerConfig, target.Server)
	if err != nil {
		return nil, err
	}
	return &registryCacheProbe{target: target, auth: auth}, nil
}

// planCache decides what the build does about the layer cache, and answers the
// status that decision is published as.
//
// It answers something for every build, including one that caches nothing:
// "there was no cache and here is why" is the reading the feature exists to
// make possible, and an absent field would leave a slow build looking like a
// regression.
func (r *BuildReconciler) planCache(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	spec kitchenv1alpha1.BuildCacheSpec,
	target buildTarget,
	// plan is the image this cache is for. A unit builds several, each from
	// its own directory with its own layers, so each caches into its own
	// image repository — one shared cache tag would have four builds
	// overwriting each other's layers every commit and reporting a warm
	// cache none of them could use.
	plan buildPlan,
) *kitchenv1alpha1.BuildCacheStatus {
	if spec.Enabled != nil && !*spec.Enabled {
		return &kitchenv1alpha1.BuildCacheStatus{}
	}

	status := &kitchenv1alpha1.BuildCacheStatus{
		Enabled: true,
		Ref:     cacheRef(plan.Repository, build.Spec.Git.Branch, spec.Scope, plan.Strategy),
	}
	if plan.Strategy != kitchenv1alpha1.BuildStrategyBuildpacks {
		status.Mode = cacheMode(spec.Mode)
	}

	probe, err := r.cacheProbe(ctx, target)
	if err != nil {
		// Nothing is known about the cache, which is not a reason to build
		// without one: BuildKit and the lifecycle both survive being pointed
		// at a cache that turns out not to be there.
		logf.FromContext(ctx).Info("the layer cache could not be probed",
			"build", build.Name, "ref", status.Ref, "cause", err.Error())
		return status
	}
	warm, err := probe.Exists(ctx, status.Ref)
	if err != nil {
		logf.FromContext(ctx).Info("the layer cache could not be probed",
			"build", build.Name, "ref", status.Ref, "cause", err.Error())
		return status
	}
	status.Warm = warm
	if warm {
		return status
	}

	// Cold. Either this scope has never been built, or the last build
	// exported a cache the registry did not keep — which is what a registry
	// that does not implement the cache manifest media types looks like from
	// the outside, since BuildKit is told to treat the failed export as a
	// warning rather than as a failed build.
	if r.cacheWasRefused(ctx, build, project, status.Ref) {
		return &kitchenv1alpha1.BuildCacheStatus{
			Ref: status.Ref,
			Message: "the registry did not keep the cache the last build exported to " + status.Ref +
				", so this build was run without one; the next build tries again, " +
				"and kitchen.builds.cache.enabled=false stops it trying",
		}
	}
	status.Message = "nothing had been cached under " + status.Ref + " yet, so this build had nothing to reuse"
	return status
}

// cacheWasRefused reports whether the project's last finished build exported a
// cache to this same ref and the cache is not there now.
//
// That is as close to a capability check as a registry allows: whether it
// accepts a cache manifest cannot be asked, only attempted. The consequence is
// deliberately not sticky — the next build exports again, so an installation
// that moves to a registry which does keep caches recovers on its own, and one
// that never will pays a failed export it is told about every other build
// rather than a failed build ever.
func (r *BuildReconciler) cacheWasRefused(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	ref string,
) bool {
	builds := &kitchenv1alpha1.BuildList{}
	if err := r.List(ctx, builds, client.InNamespace(build.Namespace)); err != nil {
		return false
	}
	previous := make([]kitchenv1alpha1.Build, 0, len(builds.Items))
	for _, candidate := range builds.Items {
		if candidate.Name == build.Name || candidate.Spec.ProjectRef.Name != project.Name {
			continue
		}
		if candidate.Status.Phase != kitchenv1alpha1.BuildSucceeded || candidate.Status.Cache == nil {
			continue
		}
		previous = append(previous, candidate)
	}
	if len(previous) == 0 {
		return false
	}
	sort.Slice(previous, func(i, j int) bool {
		return previous[j].CreationTimestamp.Before(&previous[i].CreationTimestamp)
	})
	last := previous[0].Status.Cache
	return last.Enabled && last.Ref == ref
}

// cacheProbe resolves how to ask the registry what it is holding, with the
// credential the build itself pushes under.
func (r *BuildReconciler) cacheProbe(ctx context.Context, target buildTarget) (CacheProbe, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{
		Namespace: target.Connection.Namespace,
		Name:      target.Connection.Spec.CredentialsSecretRef.Name,
	}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	factory := r.CacheProbes
	if factory == nil {
		factory = defaultCacheProbe
	}
	return factory(secret.Data[corev1.DockerConfigJsonKey], target.Registry)
}

// cacheMode is the export mode, defaulted. An installation that predates the
// field, or a Kitchen singleton that could not be read, gets the mode that
// makes the cache worth having.
func cacheMode(mode kitchenv1alpha1.BuildCacheMode) kitchenv1alpha1.BuildCacheMode {
	if mode == kitchenv1alpha1.BuildCacheModeMin {
		return kitchenv1alpha1.BuildCacheModeMin
	}
	return kitchenv1alpha1.BuildCacheModeMax
}

// cacheRef is where the cache for one image lives: a tag in that image's own
// repository, so it is covered by the same credential, the same retention and
// the same registry quota as the images beside it — and so a unit's several
// workloads cache separately, since they share no layers and would otherwise
// share a tag.
func cacheRef(
	repository, branch string,
	scope kitchenv1alpha1.BuildCacheScope,
	strategy kitchenv1alpha1.BuildStrategy,
) string {
	tag := cacheTagPrefix
	if strategy == kitchenv1alpha1.BuildStrategyBuildpacks {
		tag += "-" + cnbCacheTagInfix
	}
	if scope == kitchenv1alpha1.BuildCacheScopeBranch {
		tag += "-" + cacheSlug(branch)
	}
	return repository + ":" + tag
}

// cacheSlug turns a branch name into something a registry accepts as part of a
// tag. Anything outside the tag alphabet becomes a dash, and a name that had
// to be changed or shortened carries a hash of the original so that
// `feat/checkout` and `feat-checkout` are not handed each other's layers.
func cacheSlug(branch string) string {
	var slug strings.Builder
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			slug.WriteRune(r)
		default:
			slug.WriteRune('-')
		}
	}
	clean := strings.Trim(slug.String(), "-.")
	if clean == branch && len(clean) <= cacheSlugLimit {
		return clean
	}
	sum := sha256.Sum256([]byte(branch))
	if len(clean) > cacheSlugLimit {
		clean = clean[:cacheSlugLimit]
	}
	if clean == "" {
		clean = "branch"
	}
	return clean + "-" + hex.EncodeToString(sum[:4])
}

// buildkitCacheArgs are the flags that make a BuildKit build reuse and leave
// behind layers.
//
// Three attributes on the export are not decoration. `image-manifest` and
// `oci-mediatypes` write the cache as an ordinary OCI image manifest, which is
// what a registry that rejects BuildKit's default cache manifest will accept —
// the difference between caching working on a plain registry and not.
// `ignore-error` is what keeps a registry that refuses it anyway from failing a
// build whose image was already pushed: the export is the last thing BuildKit
// does, and without this a rejected cache manifest is a red build over an
// image that exists.
//
// The import is only passed for a cache that is known to be there. BuildKit
// tolerates one that is not, but says so at length in a build log somebody is
// reading to find out why their build failed.
func buildkitCacheArgs(cache *kitchenv1alpha1.BuildCacheStatus) []string {
	if cache == nil || !cache.Enabled {
		return nil
	}
	args := []string{}
	if cache.Warm {
		args = append(args, "--import-cache", "type=registry,ref="+cache.Ref)
	}
	return append(args, "--export-cache", strings.Join([]string{
		"type=registry",
		"ref=" + cache.Ref,
		"mode=" + string(cacheMode(cache.Mode)),
		"image-manifest=true",
		"oci-mediatypes=true",
		"ignore-error=true",
	}, ","))
}
