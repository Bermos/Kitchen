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

package idp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ClientsPath is the operator's client-management endpoint on the identity
// provider the chart ships: the redirect list of a client it registered, and
// taking that client away again.
//
// Registering a client is standard — RFC 7591, and the whole of what
// docs/AUTH.md asks an issuer for. *Changing* one is RFC 7592, which is a
// separate specification an issuer may implement or not, and the one the
// chart ships does not: its registration response names no client
// configuration endpoint. So this sits where the account directory sits, for
// the same reason — under a prefix that is Kitchen's, authenticated by the
// operator's service credential — and it is reached only when the issuer
// offered nothing standard. An installation federated to an issuer of its own
// keeps every client it registers; what it does not keep is the redirect list
// being maintained for it. See ErrNoClientManagement.
const ClientsPath = "/kitchen/clients"

// ErrNoClientManagement says the issuer offers no way to change or remove a
// client once it has been registered: no RFC 7592 client configuration
// endpoint in its registration answer, and no Kitchen prefix either.
//
// It is a federated issuer, and it is not a fault to be fixed. Everything
// else works — the client is registered, the application signs people in with
// it — and what is lost is the automation on top:
//
//   - **A redirect list stops following the environments.** A preview that
//     appears after the client was registered has a callback URL the issuer
//     will refuse, so signing in from that preview fails until somebody adds
//     it at the issuer by hand. The claim says so on its condition rather
//     than looking bound and behaving otherwise.
//   - **A deleted claim leaves its client behind.** The operator says which
//     client id it could not remove, because a client nobody deregistered is
//     a credential that outlives the thing it was for.
//
// Callers report it rather than retry it: an endpoint that is not there does
// not appear by being asked again.
var ErrNoClientManagement = errors.New("the issuer offers no way to manage a registered client")

// ClientHandle is a registered client and how to get back to it.
//
// The two registration fields are RFC 7592's: an issuer that supports client
// management hands out a URL for the client and a token that authorizes
// changes to it, once, in the registration answer. They are stored with the
// client's credentials because they cannot be asked for again.
type ClientHandle struct {
	// ID is the client_id.
	ID string

	// RegistrationURI is the client configuration endpoint the issuer named,
	// empty when it named none.
	RegistrationURI string

	// RegistrationToken authorizes calls to RegistrationURI.
	RegistrationToken string
}

// Manageable reports whether the issuer named a client configuration
// endpoint for this client.
func (h ClientHandle) Manageable() bool {
	return h.RegistrationURI != "" && h.RegistrationToken != ""
}

// clientRequest is one client's metadata as both RFC 7591 registration and
// RFC 7592 update carry it.
func clientRequest(want ClientRegistration) map[string]any {
	payload := map[string]any{
		"client_name":                want.Name,
		"redirect_uris":              want.RedirectURIs,
		"grant_types":                want.GrantTypes,
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "client_secret_basic",
	}
	if len(want.Scopes) > 0 {
		payload["scope"] = strings.Join(want.Scopes, " ")
	}
	return payload
}

// UpdateClient replaces a registered client's metadata — in practice its
// redirect list, which is the one part of it the platform keeps moving.
//
// It prefers the standard route: RFC 7592 replaces the whole registration, so
// the caller passes what it would have registered rather than a patch. When
// the issuer named no client configuration endpoint it falls back to the
// Kitchen prefix, and when that is not there either the answer is
// ErrNoClientManagement — the caller's cue to report rather than retry.
func (c *Client) UpdateClient(ctx context.Context, handle ClientHandle, want ClientRegistration) error {
	if handle.ID == "" {
		return errors.New("updating an OAuth client: no client id")
	}
	what := fmt.Sprintf("updating the OAuth client %q", handle.ID)

	if handle.Manageable() {
		payload := clientRequest(want)
		payload["client_id"] = handle.ID
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		return c.configureClient(ctx, http.MethodPut, handle, body, what)
	}

	body, err := json.Marshal(map[string]any{
		"clientId":     handle.ID,
		"clientName":   want.Name,
		"redirectURIs": want.RedirectURIs,
		"grantTypes":   want.GrantTypes,
		"scopes":       want.Scopes,
	})
	if err != nil {
		return err
	}
	_, err = c.callDirectory(ctx, http.MethodPut, c.cfg.BaseURL+ClientsPath, body, what)
	if errors.Is(err, errDirectoryNotFound) {
		return fmt.Errorf("%s: %w", what, ErrNoClientManagement)
	}
	return err
}

// DeleteClient deregisters a client, so that credentials which have outlived
// what they were issued for cannot be used to sign anybody in.
//
// A client that is already gone is a success: the caller is a finalizer, and
// deletion asking for something twice has to be as good as asking once.
func (c *Client) DeleteClient(ctx context.Context, handle ClientHandle) error {
	if handle.ID == "" {
		return nil
	}
	what := fmt.Sprintf("deregistering the OAuth client %q", handle.ID)

	if handle.Manageable() {
		err := c.configureClient(ctx, http.MethodDelete, handle, nil, what)
		if errors.Is(err, errClientGone) {
			return nil
		}
		return err
	}

	endpoint := c.cfg.BaseURL + ClientsPath + "?" + url.Values{"clientId": []string{handle.ID}}.Encode()
	_, err := c.callDirectory(ctx, http.MethodDelete, endpoint, nil, what)
	if errors.Is(err, errDirectoryNotFound) {
		// The prefix answers 404 both for a client that is not there and for
		// an issuer that has never heard of it, and the two are the same
		// answer to a caller that is trying to remove the thing: it is not
		// there, and this is not the place to find out why.
		return nil
	}
	return err
}

// configureClient performs one RFC 7592 call against the client's own
// configuration endpoint, which is authorized by the registration access
// token rather than by the operator's service credential — the token is the
// issuer's statement about this one client.
func (c *Client) configureClient(
	ctx context.Context,
	method string,
	handle ClientHandle,
	body []byte,
	what string,
) error {
	req, err := c.request(ctx, method, handle.RegistrationURI, bytesReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	req.Header.Set("authorization", "Bearer "+handle.RegistrationToken)

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = res.Body.Close() }()

	answer, err := readLimited(res.Body)
	if err != nil {
		return err
	}
	switch {
	case res.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s: %w", what, errClientGone)
	case res.StatusCode < 200 || res.StatusCode > 299:
		return fmt.Errorf("%s: %s: %s", what, res.Status, summarize(answer))
	}
	// The answer is the client as the issuer now holds it, and nothing here
	// reads it: the operator's record is what it registered, and a client the
	// issuer described differently would be a client to register again rather
	// than one to believe.
	return nil
}

// errClientGone is the unwrapped 404 from a client configuration endpoint:
// the client this handle names is not registered any more.
var errClientGone = errors.New("the client is not registered")
