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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/gitprovider"
	"github.com/Bermos/Kitchen/internal/provider"
)

// The connection write surface. The interesting part is the credential: the
// API deliberately never reads credentials back, so writing one means the
// operator creates the Secret from the request body and the response never
// echoes it. That keeps the invariant "credentials never leave the operator"
// while removing the last reason onboarding needed kubectl.

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

const (
	// connectionSecretPrefix names the credentials secrets this API manages.
	connectionSecretPrefix = "kitchen-connection-"

	// managedByLabelKey / managedByLabelValue mark the secrets the API wrote,
	// so deleting a connection only ever removes credentials the platform
	// itself stored — never a secret something else (an Infisical sync, a
	// hand-written manifest) put there.
	managedByLabelKey   = "app.kubernetes.io/managed-by"
	managedByLabelValue = "kitchen"

	// gitTokenKey is the key the reconcilers read a git or API token from.
	gitTokenKey = "token"
)

// connectionCredential is a credential as the API accepts one — a token, or a
// username and password, depending on the provider. It is write-only: nothing
// in this package ever serializes it back out.
type connectionCredential struct {
	Token    string `json:"token,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type createConnectionRequest struct {
	Name       string                `json:"name"`
	Provider   string                `json:"provider"`
	Config     map[string]any        `json:"config,omitempty"`
	Credential *connectionCredential `json:"credential"`
}

type patchConnectionRequest struct {
	Config     *map[string]any       `json:"config,omitempty"`
	Credential *connectionCredential `json:"credential,omitempty"`
}

// tokenProviders take a bare token; the registry is the one provider whose
// credential is a username and password baked into a dockerconfigjson.
var tokenProviders = map[string]bool{
	"github": true,
	"gitlab": true,
	"gitea":  true,
	"neon":   true,
}

const registryProvider = "dockerRegistry"

// connectionSecretData turns a credential into the secret the reconcilers
// expect for the provider: a `token` key, or a `.dockerconfigjson` for the
// registry host named in the config.
func connectionSecretData(providerName string, config map[string]any, credential *connectionCredential) (map[string][]byte, corev1.SecretType, error) {
	switch {
	case tokenProviders[providerName]:
		if credential.Token == "" {
			return nil, "", fmt.Errorf("provider %s authenticates with a token: pass {\"credential\": {\"token\": \"...\"}}", providerName)
		}
		return map[string][]byte{gitTokenKey: []byte(credential.Token)}, corev1.SecretTypeOpaque, nil
	case providerName == registryProvider:
		if credential.Username == "" || credential.Password == "" {
			return nil, "", fmt.Errorf("provider %s authenticates with a username and password: pass {\"credential\": {\"username\": \"...\", \"password\": \"...\"}}", registryProvider)
		}
		server := registryServer(config)
		if server == "" {
			return nil, "", fmt.Errorf("provider %s needs the registry in config.url, e.g. {\"config\": {\"url\": \"harbor.example.com/kitchen\"}}", registryProvider)
		}
		dockerConfig, err := provider.DockerConfigJSON(server, credential.Username, credential.Password)
		if err != nil {
			return nil, "", err
		}
		return map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig}, corev1.SecretTypeDockerConfigJson, nil
	default:
		return nil, "", fmt.Errorf("unknown provider %q: one of github, gitlab, gitea, dockerRegistry, neon", providerName)
	}
}

// registryServer is the host builds authenticate against, off the registry
// prefix images are pushed under: "harbor.example.com/kitchen" pushes to
// harbor.example.com.
func registryServer(config map[string]any) string {
	url, _ := config["url"].(string)
	url = strings.TrimSpace(url)
	url = strings.TrimPrefix(url, "https://")
	url = strings.TrimPrefix(url, "http://")
	server, _, _ := strings.Cut(url, "/")
	return server
}

// rawConfig marshals the request's provider config for the spec, nil when
// there is none.
func rawConfig(config map[string]any) (*runtime.RawExtension, error) {
	if len(config) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return &runtime.RawExtension{Raw: raw}, nil
}

// writeCredentialsSecret creates or overwrites the named credentials secret.
// An update deletes and recreates rather than patching, because the secret's
// type can change with the data and type is immutable on a live secret.
func (s *Server) writeCredentialsSecret(req *http.Request, name string, data map[string][]byte, secretType corev1.SecretType) error {
	ctx := req.Context()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.Namespace,
			Labels:    map[string]string{managedByLabelKey: managedByLabelValue},
		},
		Type: secretType,
		Data: data,
	}
	err := s.Client.Create(ctx, secret)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing := &corev1.Secret{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, existing); err != nil {
		return err
	}
	if existing.Type == secretType {
		patch := client.MergeFrom(existing.DeepCopy())
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[managedByLabelKey] = managedByLabelValue
		existing.Data = data
		return s.Client.Patch(ctx, existing, patch)
	}
	if err := s.Client.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return s.Client.Create(ctx, secret)
}

func (s *Server) createConnection(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := createConnectionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Provider = strings.TrimSpace(body.Provider)

	if body.Name == "" {
		badRequest(w, "name is required")
		return
	}
	if errs := validation.IsDNS1123Label(body.Name); len(errs) > 0 {
		badRequest(w, "name must work as a DNS label — lowercase letters, digits and '-', starting and ending alphanumeric (got %q)", body.Name)
		return
	}
	if body.Credential == nil {
		badRequest(w, "credential is required: the operator stores it in a Secret and never reads it back to you")
		return
	}
	data, secretType, err := connectionSecretData(body.Provider, body.Config, body.Credential)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	config, err := rawConfig(body.Config)
	if err != nil {
		badRequest(w, "config is not serializable: %s", err.Error())
		return
	}

	// Existence is checked before the secret is written so a name collision
	// does not overwrite the credentials of the connection already there.
	existing := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, body.Name, existing); err == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf("connection %q already exists", body.Name)})
		return
	} else if !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}

	secretName := connectionSecretPrefix + body.Name
	caller, _ := CallerFrom(ctx)
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name:        body.Name,
			Namespace:   s.Namespace,
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             body.Provider,
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: secretName},
			Config:               config,
		},
	}
	// Recorded before the credential is written, not before the Connection:
	// the secret is the part of this request that matters, and a credential
	// on the cluster that no record mentions is the failure to avoid.
	if !s.recorded(w, req, audit.Transition{
		Object:     connection,
		Kind:       audit.KindConnection,
		Operation:  clickhouse.AuditCreate,
		Privileged: audit.PrivilegeCredential,
		To:         body.Provider,
		Reason:     fmt.Sprintf("connection %s created for provider %s", body.Name, body.Provider),
		Details:    map[string]any{"provider": body.Provider, "secret": secretName},
	}) {
		return
	}
	if err := s.writeCredentialsSecret(req, secretName, data, secretType); err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.Client.Create(ctx, connection); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("connection created through the api",
		"connection", connection.Name, "provider", body.Provider, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newConnectionView(connection))
}

// connectionProbeTimeout bounds one credential test. The probe is a single
// request to the provider, and a caller is watching a spinner: a provider that
// has not answered by then is an answer of its own.
const connectionProbeTimeout = 15 * time.Second

// testConnectionRequest asks the platform to try a credential before it is
// stored — the dashboard's "Test connection" — or to re-check the one a
// connection already has. Naming a connection supplies whatever the request
// leaves out: its provider, its config, and, without a credential, the stored
// credential itself.
type testConnectionRequest struct {
	Name       string                `json:"name,omitempty"`
	Provider   string                `json:"provider,omitempty"`
	Config     map[string]any        `json:"config,omitempty"`
	Credential *connectionCredential `json:"credential,omitempty"`
}

// connectionTestView is the probe's verdict, in the same three parts the
// Connected and CredentialsValid conditions are written from: a provider that
// is down and a credential that is wrong have to read differently. Message is
// the provider's own words and never contains the credential.
type connectionTestView struct {
	Reachable         bool   `json:"reachable"`
	CredentialChecked bool   `json:"credentialChecked"`
	CredentialValid   bool   `json:"credentialValid"`
	Message           string `json:"message"`
	// Warnings are what an accepted credential still cannot do — a GitHub
	// token that registers webhooks but could not post a commit status. The
	// connection works; something the platform wants would not.
	Warnings []string `json:"warnings,omitempty"`
}

// testConnection runs the credential probe the ConnectionReconciler runs, and
// stores nothing: no Secret is written and no Connection is created, so a
// credential that turns out to be wrong leaves no trace to clean up. It is the
// same probe on purpose — a green test and a green Connection have to mean the
// same thing.
//
// It reaches the address in config.apiUrl, which the caller may supply; that
// is no more than creating a connection already does, and every caller here
// has been through the identity provider.
func (s *Server) testConnection(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := testConnectionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Provider = strings.TrimSpace(body.Provider)

	// The connection the test is about, when there is one. A name nothing
	// answers to is only an error when the request does not carry the whole
	// probe itself — testing a credential before creating the connection is
	// the point of the endpoint.
	var stored *kitchenv1alpha1.Connection
	if body.Name != "" {
		existing := &kitchenv1alpha1.Connection{}
		err := s.get(ctx, body.Name, existing)
		switch {
		case err == nil:
			stored = existing
		case apierrors.IsNotFound(err) && body.Credential != nil && body.Provider != "":
		default:
			s.writeError(w, err)
			return
		}
	}

	providerName := body.Provider
	if providerName == "" && stored != nil {
		providerName = stored.Spec.Provider
	}
	if providerName == "" {
		badRequest(w, "provider is required, unless name refers to a connection that already exists")
		return
	}

	config := body.Config
	if config == nil && stored != nil && stored.Spec.Config != nil {
		config = map[string]any{}
		if err := json.Unmarshal(stored.Spec.Config.Raw, &config); err != nil {
			s.writeError(w, err)
			return
		}
	}

	// The credential is either the one being tried or the one already stored;
	// the candidate is built into a Secret the probe never sees the inside of,
	// which keeps this endpoint on the same code path as the reconciler.
	creds := &corev1.Secret{}
	switch {
	case body.Credential != nil:
		data, secretType, err := connectionSecretData(providerName, config, body.Credential)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		creds.Name = connectionSecretPrefix + "candidate"
		creds.Namespace = s.Namespace
		creds.Type = secretType
		creds.Data = data
	case stored != nil:
		key := types.NamespacedName{Namespace: s.Namespace, Name: stored.Spec.CredentialsSecretRef.Name}
		if err := s.Client.Get(ctx, key, creds); err != nil {
			s.writeError(w, err)
			return
		}
	default:
		badRequest(w, "credential is required, unless name refers to a connection whose stored credential should be re-checked")
		return
	}

	rawCfg, err := rawConfig(config)
	if err != nil {
		badRequest(w, "config is not serializable: %s", err.Error())
		return
	}
	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: body.Name, Namespace: s.Namespace},
		Spec:       kitchenv1alpha1.ConnectionSpec{Provider: providerName, Config: rawCfg},
	}
	if conn.Name == "" {
		conn.Name = "candidate"
	}

	factory := s.Probes
	if factory == nil {
		factory = provider.Default
	}
	credProbe, err := factory(conn, creds)
	if errors.Is(err, provider.ErrNotImplemented) {
		// The CRD can admit a provider before this operator version knows it,
		// exactly as the reconciler reports it: unchecked, not red.
		writeJSON(w, http.StatusOK, connectionTestView{Message: fmt.Sprintf(
			"the platform has no %s implementation yet, so the credential cannot be checked", providerName)})
		return
	}
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	probeCtx, cancel := context.WithTimeout(ctx, connectionProbeTimeout)
	defer cancel()
	result := credProbe.Probe(probeCtx)

	caller, _ := CallerFrom(ctx)
	s.log().Info("connection tested through the api", "connection", body.Name, "provider", providerName,
		"reachable", result.Reachable, "credentialValid", result.CredentialValid, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, connectionTestView{
		Reachable:         result.Reachable,
		CredentialChecked: result.CredentialChecked,
		CredentialValid:   result.CredentialValid,
		Message:           result.Message,
		Warnings:          result.Warnings,
	})
}

func (s *Server) patchConnection(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	connection := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, req.PathValue("name"), connection); err != nil {
		s.writeError(w, err)
		return
	}

	body := patchConnectionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Config == nil && body.Credential == nil {
		badRequest(w, "nothing to change: pass config, credential, or both")
		return
	}

	// The credential is validated against the config the connection will have
	// after this patch — a registry rotation needs the registry host.
	effectiveConfig := map[string]any{}
	if connection.Spec.Config != nil {
		_ = json.Unmarshal(connection.Spec.Config.Raw, &effectiveConfig)
	}
	if body.Config != nil {
		effectiveConfig = *body.Config
	}

	if body.Credential != nil {
		data, secretType, err := connectionSecretData(connection.Spec.Provider, effectiveConfig, body.Credential)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		// A rotated credential is the security-relevant half of this
		// endpoint, so it is recorded as its own transition rather than
		// folded into "the connection changed".
		if !s.recorded(w, req, audit.Transition{
			Object:     connection,
			Kind:       audit.KindConnection,
			Operation:  clickhouse.AuditUpdate,
			Privileged: audit.PrivilegeCredential,
			To:         connection.Spec.Provider,
			Reason:     fmt.Sprintf("the credential for connection %s was replaced", connection.Name),
			Details:    map[string]any{"provider": connection.Spec.Provider, "rotated": true},
		}) {
			return
		}
		if err := s.writeCredentialsSecret(req, connection.Spec.CredentialsSecretRef.Name, data, secretType); err != nil {
			s.writeError(w, err)
			return
		}
	}

	if body.Config != nil {
		config, err := rawConfig(*body.Config)
		if err != nil {
			badRequest(w, "config is not serializable: %s", err.Error())
			return
		}
		if !s.recorded(w, req, audit.Transition{
			Object:    connection,
			Kind:      audit.KindConnection,
			Operation: clickhouse.AuditUpdate,
			To:        connection.Spec.Provider,
			Reason:    fmt.Sprintf("the configuration of connection %s was changed", connection.Name),
			Details:   map[string]any{"provider": connection.Spec.Provider, "rotated": false},
		}) {
			return
		}
		patch := client.MergeFrom(connection.DeepCopy())
		connection.Spec.Config = config
		if err := s.Client.Patch(ctx, connection, patch); err != nil {
			s.writeError(w, err)
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("connection changed through the api",
		"connection", connection.Name, "rotated", body.Credential != nil, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newConnectionView(connection))
}

func (s *Server) deleteConnection(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	connection := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, req.PathValue("name"), connection); err != nil {
		s.writeError(w, err)
		return
	}

	// A connection something references must not disappear under it: the
	// error names what is still using it, which is also the undo list.
	var users []string
	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, projects, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	for i := range projects.Items {
		project := &projects.Items[i]
		if project.Spec.Source.ConnectionRef.Name == connection.Name ||
			project.Spec.Registry.ConnectionRef.Name == connection.Name {
			users = append(users, "project "+project.Name)
		}
	}
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, claims, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	for i := range claims.Items {
		if claims.Items[i].Connection() == connection.Name {
			users = append(users, "claim "+claims.Items[i].Name)
		}
	}
	if len(users) > 0 {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"connection %q is still in use by %s: move or delete those first",
			connection.Name, strings.Join(users, ", "))})
		return
	}

	if !s.recorded(w, req, audit.Transition{
		Object:    connection,
		Kind:      audit.KindConnection,
		Operation: clickhouse.AuditDelete,
		From:      connection.Spec.Provider,
		Reason:    fmt.Sprintf("connection %s and the credential the platform wrote for it were deleted", connection.Name),
		Details:   map[string]any{"provider": connection.Spec.Provider},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, connection); err != nil {
		s.writeError(w, err)
		return
	}

	// The credentials secret goes with the connection only when the platform
	// wrote it; anything else that put a secret there keeps it.
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: connection.Spec.CredentialsSecretRef.Name}
	if err := s.Client.Get(ctx, key, secret); err == nil && secret.Labels[managedByLabelKey] == managedByLabelValue {
		if err := s.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			s.writeError(w, err)
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("connection deleted through the api",
		"connection", connection.Name, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// repositoryListTimeout bounds one repository listing. The walk is several
// sequential requests to the provider and somebody is watching a dropdown
// spin: a provider that has not finished by then is an answer of its own, and
// the field falls back to being typed into.
const repositoryListTimeout = 20 * time.Second

// repositoryView is one repository the picker offers. It carries what the
// create-a-project form fills in from it — the name, and the branch the
// project's production branch should default to — plus enough to tell two
// similarly-named repositories apart.
type repositoryView struct {
	FullName      string `json:"fullName"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	Private       bool   `json:"private,omitempty"`
	Description   string `json:"description,omitempty"`
}

// connectionRepositoriesView is what one connection's credential can see.
//
// Supported is the field that matters: a provider the platform cannot
// enumerate is not a failure, it is a form field that has to be typed into
// instead, and answering 200 with `supported: false` is the same shape
// testConnection uses to say a provider has no implementation yet. Truncated
// says the listing was cut short, because a repository missing from a picker
// is otherwise indistinguishable from one that does not exist.
type connectionRepositoriesView struct {
	Provider  string           `json:"provider"`
	Supported bool             `json:"supported"`
	Items     []repositoryView `json:"items"`
	Truncated bool             `json:"truncated,omitempty"`
	// Message is why there is no listing, in words a form can show. Empty
	// when there is one.
	Message string `json:"message,omitempty"`
}

// unsupportedRepositories is the "type the name instead" answer.
func unsupportedRepositories(w http.ResponseWriter, providerName, message string) {
	writeJSON(w, http.StatusOK, connectionRepositoriesView{
		Provider: providerName,
		Items:    []repositoryView{},
		Message:  message,
	})
}

// listConnectionRepositories answers what a connection's stored credential
// can see, so that naming a repository is a choice from a list rather than a
// string somebody has to spell correctly.
//
// It is the one route under /connections/ that is not the operator's alone,
// for the same reason the list next to it is not: creating a project is
// self-service, and its second field is the repository. It reads no
// credential back — the token is used to ask the provider a question and
// never leaves the operator — and it writes nothing at all.
func (s *Server) listConnectionRepositories(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	connection := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, req.PathValue("name"), connection); err != nil {
		s.writeError(w, err)
		return
	}
	providerName := connection.Spec.Provider

	git, unsupported, err := s.gitProviderFor(ctx, connection)
	if unsupported != "" {
		unsupportedRepositories(w, providerName, unsupported+": type the repository as owner/name")
		return
	}
	if err != nil {
		s.writeProviderError(w, connection, err)
		return
	}
	lister, ok := gitprovider.Repositories(git)
	if !ok {
		unsupportedRepositories(w, providerName, fmt.Sprintf(
			"the platform's %s support cannot enumerate repositories: type the repository as owner/name", providerName))
		return
	}

	listCtx, cancel := context.WithTimeout(ctx, repositoryListTimeout)
	defer cancel()
	listing, err := lister.ListRepositories(listCtx)
	if err != nil {
		// The provider's own words, which is where a rejected or
		// under-scoped token says so. A dropdown that cannot be filled is not
		// the platform failing, so it reads as a bad gateway rather than a
		// 500, and the form still takes a typed name.
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"connection %q could not list repositories: %s", connection.Name, err.Error())})
		return
	}

	items := make([]repositoryView, 0, len(listing.Repositories))
	for _, repo := range listing.Repositories {
		items = append(items, repositoryView{
			FullName:      repo.FullName,
			DefaultBranch: repo.DefaultBranch,
			Private:       repo.Private,
			Description:   repo.Description,
		})
	}
	writeJSON(w, http.StatusOK, connectionRepositoriesView{
		Provider:  providerName,
		Supported: true,
		Items:     items,
		Truncated: listing.Truncated,
	})
}
