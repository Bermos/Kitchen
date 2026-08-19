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

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/version"
)

// The client is a client, and that is the whole design. Every command below is
// a request against docs/API.md and a way of rendering the answer: there is no
// second model of the platform in here, no cache of what a project is, and
// nothing the CLI knows that the API did not just say. That is what keeps the
// CLI from being a surface of its own that has to be kept in step — a route
// added to internal/api/policy.go is reachable through `kitchen api` the day
// it lands, and gets a command of its own when it earns one.

const (
	// apiPrefix is where the operator serves this API, from docs/API.md.
	apiPrefix = "/api/v1"
	// configPath is the dashboard's public configuration, served by the same
	// process outside /api/. It is how the CLI finds the issuer without being
	// told: one unauthenticated GET, the same values every login redirect
	// already shows.
	configPath = "/config.json"
	// eventStream is the media type the log endpoints follow on.
	eventStream = "text/event-stream"
)

// platformConfig is what /config.json answers — internal/ui.Config, read from
// the outside.
type platformConfig struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientId"`
	APIURL   string `json:"apiURL"`
	Version  string `json:"version"`
}

// authorizer supplies the bearer token for a request. It is an interface so a
// test can hand the client a fixed token, and so the token-getting logic —
// which reads files, exchanges keys and writes a cache back — stays out of the
// transport.
type authorizer interface {
	bearer(ctx context.Context) (string, error)
}

// staticToken is an authorizer that already has one.
type staticToken string

func (t staticToken) bearer(context.Context) (string, error) { return string(t), nil }

// client talks to one installation's API.
type client struct {
	base string
	http *http.Client
	auth authorizer
}

func newClient(base string, auth authorizer) *client {
	return &client{
		base: strings.TrimRight(base, "/"),
		// No client-level timeout: a followed build holds its connection open
		// for as long as the build runs, and a timeout here would cut it. Every
		// request carries a context instead, which is what --timeout sets.
		http: &http.Client{},
		auth: auth,
	}
}

// request builds an authenticated request against a path under /api/v1.
func (c *client) request(ctx context.Context, method, path string, query url.Values, body any) (*http.Request, error) {
	endpoint := c.base + apiPrefix + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fail(codeFailed, "encoding the request body: "+err.Error())
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, fail(codeUsage, err.Error())
	}
	token, err := c.auth.bearer(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "kitchen-cli/"+version.Version)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	return req, nil
}

// do runs a request and decodes the answer into `into`, which may be nil for a
// response with nothing to read.
//
// `doing` names the operation in the words the failure will use, so that every
// error out of this client reads as a sentence — "starting a build: you have
// viewer on shop; starting a build needs developer" — without a per-call
// string at the point of failure.
func (c *client) do(ctx context.Context, doing, method, path string, query url.Values, body, into any) error {
	req, err := c.request(ctx, method, path, query, body)
	if err != nil {
		return annotate(err, doing)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return unreachable(err, c.base).doing(doing)
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return unreachable(err, c.base).doing(doing)
	}
	if res.StatusCode >= 400 {
		return fromStatus(res.StatusCode, answer).doing(doing)
	}
	if into == nil || len(bytes.TrimSpace(answer)) == 0 {
		return nil
	}
	if err := json.Unmarshal(answer, into); err != nil {
		return failf(codeFailed, "the platform answered %s with something that is not the expected JSON: %v",
			res.Status, err).doing(doing)
	}
	return nil
}

// raw is `kitchen api`: one request, and the bytes that came back.
//
// It exists so that the CLI is never the reason something the API can do
// cannot be done from a terminal. A route added to the API is reachable
// through this the day it lands, which is what buys the time to decide whether
// it deserves a command of its own.
func (c *client) raw(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
) (int, []byte, error) {
	var payload io.Reader
	if len(body) > 0 {
		payload = bytes.NewReader(body)
	}
	endpoint := c.base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return 0, nil, fail(codeUsage, err.Error())
	}
	token, err := c.auth.bearer(ctx)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("authorization", "Bearer "+token)
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "kitchen-cli/"+version.Version)
	if len(body) > 0 {
		req.Header.Set("content-type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return 0, nil, unreachable(err, c.base)
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return res.StatusCode, nil, unreachable(err, c.base)
	}
	return res.StatusCode, answer, nil
}

// stream follows one of the API's Server-Sent Events endpoints, calling `onRow`
// for every `data:` event until the context ends or the platform closes the
// stream.
//
// The API's own stream sends the query's current page first and then every row
// that arrives after it, so a caller gets a tail and its backlog from one
// request. An `event: error` ends the stream with that error, which is how the
// API reports a failure it can no longer turn into a status code.
func (c *client) stream(ctx context.Context, doing, path string, query url.Values, onRow func([]byte) error) error {
	req, err := c.request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return annotate(err, doing)
	}
	req.Header.Set("accept", eventStream)

	res, err := c.http.Do(req)
	if err != nil {
		return unreachable(err, c.base).doing(doing)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode >= 400 {
		answer, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return fromStatus(res.StatusCode, answer).doing(doing)
	}
	// A platform that answered a plain page rather than a stream is not a
	// failure to report — it is an older operator, and the caller can still
	// read what came back. Saying so is better than a tail that silently
	// stops after one page.
	if !strings.Contains(res.Header.Get("content-type"), eventStream) {
		return failf(codeFailed, "the platform answered %q rather than a stream",
			res.Header.Get("content-type")).doing(doing).
			withHint("follow needs an operator that streams these logs; drop --follow to read a bounded page")
	}

	return readEvents(res.Body, onRow)
}

// readEvents decodes the Server-Sent Events wire format: `field: value` lines,
// one event per blank line, comments (`: keepalive`) ignored.
//
// Only the two fields the API sends are honoured — `data` and `event` — and an
// `event: error` is turned into the failure the API meant it as. Multi-line
// data is joined with newlines as the format requires, though nothing the API
// sends today has more than one line.
func readEvents(body io.Reader, onRow func([]byte) error) error {
	scanner := bufio.NewScanner(body)
	// A build log line can be long; the default 64KiB token limit would end a
	// tail with an error in the middle of somebody's stack trace.
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)

	var data []string
	var kind string

	// deliver hands one finished event on, and resets for the next.
	deliver := func() error {
		payload, eventKind := strings.Join(data, "\n"), kind
		data, kind = nil, ""
		if payload == "" {
			return nil
		}
		if eventKind == "error" {
			// The stream had already started when the platform hit this, so
			// it could not be a status code any more. It is one here.
			return fromStatus(http.StatusInternalServerError, []byte(payload))
		}
		return onRow([]byte(payload))
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if err := deliver(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// A comment. The API sends one every fifteen seconds so that
			// nothing in the middle reaps an idle connection.
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fail(codeUnreachable, "the stream ended: "+err.Error())
	}
	return deliver()
}

// discover reads /config.json, which needs no credential — it is the same
// public configuration the dashboard bootstraps itself from.
func (c *client) discover(ctx context.Context) (*platformConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+configPath, nil)
	if err != nil {
		return nil, fail(codeUsage, err.Error())
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "kitchen-cli/"+version.Version)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, unreachable(err, c.base).doing("asking the platform where to sign in")
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, unreachable(err, c.base).doing("asking the platform where to sign in")
	}
	if res.StatusCode >= 400 {
		return nil, fromStatus(res.StatusCode, answer).
			doing("asking the platform where to sign in").
			withHint("check --api: it wants the platform's own URL, the one the dashboard is served on")
	}

	config := &platformConfig{}
	if err := json.Unmarshal(answer, config); err != nil {
		return nil, failf(codeFailed, "%s%s did not answer the platform's configuration: %v",
			c.base, configPath, err).
			withHint("check --api: it wants the platform's own URL, the one the dashboard is served on")
	}
	return config, nil
}

// exchange turns an API key into the short-lived token the API accepts.
//
// The key belongs to the identity provider and is exchanged there, exactly as
// docs/API.md documents for CI: the operator never sees it, so a leaked key is
// revoked in one place and the CLI holds nothing the platform has to be told
// about.
func exchange(ctx context.Context, httpClient *http.Client, issuer, key string) (string, error) {
	endpoint := strings.TrimRight(issuer, "/") + "/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fail(codeUsage, err.Error())
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "kitchen-cli/"+version.Version)

	res, err := httpClient.Do(req)
	if err != nil {
		return "", unreachable(err, issuer).doing("exchanging the API key for a token")
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", unreachable(err, issuer).doing("exchanging the API key for a token")
	}
	if res.StatusCode >= 400 {
		f := fromStatus(res.StatusCode, answer).doing("exchanging the API key for a token")
		if res.StatusCode == http.StatusUnauthorized {
			f.Code = codeUnauthenticated
			f.Hint = "the identity provider does not know this key: it may have been revoked, " +
				"or it belongs to another installation. Issue a new one from the project's Keys tab " +
				"(or POST /projects/{name}/keys) and run `kitchen login` again"
		}
		return "", f
	}

	exchanged := struct {
		Token string `json:"token"`
	}{}
	if err := json.Unmarshal(answer, &exchanged); err != nil || exchanged.Token == "" {
		return "", fail(codeUnauthenticated, "the identity provider answered no token for this key").
			withHint("check that " + issuer + " is this platform's issuer")
	}
	return exchanged.Token, nil
}

// tokenExpiry reads a JWT's `exp` without verifying anything.
//
// Nothing is being decided here: the API validates the signature, and this is
// only how the CLI knows when to exchange again rather than being refused
// once. A token it cannot read expires "now", which costs one extra exchange
// and never uses a token that has run out.
func tokenExpiry(token string) *time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64URL(parts[1])
	if err != nil {
		return nil
	}
	claims := struct {
		Exp int64 `json:"exp"`
	}{}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return nil
	}
	at := time.Unix(claims.Exp, 0).UTC()
	return &at
}

// unreachable is the failure for "the platform could not be talked to at all",
// which is a different thing from any answer it could have given: no code, no
// body, nothing to relay. It is its own exit status for that reason — a script
// that retries on a network blip must not retry on a 403.
func unreachable(err error, target string) *failure {
	if errors.Is(err, context.DeadlineExceeded) {
		return failf(codeTimedOut, "%s did not answer in time: %v", target, err)
	}
	if errors.Is(err, context.Canceled) {
		return fail(codeInterrupted, "interrupted")
	}
	return failf(codeUnreachable, "cannot reach %s: %v", target, err).
		withHint("check --api (or KITCHEN_API) and that this machine can reach the platform")
}

// annotate names the operation on a failure that came from somewhere with no
// opinion about it.
func annotate(err error, doing string) error {
	f := asFailure(err)
	if f.Doing == "" {
		f.Doing = doing
	}
	return f
}

// base64URL decodes a JWT segment, which is base64url without padding.
func base64URL(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(segment, "="))
}
