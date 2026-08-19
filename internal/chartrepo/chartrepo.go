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

// Package chartrepo answers one question: which versions has the platform's
// Helm chart been published at?
//
// The chart lives in an OCI registry, so its versions are that repository's
// tags. Only the read half of the registry API is implemented, and only far
// enough to list them — pulling the chart is helm's job, in the update job,
// where the credentials to write to a cluster already are.
//
// Nothing here is required for the platform to run. An installation with no
// route to the registry gets an error it can show next to a version field
// rather than a broken settings page.
package chartrepo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver/v4"
)

const (
	// DefaultTTL is how long a successful listing is reused. Releases are
	// occasional and the answer is only read by an open settings page, so
	// this is about not making the registry answer the same question for
	// every visitor rather than about freshness.
	DefaultTTL = time.Hour

	// MinRefreshInterval is the floor a forced refresh still respects, and is
	// exported because the dashboard tells the reader what it is rather than
	// leaving a control that sometimes does nothing. It is short enough to be
	// invisible to someone who published a release and came to look for it.
	MinRefreshInterval = 10 * time.Second

	// failureTTL is how long a failed listing is remembered. Short enough
	// that fixing the network shows up quickly, long enough that a cluster
	// with no egress at all is not retrying on every request.
	failureTTL = 5 * time.Minute

	// requestTimeout bounds the whole listing, token request included. It is
	// answered inside a dashboard request, so it fails fast.
	requestTimeout = 10 * time.Second

	// maxResponse bounds what a registry may hand back. A tag listing is a
	// short JSON document; this is only here so that a misbehaving or
	// impersonated endpoint cannot be read into memory without limit.
	maxResponse = 4 << 20
)

// readAll reads a bounded response body.
func readAll(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, maxResponse))
}

// Client lists a chart's published versions, with the answer cached.
// The zero value is not usable; see New.
type Client struct {
	registry   string
	repository string
	http       *http.Client
	ttl        time.Duration

	mu        sync.Mutex
	versions  []semver.Version
	err       error
	refreshed time.Time
}

// New builds a client for a chart reference in the form the chart's
// selfUpdate.chart value takes, e.g. "oci://ghcr.io/bermos/charts/kitchen".
// A reference that is not an OCI one is an error rather than a silent
// fallback: this package speaks the registry API and nothing else, and an
// installation pointed at a classic HTTP chart repository should be told so.
func New(ref string) (*Client, error) {
	rest, ok := strings.CutPrefix(ref, "oci://")
	if !ok {
		return nil, fmt.Errorf("%q is not an oci:// chart reference, so its versions cannot be listed", ref)
	}
	registry, repository, ok := strings.Cut(strings.Trim(rest, "/"), "/")
	if !ok || registry == "" || repository == "" {
		return nil, fmt.Errorf("%q names no repository inside a registry", ref)
	}
	return &Client{
		registry:   registry,
		repository: repository,
		http:       &http.Client{Timeout: requestTimeout},
		ttl:        DefaultTTL,
	}, nil
}

// Listing is an answer and when it was taken. The two travel together because
// the client is shared: reading the versions and the time through separate
// calls could report one refresh's answer under another's timestamp, and the
// timestamp is the whole of how a dashboard says an hour-old answer is old.
type Listing struct {
	Versions  []semver.Version
	CheckedAt time.Time
}

// Versions returns every published version that parses as SemVer, oldest
// first. Tags that are not versions — "latest", a moving channel name — are
// skipped rather than reported: they are not something to upgrade to.
func (c *Client) Versions(ctx context.Context) (Listing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ttl := c.ttl
	if c.err != nil {
		ttl = failureTTL
	}
	return c.listing(ctx, ttl)
}

// Refresh asks the registry again without waiting for the cached answer to
// expire. Someone who has just published a release is the case it exists for:
// DefaultTTL is an hour, and until this the only ways to see a version sooner
// were to wait it out or restart the operator the cache lives in.
//
// The floor is not the TTL in miniature. The failure a forced listing can
// cause is being rate-limited by the registry, and a rate-limited listing is
// an error cached for failureTTL — five minutes of "the published versions
// could not be listed" bought by a control someone held down.
func (c *Client) Refresh(ctx context.Context) (Listing, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.listing(ctx, MinRefreshInterval)
}

// listing answers from the cache while it is younger than ttl, and from the
// registry otherwise. The caller holds the lock, which is also what stops two
// arrivals at once from both reaching the registry.
func (c *Client) listing(ctx context.Context, ttl time.Duration) (Listing, error) {
	if !c.refreshed.IsZero() && time.Since(c.refreshed) < ttl {
		return Listing{Versions: c.versions, CheckedAt: c.refreshed}, c.err
	}

	versions, err := c.fetch(ctx)
	c.versions, c.err, c.refreshed = versions, err, time.Now()
	return Listing{Versions: versions, CheckedAt: c.refreshed}, err
}

// Latest is the newest stable version, and whether there is one. Pre-releases
// are published under the same repository and sort below their release, but
// they are not what a button offering "the latest version" should mean.
func Latest(versions []semver.Version) (semver.Version, bool) {
	for i := len(versions) - 1; i >= 0; i-- {
		if len(versions[i].Pre) == 0 {
			return versions[i], true
		}
	}
	return semver.Version{}, false
}

func (c *Client) fetch(ctx context.Context) ([]semver.Version, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	tagsURL := fmt.Sprintf("https://%s/v2/%s/tags/list", c.registry, c.repository)

	body, challenge, err := c.get(ctx, tagsURL, "")
	if err != nil {
		return nil, err
	}
	// Public repositories still expect a token; the registry says which one
	// to ask for and where, and for a public chart the request needs no
	// credentials of its own.
	if challenge != "" {
		token, err := c.token(ctx, challenge)
		if err != nil {
			return nil, err
		}
		if body, _, err = c.get(ctx, tagsURL, token); err != nil {
			return nil, err
		}
	}

	listing := struct {
		Tags []string `json:"tags"`
	}{}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("unreadable tag listing from %s: %w", c.registry, err)
	}

	versions := make([]semver.Version, 0, len(listing.Tags))
	for _, tag := range listing.Tags {
		version, err := semver.Parse(strings.TrimPrefix(tag, "v"))
		if err != nil {
			continue
		}
		versions = append(versions, version)
	}
	semver.Sort(versions)
	return versions, nil
}

// get performs one registry request, returning the body on success or, on a
// 401, the challenge to satisfy before trying again.
func (c *Client) get(ctx context.Context, target, token string) (body []byte, challenge string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("cannot reach %s: %w", c.registry, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized && token == "" {
		if authenticate := resp.Header.Get("Www-Authenticate"); authenticate != "" {
			return nil, authenticate, nil
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s answered %s for %s", c.registry, resp.Status, c.repository)
	}

	body, err = readAll(resp)
	return body, "", err
}

// token satisfies a Bearer challenge. Anonymous pull is the case that matters:
// a published chart is public, and the registry hands out a token for it
// without credentials.
func (c *Client) token(ctx context.Context, challenge string) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", fmt.Errorf("%s asked for %q authentication, which is not supported", c.registry, scheme)
	}

	fields := map[string]string{}
	for _, param := range splitParams(params) {
		key, value, ok := strings.Cut(param, "=")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.Trim(value, `"`)
	}
	realm := fields["realm"]
	if realm == "" {
		return "", fmt.Errorf("%s asked for a token but named no realm to get one from", c.registry)
	}

	query := url.Values{}
	for _, key := range []string{"service", "scope"} {
		if value := fields[key]; value != "" {
			query.Set(key, value)
		}
	}
	if query.Get("scope") == "" {
		query.Set("scope", "repository:"+c.repository+":pull")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+query.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach the token endpoint at %s: %w", realm, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the token endpoint at %s answered %s", realm, resp.Status)
	}

	body, err := readAll(resp)
	if err != nil {
		return "", err
	}
	issued := struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}{}
	if err := json.Unmarshal(body, &issued); err != nil {
		return "", fmt.Errorf("unreadable token response from %s: %w", realm, err)
	}
	if issued.Token != "" {
		return issued.Token, nil
	}
	if issued.AccessToken != "" {
		return issued.AccessToken, nil
	}
	return "", fmt.Errorf("the token endpoint at %s issued no token", realm)
}

// splitParams splits a challenge's comma-separated parameters, ignoring commas
// inside quoted values — registries put them in `scope`.
func splitParams(params string) []string {
	out := []string{}
	quoted, start := false, 0
	for i, r := range params {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			out = append(out, params[start:i])
			start = i + 1
		}
	}
	return append(out, params[start:])
}
