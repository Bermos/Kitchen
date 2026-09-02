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

package flows

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTemplatePathClassifiesIdentifiers(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"a numeric key", "/users/12345", "/users/:id"},
		{"a uuid", "/orders/8f3ab2c1-1f2e-4a3b-9c4d-5e6f70819a2b", "/orders/:uuid"},
		{"a short digest", "/objects/8f3ab2c1", "/objects/:hash"},
		{"a commit", "/builds/9a1f0c4e2b7d6538a1f0c4e2b7d65381", "/builds/:hash"},
		{"a ulid", "/events/01ARZ3NDEKTSV4RRFFQ69G5FAV", "/events/:hash"},
		{"a ksuid", "/events/0ujsswThIGTUYm2K8FjOOfXtY1K", "/events/:hash"},
		{"a session token", "/callback/QhK7dLm2Xr9Ts4Vw8Zb3Nc6P", "/callback/:hash"},
		{"a hashed bundle", "/assets/app.8f3ab2c1.js", "/assets/*.js"},
		{"a hashed chunk", "/_next/static/main.4f2a1b9c.chunk.js", "/_next/static/*.js"},

		// A number is valid hex, so the order of the rules is what keeps an id
		// from reading as a digest.
		{"an eight digit key", "/users/12345678", "/users/:id"},

		// Everything a route actually names has to survive, or the route table
		// is a table of placeholders.
		{"a plain route", "/api/v1/users", "/api/v1/users"},
		{"a versioned route", "/v1.2.3/status", "/v1.2.3/status"},
		{"a slug", "/blog/getting-started-2026", "/blog/getting-started-2026"},
		{"a snake case route", "/api/user_profiles", "/api/user_profiles"},
		{"an unhashed asset", "/static/jquery.min.js", "/static/jquery.min.js"},
		{"a favicon", "/favicon.ico", "/favicon.ico"},
		{"a date", "/archive/2026.05.01", "/archive/2026.05.01"},

		{"the root", "/", "/"},
		{"nothing at all", "", "/"},
		{"a doubled separator", "//users//12345", "/users/:id"},
		{"a trailing separator", "/users/", "/users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := templatePath(tc.path); got != tc.want {
				t.Errorf("templatePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestTemplatePathCapsDepthAndSegmentLength(t *testing.T) {
	// Past the depth cap the path is describing a tree somebody is walking,
	// and the tail of it is not worth a series.
	deep := strings.Repeat("/a", maxRouteDepth+4)
	want := strings.Repeat("/a", maxRouteDepth) + overflowRoute
	if got := templatePath(deep); got != want {
		t.Errorf("templatePath(deep) = %q, want %q", got, want)
	}

	// Exactly at the cap nothing is folded: the cap is a limit, not an
	// off-by-one waiting to truncate a legitimate route.
	exact := strings.Repeat("/a", maxRouteDepth)
	if got := templatePath(exact); got != exact {
		t.Errorf("templatePath(exact) = %q, want %q", got, exact)
	}

	// A long segment that matched no rule keeps its beginning, so what it was
	// is still readable, and loses the rest.
	long := strings.Repeat("w", maxSegmentLength+10)
	got := templatePath("/" + long)
	if !strings.HasPrefix(got, "/"+strings.Repeat("w", maxSegmentLength)) || !strings.HasSuffix(got, overflowSegment) {
		t.Errorf("templatePath(long segment) = %q", got)
	}
	if len([]rune(got)) != maxSegmentLength+2 {
		t.Errorf("templatePath(long segment) kept %d runes", len([]rune(got)))
	}
}

func TestSplitURLDropsTheQueryString(t *testing.T) {
	// The query is a privacy rule, not an optimisation: it never reaches a row
	// by any path through this function.
	const secret = "token=hunter2"
	for _, raw := range []string{
		"http://shop.example.com/search?q=shoes&" + secret,
		"http://shop.example.com/search#" + secret,
		// A URL net/url refuses still has to lose its query.
		"http://shop.example.com/search%zz?" + secret,
	} {
		authority, path := splitURL(raw)
		if strings.Contains(path, secret) || strings.Contains(authority, secret) {
			t.Errorf("splitURL(%q) leaked the query: %q %q", raw, authority, path)
		}
		if authority != "shop.example.com" {
			t.Errorf("splitURL(%q) authority = %q", raw, authority)
		}
		if !strings.HasPrefix(path, "/search") {
			t.Errorf("splitURL(%q) path = %q", raw, path)
		}
	}
}

func TestSplitURLHandlesWhatTheEdgeActuallySends(t *testing.T) {
	for _, tc := range []struct{ name, raw, authority, path string }{
		{"a full url", "http://shop.example.com/x", "shop.example.com", "/x"},
		{"a port", "http://shop.example.com:8080/x", "shop.example.com:8080", "/x"},
		{"redacted userinfo", "http://user:REDACTED@shop.example.com/x", "shop.example.com", "/x"},
		{"no path at all", "http://shop.example.com", "shop.example.com", "/"},
		{"a bare target", "/x", "", "/x"},
		{"nothing", "", "", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority, path := splitURL(tc.raw)
			if authority != tc.authority || path != tc.path {
				t.Errorf("splitURL(%q) = %q %q, want %q %q", tc.raw, authority, path, tc.authority, tc.path)
			}
		})
	}
}

func TestTruncatePathKeepsValidUTF8(t *testing.T) {
	short := "/api/v1/users"
	if got := truncatePath(short); got != short {
		t.Errorf("truncatePath(%q) = %q", short, got)
	}

	// The cut lands mid-character on purpose: a shorter path is fine, half a
	// character is a column something will later fail to render.
	long := "/" + strings.Repeat("é", maxRawPathBytes)
	got := truncatePath(long)
	if len(got) > maxRawPathBytes {
		t.Errorf("truncatePath kept %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncatePath produced invalid UTF-8: %q", got)
	}
}

// The API asks this of a project's declared health path in order to leave the
// platform's own probes out of that project's traffic, and the answer has to be
// the template the follower charged the request to — the two are compared as
// strings in a WHERE clause, so "close enough" is a filter that matches nothing.
func TestRouteTemplateIsWhatTheFollowerWouldHaveStored(t *testing.T) {
	budgets := newRouteBudgets()
	for _, path := range []string{"/api/health", "/health/12345", "/", "/healthz/"} {
		if want, got := budgets.route("shop", "shop-production", path), RouteTemplate(path); got != want {
			t.Errorf("RouteTemplate(%q) = %q, but the follower stored %q", path, got, want)
		}
	}
}

// A path declared with stray whitespace, or not declared at all, must not
// become a template that matches the root route: every request would then be a
// health check.
func TestRouteTemplateTrimsWhatItIsGiven(t *testing.T) {
	if got := RouteTemplate("  /api/health  "); got != "/api/health" {
		t.Errorf("RouteTemplate did not trim: %q", got)
	}
	if got := RouteTemplate(""); got != "/" {
		t.Errorf("RouteTemplate(\"\") = %q, want the root", got)
	}
}
