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
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// signingAlgorithms are the algorithms a platform token may be signed with.
// The identity provider signs with EdDSA (Ed25519) by default; the rest are
// listed so that swapping the issuer's key type does not need an operator
// release. Anything outside the list — `none` above all — is refused before a
// signature is even looked at.
var signingAlgorithms = []string{
	oidc.EdDSA,
	oidc.RS256, oidc.RS384, oidc.RS512,
	oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512,
}

// Caller is the authenticated identity behind a request: whoever the identity
// provider says the bearer token belongs to. The operator keeps no session
// state, so this lives exactly as long as the request does.
type Caller struct {
	// Subject is the account's stable identifier at the issuer.
	Subject string
	// Email and Name are informational, present when the token carries them.
	Email string
	Name  string
	// ClientID is the OAuth client the token was issued to, when the token
	// says so (`azp`). Empty for tokens minted straight from a session.
	ClientID string
	// Scopes the token was granted. Kitchen does not enforce scopes yet —
	// teams and RBAC land with the organizations plugin — but they are
	// carried through so that logs and future policy have them.
	Scopes []string
}

type callerContextKey struct{}

// CallerFrom returns the identity the request was authenticated as.
func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerContextKey{}).(Caller)
	return caller, ok
}

// issuerConfig is the platform's identity provider as the API needs to see it:
// where to validate tokens against, and which audiences it will accept.
type issuerConfig struct {
	issuer    string
	audiences []string
}

// authenticator validates bearer tokens against the platform's identity
// provider. Validation is stateless — a signature check against the issuer's
// JWKS plus the registered claims — so no request ever waits on the identity
// provider once its keys are known.
type authenticator struct {
	// extraAudiences are accepted on top of the ones derived from the
	// Kitchen object, for installations that put the API behind another
	// name than the one the platform generates.
	extraAudiences []string

	mu       sync.Mutex
	issuer   string
	verifier *oidc.IDTokenVerifier
}

// errNoIssuer is what every request gets when the platform has no identity
// provider: with no issuer there is no such thing as a valid token, so the
// API stays shut rather than falling open.
var errNoIssuer = errors.New("this installation has no identity provider, so no token can be valid")

// issuerFor reads the identity provider's URL and the API's own audience off
// the Kitchen singleton. Both follow the platform's naming (auth.<baseDomain>,
// kitchen.<baseDomain>) unless the object overrides them.
func issuerFor(kitchen *kitchenv1alpha1.Kitchen) (issuerConfig, error) {
	if kitchen == nil || !kitchen.Spec.Auth.Enabled {
		return issuerConfig{}, errNoIssuer
	}
	host := kitchen.Spec.Auth.Host
	if host == "" {
		if kitchen.Spec.BaseDomain == "" {
			return issuerConfig{}, errNoIssuer
		}
		host = "auth." + kitchen.Spec.BaseDomain
	}
	// The host is a hostname, but an installation pointed at an identity
	// provider that speaks plain HTTP — a local one during development —
	// may spell the scheme out.
	issuer := strings.TrimSuffix(host, "/")
	if !strings.Contains(issuer, "://") {
		issuer = "https://" + issuer
	}

	// The API is a resource server of its own: a client that asks the
	// issuer for a token for this URL (`resource=`) gets one scoped to it,
	// which is the audience this API most wants to see. Tokens minted from
	// a session — what an API key is exchanged for — carry the issuer
	// instead, so both are accepted.
	audiences := []string{issuer}
	if external := externalURL(kitchen); external != "" {
		audiences = append(audiences, external)
	}
	return issuerConfig{issuer: issuer, audiences: audiences}, nil
}

// externalURL is the base URL the operator API answers on from outside the
// cluster, mirroring the chart's default of https://kitchen.<baseDomain>.
func externalURL(kitchen *kitchenv1alpha1.Kitchen) string {
	if kitchen == nil {
		return ""
	}
	if url := strings.TrimSuffix(kitchen.Spec.API.ExternalURL, "/"); url != "" {
		return url
	}
	if kitchen.Spec.BaseDomain == "" {
		return ""
	}
	return "https://kitchen." + kitchen.Spec.BaseDomain
}

// verifierFor returns a verifier for the given issuer, discovering it on first
// use and re-discovering it if the platform is ever pointed at another one.
// go-oidc caches the JWKS behind the verifier and refetches it when a token
// arrives with an unknown key id, which is what makes key rotation a non-event.
func (a *authenticator) verifierFor(ctx context.Context, issuer string) (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil && a.issuer == issuer {
		return a.verifier, nil
	}

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the identity provider at %s: %w", issuer, err)
	}
	// The audience check is done here rather than by go-oidc, which only
	// compares against a single client id: a token for the API's own URL
	// and a token for the issuer are both legitimate, and neither is a
	// client id.
	a.verifier = provider.Verifier(&oidc.Config{
		SkipClientIDCheck:    true,
		SupportedSigningAlgs: signingAlgorithms,
	})
	a.issuer = issuer
	return a.verifier, nil
}

// tokenClaims are the parts of a validated token the API reads.
type tokenClaims struct {
	Subject  string `json:"sub"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	ClientID string `json:"azp"`
	Scope    string `json:"scope"`
}

// authenticate validates the request's bearer token and returns who it belongs
// to. Every error it returns is a 401: the caller could not prove who they are,
// whether the token was missing, expired, signed by a stranger or meant for
// somebody else.
func (a *authenticator) authenticate(ctx context.Context, req *http.Request, cfg issuerConfig) (Caller, error) {
	raw, err := bearerToken(req)
	if err != nil {
		return Caller{}, err
	}

	verifier, err := a.verifierFor(ctx, cfg.issuer)
	if err != nil {
		return Caller{}, err
	}
	token, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Caller{}, fmt.Errorf("the token was not accepted: %w", err)
	}

	audiences := append(slices.Clone(cfg.audiences), a.extraAudiences...)
	if !slices.ContainsFunc(token.Audience, func(aud string) bool { return slices.Contains(audiences, aud) }) {
		return Caller{}, fmt.Errorf("the token is for %s, not for this API (expected one of %s)",
			strings.Join(token.Audience, ", "), strings.Join(audiences, ", "))
	}

	claims := tokenClaims{}
	if err := token.Claims(&claims); err != nil {
		return Caller{}, fmt.Errorf("the token's claims are unreadable: %w", err)
	}
	if claims.Subject == "" {
		return Caller{}, errors.New("the token names no subject")
	}
	return Caller{
		Subject:  claims.Subject,
		Email:    claims.Email,
		Name:     claims.Name,
		ClientID: claims.ClientID,
		Scopes:   strings.Fields(claims.Scope),
	}, nil
}

// bearerToken pulls the credential out of the Authorization header. An API key
// is deliberately not accepted here: keys are exchanged for a short-lived token
// at the issuer, which keeps the operator out of the business of looking
// credentials up.
func bearerToken(req *http.Request) (string, error) {
	header := req.Header.Get("Authorization")
	if header == "" {
		return "", errors.New("no bearer token: send one as `Authorization: Bearer <token>`")
	}
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		// Case-insensitive per RFC 7235, but only after the common path.
		if fields := strings.Fields(header); len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
			token, ok = fields[1], true
		}
	}
	if !ok || strings.TrimSpace(token) == "" {
		return "", errors.New("the Authorization header is not a bearer token")
	}
	return strings.TrimSpace(token), nil
}

// authenticated wraps a handler so that it only ever runs for a request that
// carried a valid token. The identity provider is resolved per request from the
// Kitchen object, so turning auth on or off — or moving the issuer — takes
// effect without restarting the operator.
func (s *Server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()

		kitchen := &kitchenv1alpha1.Kitchen{}
		if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
			s.log().Error(err, "cannot read the Kitchen object, refusing the request")
			unauthorized(w, errNoIssuer.Error())
			return
		}

		cfg, err := issuerFor(kitchen)
		if err != nil {
			unauthorized(w, err.Error())
			return
		}

		caller, err := s.auth.authenticate(ctx, req, cfg)
		if err != nil {
			unauthorized(w, err.Error())
			return
		}

		next.ServeHTTP(w, req.WithContext(context.WithValue(ctx, callerContextKey{}, caller)))
	})
}

// unauthorized answers a request that could not prove who it is. The
// WWW-Authenticate header is what tells a client it is looking at an OAuth
// resource server rather than at something that wants a password.
func unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="kitchen", error="invalid_token"`)
	writeJSON(w, http.StatusUnauthorized, errorBody{Error: reason})
}
