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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
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
func connectionSecretData(provider string, config map[string]any, credential *connectionCredential) (map[string][]byte, corev1.SecretType, error) {
	switch {
	case tokenProviders[provider]:
		if credential.Token == "" {
			return nil, "", fmt.Errorf("provider %s authenticates with a token: pass {\"credential\": {\"token\": \"...\"}}", provider)
		}
		return map[string][]byte{gitTokenKey: []byte(credential.Token)}, corev1.SecretTypeOpaque, nil
	case provider == registryProvider:
		if credential.Username == "" || credential.Password == "" {
			return nil, "", fmt.Errorf("provider %s authenticates with a username and password: pass {\"credential\": {\"username\": \"...\", \"password\": \"...\"}}", registryProvider)
		}
		server := registryServer(config)
		if server == "" {
			return nil, "", fmt.Errorf("provider %s needs the registry in config.url, e.g. {\"config\": {\"url\": \"harbor.example.com/kitchen\"}}", registryProvider)
		}
		auth := base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password))
		dockerConfig, err := json.Marshal(map[string]any{
			"auths": map[string]any{
				server: map[string]string{
					"username": credential.Username,
					"password": credential.Password,
					"auth":     auth,
				},
			},
		})
		if err != nil {
			return nil, "", err
		}
		return map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig}, corev1.SecretTypeDockerConfigJson, nil
	default:
		return nil, "", fmt.Errorf("unknown provider %q: one of github, gitlab, gitea, dockerRegistry, neon", provider)
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
	if err := s.writeCredentialsSecret(req, secretName, data, secretType); err != nil {
		s.writeError(w, err)
		return
	}

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
	if err := s.Client.Create(ctx, connection); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("connection created through the api",
		"connection", connection.Name, "provider", body.Provider, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newConnectionView(connection))
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
		if claims.Items[i].Spec.ConnectionRef.Name == connection.Name {
			users = append(users, "claim "+claims.Items[i].Name)
		}
	}
	if len(users) > 0 {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"connection %q is still in use by %s: move or delete those first",
			connection.Name, strings.Join(users, ", "))})
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
