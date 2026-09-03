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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/idp"
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
	// EmailVerified is the token's `email_verified` claim. It is not
	// decoration: an access entry may name an address instead of a `sub`, and
	// such an entry is honoured only for a verified address — an unverified
	// one is something the token holder said about themselves. internal/access
	// applies that rule; this carries the fact it needs.
	EmailVerified bool
	// ClientID is the OAuth client the token was issued to, when the token
	// says so (`azp`). Empty for tokens minted straight from a session, and
	// otherwise one of PlatformClientIDs — a token naming any other client
	// never reaches a handler.
	ClientID string
	// Scopes the token was granted. Kitchen does not authorize on them and
	// is not going to: the token says who the caller is, and Kitchen decides
	// what they may do from the access recorded on its own objects
	// (docs/AUTH.md, "Who may do what"). They are carried through because
	// they say what the caller asked the issuer for, which is worth logging.
	Scopes []string
}

type (
	callerContextKey  struct{}
	kitchenContextKey struct{}
)

// CallerFrom returns the identity the request was authenticated as.
func CallerFrom(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerContextKey{}).(Caller)
	return caller, ok
}

// access is this caller as the package that decides what they may do sees
// them: the three claims a membership entry can name, and nothing else.
func (c Caller) access() access.Caller {
	return access.Caller{Subject: c.Subject, Email: c.Email, EmailVerified: c.EmailVerified}
}

// isMachine reports whether this token was exchanged from a CI key rather than
// held by somebody who signed in.
//
// The address is the marker because it is the only one there is: a key becomes
// a session at the issuer, so the token it produces is shaped like anybody
// else's. Machine accounts are created under a reserved domain
// (idp.MachineAccountDomain, `.local`, RFC 6762) precisely so that they are
// recognisable, and the address is the issuer's own record rather than
// anything the caller says about itself — a person cannot register there, and
// a key cannot register anywhere else.
//
// It is not a role and it never grants anything. Every role this caller holds
// is resolved from the subject alone, exactly as before; this answers the one
// different question a route can ask, which is whether the caller is the kind
// of account that may widen its own access.
//
// An address that is absent or is under any other domain reads as a person's.
// That is deliberate rather than lax: a federated issuer need not send `email`
// at all, and refusing project creation to every account on such an
// installation would be a much larger change than the one rule this is. It
// costs nothing here, because the address on a key's token is not the caller's
// to choose — the issuer writes it from its own user table, where a machine
// account can only exist under this domain and a person can only exist
// outside it.
func (c Caller) isMachine() bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(c.Email)), "@"+idp.MachineAccountDomain)
}

// kitchenFrom is the Kitchen singleton the request was authenticated against —
// where the platform's operator list lives. It is nil only for a request that
// never went through the token check, which resolves to no operator at all
// rather than to everyone (see access.PlatformRoleFor).
func kitchenFrom(ctx context.Context) *kitchenv1alpha1.Kitchen {
	kitchen, _ := ctx.Value(kitchenContextKey{}).(*kitchenv1alpha1.Kitchen)
	return kitchen
}

// meView is the caller as described to themselves: who the token says they
// are, and the hat they wear on this platform. It says nothing about anybody
// else, which is why any valid token may ask for it.
//
// The project roles are deliberately not here. They belong on the projects
// themselves — the overview renders a list, and a caller's role arrives with
// each project rather than as a second list to join against.
type meView struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Name    string `json:"name,omitempty"`
	// PlatformRole is "operator" or "member".
	PlatformRole string `json:"platformRole"`
}

func (s *Server) getMe(w http.ResponseWriter, req *http.Request) {
	caller, _ := CallerFrom(req.Context())
	writeJSON(w, http.StatusOK, meView{
		Subject:      caller.Subject,
		Email:        caller.Email,
		Name:         caller.Name,
		PlatformRole: platformRoleFrom(req.Context()).String(),
	})
}

// issuerConfig is the platform's identity provider as the API needs to see it:
// where to validate tokens against, and which audiences it will accept.
type issuerConfig struct {
	issuer    string
	audiences []string
}

// PlatformClientIDs is the one place that says which OAuth clients are the
// platform's own — the only clients this API will act on behalf of.
//
// The issuer registers clients for two quite different things. Some are the
// platform's: the dashboard, seeded with an id the chart chooses. The rest
// belong to *applications* — an `oidcClient` claim registers one for whatever
// a project developer is deploying, and its redirect list is theirs to name.
// Both are clients of the same issuer, and until a token said which one it was
// issued to, an application's client could ask for a token for this API
// (`resource=`) and be handed the consenting person's whole role here. An
// operator signing in to a colleague's app would have handed it operator
// access for an hour.
//
// So the set is derived from configuration the chart writes — the dashboard's
// client id, `--ui-client-id`, the same value the identity provider is seeded
// with — and never from anything a registration says about itself. A client id
// is issued by the provider, so a developer cannot choose one; and nothing a
// developer can reach writes this list.
//
// The CLI is deliberately not in it: it holds an API key and exchanges it at
// the issuer for a session-minted token, which names no client at all
// (docs/CLI.md). A token with no `azp` is the CI path and stays accepted.
func PlatformClientIDs(dashboardClientID string) []string {
	if id := strings.TrimSpace(dashboardClientID); id != "" {
		return []string{id}
	}
	return nil
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
	// platformClients are the OAuth clients whose tokens this API accepts —
	// PlatformClientIDs is what says which those are. A token naming any
	// other client in `azp` is refused however valid its signature is.
	platformClients []string

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
	// The host is a hostname, so the scheme comes from the platform's TLS
	// mode: in mode "none" the Gateway only listens on HTTP, and an issuer
	// advertised as https there is one nothing serves. An installation
	// pointed at an external identity provider may spell the scheme out
	// instead, and then it is taken as given.
	issuer := strings.TrimSuffix(host, "/")
	if !strings.Contains(issuer, "://") {
		issuer = kitchen.Spec.TLS.Mode.Scheme() + "://" + issuer
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
// cluster, mirroring the chart's default of kitchen.<baseDomain> under the
// scheme the platform's TLS mode serves.
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
	return kitchen.Spec.TLS.Mode.Scheme() + "://kitchen." + kitchen.Spec.BaseDomain
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
//
// EmailVerified is decoded raw and read by access.VerifiedClaim, because
// issuers disagree about its type: the specification says boolean and several
// send the string "true". token.Claims is json.Unmarshal, so a plain bool here
// does not merely lose the claim — the string makes the decode of the *whole*
// claim set fail, which authenticate reports as "the token's claims are
// unreadable" and which is therefore a 401 on every route for every caller,
// dashboard and CI key alike. The lenient reading lives in internal/access
// because that is the package the claim matters to (an entry naming an address
// is honoured only for a verified one), and because the forward-auth gate has
// to answer the same question about the same issuer.
type tokenClaims struct {
	Subject       string          `json:"sub"`
	Email         string          `json:"email"`
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	ClientID      string          `json:"azp"`
	Scope         string          `json:"scope"`
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
	// Which client the token was issued to, when it says. The audience check
	// above establishes that the token is *for* this API; this establishes
	// that it was minted for something entitled to call it. An application's
	// OAuth client is a client of the same issuer as the dashboard, and a
	// token it obtained by asking for `resource=<this API>` would otherwise
	// arrive here signed, unexpired, in audience, and carrying every role the
	// person who pressed "Allow" holds. The identity provider refuses to mint
	// such a token at all (auth/src/auth.ts); this is the resource server's
	// own half of the same rule, so that neither an older issuer nor a
	// federated one can make the API's answer depend on somebody else's
	// enforcement.
	//
	// No `azp` means the token was minted straight from a session, which is
	// what a CI key is exchanged for. That path is unchanged.
	if claims.ClientID != "" && !slices.Contains(a.platformClients, claims.ClientID) {
		return Caller{}, fmt.Errorf(
			"the token was issued to the OAuth client %q, which is not one of the platform's own: "+
				"a client registered for an application cannot call this API as the person who signed in to it",
			claims.ClientID)
	}
	if claims.Subject == "" {
		return Caller{}, errors.New("the token names no subject")
	}
	return Caller{
		Subject:       claims.Subject,
		Email:         claims.Email,
		EmailVerified: access.VerifiedClaim(claims.EmailVerified),
		Name:          claims.Name,
		ClientID:      claims.ClientID,
		Scopes:        strings.Fields(claims.Scope),
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

		// The Kitchen object travels with the request: it is where platform
		// membership is written down, and reading it a second time inside the
		// guard would be a second read of the same cached object on every
		// request the API serves.
		ctx = context.WithValue(ctx, callerContextKey{}, caller)
		ctx = context.WithValue(ctx, kitchenContextKey{}, kitchen)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// unauthorized answers a request that could not prove who it is. The
// WWW-Authenticate header is what tells a client it is looking at an OAuth
// resource server rather than at something that wants a password.
func unauthorized(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="kitchen", error="invalid_token"`)
	writeJSON(w, http.StatusUnauthorized, errorBody{Error: reason})
}
