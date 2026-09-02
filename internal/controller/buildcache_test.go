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
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const cachePrefix = "harbor.example.com/kitchen"

func TestCacheRef(t *testing.T) {
	cases := []struct {
		name     string
		branch   string
		scope    kitchenv1alpha1.BuildCacheScope
		strategy kitchenv1alpha1.BuildStrategy
		want     string
	}{{
		name:  "one cache per project by default",
		want:  cachePrefix + "/shop:buildcache",
		scope: kitchenv1alpha1.BuildCacheScopeProject,
	}, {
		name:     "the lifecycle's cache image is a different format under a different tag",
		scope:    kitchenv1alpha1.BuildCacheScopeProject,
		strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
		want:     cachePrefix + "/shop:buildcache-cnb",
	}, {
		name:   "a branch scope names the branch",
		branch: "main",
		scope:  kitchenv1alpha1.BuildCacheScopeBranch,
		want:   cachePrefix + "/shop:buildcache-main",
	}, {
		// A tag has no room for a slash, and two branches that sanitize to
		// the same thing must not be handed each other's layers.
		name:   "a branch a tag cannot spell keeps a hash of what it was",
		branch: "feat/checkout",
		scope:  kitchenv1alpha1.BuildCacheScopeBranch,
		want:   cachePrefix + "/shop:buildcache-feat-checkout-" + branchHash("feat/checkout"),
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheRef(cachePrefix+"/shop", tc.branch, tc.scope, tc.strategy)
			if got != tc.want {
				t.Fatalf("cacheRef = %q, want %q", got, tc.want)
			}
		})
	}
}

// branchHash is what cacheSlug appends to a branch name it had to change, so
// that two branches which sanitize alike keep their own caches.
func branchHash(branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return hex.EncodeToString(sum[:4])
}

// The name a slash-free branch keeps, untouched: the hash is for names that
// needed changing, not a tax on every one.
func TestCacheSlugLeavesAPlainBranchAlone(t *testing.T) {
	if got := cacheSlug("main"); got != "main" {
		t.Fatalf("cacheSlug(main) = %q", got)
	}
}

// A branch name is unbounded and a tag is not, and the two branches below
// share every character a truncation would keep.
func TestCacheSlugSeparatesLongBranches(t *testing.T) {
	prefix := strings.Repeat("release-", 8)
	first, second := cacheSlug(prefix+"one"), cacheSlug(prefix+"two")
	if first == second {
		t.Fatalf("two branches share a cache: %q", first)
	}
	for _, slug := range []string{first, second} {
		if len(slug) > cacheSlugLimit+9 {
			t.Fatalf("slug %q is %d characters, which will not fit a tag", slug, len(slug))
		}
	}
}

func TestBuildkitCacheArgs(t *testing.T) {
	cold := buildkitCacheArgs(&kitchenv1alpha1.BuildCacheStatus{
		Enabled: true, Ref: cachePrefix + "/shop:buildcache", Mode: kitchenv1alpha1.BuildCacheModeMax,
	})
	joined := strings.Join(cold, " ")
	if strings.Contains(joined, "--import-cache") {
		t.Fatalf("a cold build imports nothing, got %q", joined)
	}
	// Without ignore-error a registry that refuses the cache manifest fails a
	// build whose image is already pushed, which is the one outcome caching
	// must not produce.
	for _, want := range []string{"--export-cache", "mode=max", "image-manifest=true", "ignore-error=true"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("export is missing %q: %q", want, joined)
		}
	}

	warm := strings.Join(buildkitCacheArgs(&kitchenv1alpha1.BuildCacheStatus{
		Enabled: true, Ref: cachePrefix + "/shop:buildcache", Mode: kitchenv1alpha1.BuildCacheModeMin, Warm: true,
	}), " ")
	if !strings.Contains(warm, "--import-cache type=registry,ref="+cachePrefix+"/shop:buildcache") {
		t.Fatalf("a warm build imports the cache, got %q", warm)
	}
	if !strings.Contains(warm, "mode=min") {
		t.Fatalf("the configured mode is what is exported, got %q", warm)
	}

	if args := buildkitCacheArgs(&kitchenv1alpha1.BuildCacheStatus{}); args != nil {
		t.Fatalf("caching off means no flags, got %v", args)
	}
	if args := buildkitCacheArgs(nil); args != nil {
		t.Fatalf("no plan means no flags, got %v", args)
	}
}

func TestCnbCacheArgs(t *testing.T) {
	ref := cachePrefix + "/shop:buildcache-cnb"
	args := cnbCacheArgs(&kitchenv1alpha1.BuildCacheStatus{Enabled: true, Ref: ref})
	if len(args) != 1 || args[0] != "-cache-image="+ref {
		t.Fatalf("cnbCacheArgs = %v, want the one cache image flag", args)
	}
	if args := cnbCacheArgs(&kitchenv1alpha1.BuildCacheStatus{Ref: ref}); args != nil {
		t.Fatalf("caching off means no cache image, got %v", args)
	}
}

// The line a green build leaves on its commit. A cold build is slow for a
// reason, and the commit is where somebody wondering why is looking.
func TestSucceededDescriptionSaysWhatTheCacheDid(t *testing.T) {
	timed := func(cache *kitchenv1alpha1.BuildCacheStatus) *kitchenv1alpha1.Build {
		started := metav1.Now()
		done := metav1.NewTime(started.Add(90e9))
		return &kitchenv1alpha1.Build{Status: kitchenv1alpha1.BuildStatus{
			StartedAt: &started, CompletedAt: &done, Cache: cache,
		}}
	}

	cases := []struct {
		name  string
		build *kitchenv1alpha1.Build
		want  string
	}{{
		name:  "a warm build says so",
		build: timed(&kitchenv1alpha1.BuildCacheStatus{Enabled: true, Warm: true}),
		want:  "image built and pushed in 1m30s, cache warm",
	}, {
		name:  "a cold one says that instead of looking like a regression",
		build: timed(&kitchenv1alpha1.BuildCacheStatus{Enabled: true}),
		want:  "image built and pushed in 1m30s, cache cold",
	}, {
		name:  "caching off is not something every commit needs telling",
		build: timed(&kitchenv1alpha1.BuildCacheStatus{}),
		want:  "image built and pushed in 1m30s",
	}, {
		name:  "an untimed build still reports",
		build: &kitchenv1alpha1.Build{Status: kitchenv1alpha1.BuildStatus{}},
		want:  "the image was built and pushed",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := succeededDescription(tc.build); got != tc.want {
				t.Fatalf("succeededDescription = %q, want %q", got, tc.want)
			}
		})
	}
}

// The mode a build exports under when the platform object says nothing, which
// is what an installation that predates the field has.
func TestCacheModeDefaultsToMax(t *testing.T) {
	if got := cacheMode(""); got != kitchenv1alpha1.BuildCacheModeMax {
		t.Fatalf("cacheMode(\"\") = %q, want max", got)
	}
	if got := cacheMode(kitchenv1alpha1.BuildCacheModeMin); got != kitchenv1alpha1.BuildCacheModeMin {
		t.Fatalf("cacheMode(min) = %q, want min", got)
	}
}
