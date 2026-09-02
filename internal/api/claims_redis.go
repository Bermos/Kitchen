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

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/cache"
)

// The redis half of the claim API: a cache or a queue from a cache-capable
// Connection. The field that matters is `usage`, and this layer checks that
// it is one of the two and nothing else — whether the connection's provider
// can honour it is the provisioner's answer, and it lands on the claim.

// redisClaimShaper is the claimShaper for type redis.
type redisClaimShaper struct{}

func (redisClaimShaper) fields() []claimField {
	return []claimField{{
		name:  "redis",
		set:   func(body *createClaimRequest) bool { return body.Redis != nil },
		lacks: "no eviction policy, no memory limit, no version and no tenancy",
	}}
}

// config is spec.config as this API writes it for a redis claim.
func (redisClaimShaper) config(
	w http.ResponseWriter,
	body *createClaimRequest,
	_ *kitchenv1alpha1.Project,
) (*runtime.RawExtension, bool) {
	redis, ok := validRedisConfig(w, body.Redis)
	if !ok {
		return nil, false
	}
	if redis == nil {
		return nil, true
	}
	raw, err := json.Marshal(struct {
		Redis *kitchenv1alpha1.RedisConfig `json:"redis,omitempty"`
	}{Redis: redis})
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

func (redisClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	view.Redis = redisOf(claim)
}

// deletionOutcome says what goes and what stays, for the shape this claim
// was actually served by. They are genuinely different sentences: destroying
// a server of its own destroys a process, and destroying a tenancy deletes
// the keys under one prefix in a server other projects keep using.
func (redisClaimShaper) deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string {
	shared := claim.Status.Tenancy == string(cache.TenancyShared)
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		if shared {
			return "the keys under this claim's prefix are deleted and its user is removed; no other tenant " +
				"of the server is touched"
		}
		return "the instance and everything in it are destroyed"
	}
	if shared {
		return "the keys under this claim's prefix are kept and its user is removed, so nothing can read " +
			"them until a claim of the same name is created again"
	}
	return "the instance is kept, with whatever is in it"
}

// claimRedisView is what the claim asked its instance to be, as it answered
// it.
type claimRedisView struct {
	Usage     string `json:"usage,omitempty"`
	MaxMemory string `json:"maxMemory,omitempty"`
	Version   string `json:"version,omitempty"`
	Tenancy   string `json:"tenancy,omitempty"`
}

// redisOf is the claim's cache requirements, and nothing at all for a claim
// that asked for nothing in particular.
func redisOf(claim *kitchenv1alpha1.ResourceClaim) *claimRedisView {
	cfg := claim.Redis()
	if cfg.Usage == "" && cfg.MaxMemory == "" && cfg.Version == "" && cfg.Tenancy == "" {
		return nil
	}
	return &claimRedisView{
		Usage:     cfg.Usage,
		MaxMemory: cfg.MaxMemory,
		Version:   cfg.Version,
		Tenancy:   cfg.Tenancy,
	}
}

// validRedisConfig checks the shape of what a redis claim asks of its
// instance, and normalizes it: an empty block is nothing rather than an
// empty object on the spec.
//
// Only shape. Whether the connection's provider can *honour* a usage is its
// own answer — an external server configured to evict refuses a queue — and
// it lands on the claim's status as a failure naming what could not be
// supplied.
func validRedisConfig(
	w http.ResponseWriter,
	cfg *kitchenv1alpha1.RedisConfig,
) (*kitchenv1alpha1.RedisConfig, bool) {
	if cfg == nil {
		return nil, true
	}
	out := kitchenv1alpha1.RedisConfig{
		Usage:     strings.TrimSpace(cfg.Usage),
		MaxMemory: strings.TrimSpace(cfg.MaxMemory),
		Version:   strings.TrimSpace(cfg.Version),
		Tenancy:   strings.TrimSpace(cfg.Tenancy),
	}
	if out.Usage != "" && !cache.Usage(out.Usage).Known() {
		badRequest(w, "redis.usage is %s or %s (got %q): a cache may evict what it holds when it fills up and "+
			"a queue may not, and an application that gets the wrong one loses work without being told",
			cache.UsageCache, cache.UsageQueue, cfg.Usage)
		return nil, false
	}
	if out.MaxMemory != "" {
		quantity, err := resource.ParseQuantity(out.MaxMemory)
		if err != nil {
			badRequest(w, "redis.maxMemory is a Kubernetes quantity — \"512Mi\" (got %q): %s",
				cfg.MaxMemory, err.Error())
			return nil, false
		}
		if quantity.Sign() <= 0 {
			badRequest(w, "redis.maxMemory must be more than nothing (got %q)", cfg.MaxMemory)
			return nil, false
		}
	}
	if out.Version != "" {
		major, err := strconv.Atoi(out.Version)
		if err != nil || major < 1 || major > 99 {
			badRequest(w, "redis.version is a major version and nothing else — \"8\", not %q. Which majors "+
				"this connection can actually run is the connection's answer, and one it cannot run fails "+
				"the claim with the list", cfg.Version)
			return nil, false
		}
	}
	if out.Tenancy != "" && !cache.Tenancy(out.Tenancy).Known() {
		badRequest(w, "redis.tenancy is %s or %s (got %q): %s is a keyspace of this claim's own in a server "+
			"the platform already runs, reached with a user and a key prefix of its own; %s is a server of "+
			"this claim's own, which costs the cluster a pod per environment",
			cache.TenancyShared, cache.TenancyDedicated, cfg.Tenancy, cache.TenancyShared, cache.TenancyDedicated)
		return nil, false
	}
	if out.Usage == "" && out.MaxMemory == "" && out.Version == "" && out.Tenancy == "" {
		return nil, true
	}
	return &out, true
}
