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

package registrytoken

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Accounts is where the token service learns who is asking. The cluster's
// is ClusterAccounts; the registry check in hack/ runs one of its own.
type Accounts interface {
	// Authenticate is the grant a username and password carry, nil when
	// they are not an account. An error is the service being unable to tell,
	// which is not the same answer.
	Authenticate(ctx context.Context, username, password string) (*Grant, error)
	// SigningKey is the key tokens are signed with: the one whose
	// certificate the registry was given.
	SigningKey(ctx context.Context) (*rsa.PrivateKey, error)
}

// DefaultTTL is how long a token is good for. A client asks for a new one
// whenever the registry answers 401, so this is a bound on how long a revoked
// account — a deleted project's — keeps working, not on how long a push may
// take: the registry checks the token when each request starts.
const DefaultTTL = 10 * time.Minute

// Handler serves the token endpoint at path.
type Handler struct {
	Accounts Accounts
	// TTL defaults to DefaultTTL.
	TTL time.Duration
	// Now is the clock; tests set it.
	Now func() time.Time
}

// tokenResponse carries the token under both of the names clients read: the
// Docker token spec's `token`, and OAuth 2's `access_token`, which is what
// containerd reads from the POST form of the exchange.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"` //nolint:tagliatelle // the token spec's spelling
	ExpiresIn   int64  `json:"expires_in"`   //nolint:tagliatelle // the token spec's spelling
	IssuedAt    string `json:"issued_at"`    //nolint:tagliatelle // the token spec's spelling
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var username, password, service string
	var scopes []string
	switch req.Method {
	case http.MethodGet:
		// The Docker token flow proper: basic auth, and the scopes as query
		// parameters. It is what docker, go-containerregistry, BuildKit and
		// the kubelet send.
		var ok bool
		username, password, ok = req.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		query := req.URL.Query()
		service, scopes = query.Get("service"), query["scope"]
	case http.MethodPost:
		// The OAuth 2 password grant, which containerd tries first when it
		// holds a username and password. Refresh tokens are never issued, so
		// a refresh grant can never be valid here.
		req.Body = http.MaxBytesReader(w, req.Body, 64<<10)
		if err := req.ParseForm(); err != nil {
			http.Error(w, "unparseable token request", http.StatusBadRequest)
			return
		}
		if req.PostForm.Get("grant_type") != "password" {
			http.Error(w, "only the password grant is supported", http.StatusBadRequest)
			return
		}
		username, password = req.PostForm.Get("username"), req.PostForm.Get("password")
		service, scopes = req.PostForm.Get("service"), req.PostForm["scope"]
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if username == "" || password == "" {
		unauthorized(w)
		return
	}

	ctx := req.Context()
	log := logf.FromContext(ctx).WithName("registry-token")
	grant, err := h.Accounts.Authenticate(ctx, username, password)
	if err != nil {
		log.Error(err, "could not check a registry credential", "username", username)
		http.Error(w, "the registry's accounts could not be read", http.StatusServiceUnavailable)
		return
	}
	if grant == nil {
		unauthorized(w)
		return
	}
	key, err := h.Accounts.SigningKey(ctx)
	if err != nil {
		log.Error(err, "could not read the registry token signing key")
		http.Error(w, "the registry's signing key could not be read", http.StatusServiceUnavailable)
		return
	}

	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	ttl := h.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	token, err := sign(key, service, *grant, accessFor(*grant, scopes), now, ttl)
	if err != nil {
		log.Error(err, "could not sign a registry token")
		http.Error(w, "the token could not be signed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		Token:       token,
		AccessToken: token,
		ExpiresIn:   int64(ttl / time.Second),
		IssuedAt:    now.UTC().Format(time.RFC3339),
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="kitchen"`)
	http.Error(w, "a registry username and password are required", http.StatusUnauthorized)
}

// Server is the token service as a manager runnable: its own listener, like
// the git webhook receiver, because it answers clients that hold no platform
// token at all — a kubelet, a build — and so belongs outside the API's
// authentication entirely.
type Server struct {
	Accounts Accounts
	// BindAddr for the HTTP server, e.g. ":8093".
	BindAddr string
	// Path the endpoint is served at.
	Path string
}

// Start implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle(s.Path, &Handler{Accounts: s.Accounts})
	server := &http.Server{
		Addr:              s.BindAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	logf.Log.WithName("registry-token").Info("starting registry token service", "addr", s.BindAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: every replica
// answers, since a pull on any node may ask any of them.
func (s *Server) NeedLeaderElection() bool { return false }
