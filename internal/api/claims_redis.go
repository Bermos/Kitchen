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
		lacks: "no eviction policy, no memory limit and no version",
	}}
}

// config is spec.config as this API writes it for a redis claim.
func (redisClaimShaper) config(
	w http.ResponseWriter,
	body *createClaimRequest,
	_ *kitchenv1alpha1.Project,
	_ string,
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

func (redisClaimShaper) deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string {
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		return "the instance and everything in it are destroyed"
	}
	return "the instance is kept, with whatever is in it"
}

// claimRedisView is what the claim asked its instance to be, as it answered
// it.
type claimRedisView struct {
	Usage     string `json:"usage,omitempty"`
	MaxMemory string `json:"maxMemory,omitempty"`
	Version   string `json:"version,omitempty"`
}

// redisOf is the claim's cache requirements, and nothing at all for a claim
// that asked for nothing in particular.
func redisOf(claim *kitchenv1alpha1.ResourceClaim) *claimRedisView {
	cfg := claim.Redis()
	if cfg.Usage == "" && cfg.MaxMemory == "" && cfg.Version == "" {
		return nil
	}
	return &claimRedisView{Usage: cfg.Usage, MaxMemory: cfg.MaxMemory, Version: cfg.Version}
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
	if out.Usage == "" && out.MaxMemory == "" && out.Version == "" {
		return nil, true
	}
	return &out, true
}
