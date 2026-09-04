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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/oidcclient"
)

// The oidcClient half of the ResourceClaim reconciler: an application asks
// for single sign-on with one claim, and gets an OAuth client at the
// platform's own identity provider — the same issuer the dashboard signs
// people in at, and the same accounts.
//
// It is the second claim type, and it is deliberately the same shape as the
// first (resourceclaim_postgres.go): a claim binds to a Secret in the
// application namespace, and
// `fromResourceClaim` puts that Secret's keys into the application's
// environment. Nothing about consuming a claim had to learn a new concept.
//
// What is not the same is where the resource comes from. A database claim
// names a Connection, and the Connection's plugin provisions it; there is no
// Connection here, because the platform is already configured with an
// identity provider — the Kitchen object's `spec.auth` — and it is the
// operator's own service credential that registers clients there. That is why
// `spec.connectionRef` is refused on this type rather than ignored.
//
// The part worth the machinery is the redirect list. An OAuth client only
// accepts the callback URLs it was registered with, and a preview
// environment's URL does not exist until somebody opens a pull request. The
// operator is the component that knows: it is what creates environments, and
// what takes them away when a pull request is merged. So it keeps the list in
// step, which is the OAuth chore nobody wants to do by hand and the reason
// this type is worth having at all.

const (
	// condRedirectURIs says whether the client's redirect list still matches
	// the project's environments. It is separate from Ready on purpose: a
	// client whose list has fallen behind still signs people in everywhere it
	// was already registered for, so the claim stays bound and the condition
	// carries what is missing.
	condRedirectURIs = "RedirectURIsInSync"

	// Keys of the binding Secret an oidcClient claim writes into the
	// application namespace. They are spelled as environment variables
	// because that is what they become: `fromResourceClaim` names the key,
	// and an application reading OIDC_ISSUER out of a variable called
	// OIDC_ISSUER is one fewer thing to look up.
	bindingKeyIssuer       = "OIDC_ISSUER"
	bindingKeyClientID     = "CLIENT_ID"
	bindingKeyClientSecret = "CLIENT_SECRET"

	// Keys of the operator's own record of the client, which never leaves the
	// platform namespace.
	oidcKeyIssuer            = "issuer"
	oidcKeyInternalURL       = "internalURL"
	oidcKeyClientID          = "clientID"
	oidcKeyClientSecret      = "clientSecret"
	oidcKeyRegistrationURI   = "registrationURI"
	oidcKeyRegistrationToken = "registrationToken"
)

// oidcGrantTypes is what an application's client is registered for. The
// refresh grant is included because the alternative is signing every visitor
// out an hour after they signed in, and the issuer only mints a refresh token
// for a client that asked to be able to use one.
var oidcGrantTypes = []string{"authorization_code", "refresh_token"}

// oidcContract is the claimContract for type oidcClient. The platform is
// its own provider here, so conn is always nil and never read.
type oidcContract struct{}

func (oidcContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	_ *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	return r.reconcileOIDCClaim(ctx, claim, project)
}

// finalize deregisters the client. No Connection, no branches, and nothing
// deletionPolicy has a say over: what goes is the OAuth client, always.
func (oidcContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	return r.deregisterOIDCClient(ctx, claim)
}

// reconcileOIDCClaim drives an oidcClient claim to Bound: register the client
// if it is not registered, write the binding the application reads, and keep
// the redirect list level with the project's URLs.
func (r *ResourceClaimReconciler) reconcileOIDCClaim(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return r.pending(ctx, claim, "PlatformConfigMissing", err)
	}
	if kitchen.Spec.Auth.SecretRef == nil {
		return r.pending(ctx, claim, "IdentityProviderMissing",
			errors.New("the platform runs no identity provider (spec.auth on the Kitchen object), so there "+
				"is nowhere to register an OAuth client: install the chart with kitchen.auth.enabled, or "+
				"delete this claim"))
	}
	secret := &corev1.Secret{}
	authKey := types.NamespacedName{Namespace: PlatformNamespace, Name: kitchen.Spec.Auth.SecretRef.Name}
	if err := r.Get(ctx, authKey, secret); err != nil {
		return r.pending(ctx, claim, "AuthSecretMissing", err)
	}
	cfg, err := idp.ConfigFromSecret(secret)
	if err != nil {
		return r.pending(ctx, claim, "AuthSecretInvalid", err)
	}

	// The binding Secret lives in the application namespace, which a claim
	// can be bound before the project's first build creates.
	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	wanted, err := r.desiredRedirectURIs(ctx, claim, project, kitchen)
	if err != nil {
		return ctrl.Result{}, err
	}
	registration := idp.ClientRegistration{
		Name:         oidcClientName(project.Name, claim.Name),
		RedirectURIs: wanted,
		GrantTypes:   oidcGrantTypes,
		Scopes:       claim.OIDCClient().Scopes,
	}

	handle, registered, err := r.ensureOIDCClient(ctx, claim, cfg, registration, appNS)
	if err != nil {
		return r.failed(ctx, claim, "ClientNotRegistered", err)
	}
	claim.Status.InstanceID = handle.ID
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "ClientRegistered",
		fmt.Sprintf("registered at %s as client %s", cfg.Issuer, handle.ID))

	syncErr := r.syncRedirectURIs(ctx, claim, cfg, handle, registration, registered)

	// The platform is this claim's provider, and it can declare: a freshly
	// registered OAuth client derives from no data at all — synthetic, on
	// the platform's own word. Declaring it is what keeps an oidcClient
	// claim from reading as "undeclared" in a preview, where undeclared is
	// treated as the worst case.
	claim.Status.DataProvenance = string(database.ProvenanceSynthetic)
	// And what its previews get: the one client, whose redirect list this
	// reconcile has just brought level with them.
	claimType, _ := claim.Type()
	declare(claim, claimType, oidcclient.ProviderName)

	if err := r.bind(ctx, claim, oidcclient.ProviderName, fmt.Sprintf("claim %s bound: %s at %s",
		claim.Name, claim.Spec.Type, cfg.Issuer), map[string]any{
		"type":           claim.Spec.Type,
		"issuer":         cfg.Issuer,
		"client":         handle.ID,
		"secret":         claim.Status.SecretName,
		"dataProvenance": claim.Status.DataProvenance,
		"previewMode":    claim.Status.PreviewMode,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if syncErr != nil {
		if errors.Is(syncErr, idp.ErrNoClientManagement) {
			// An issuer that cannot be asked to change a client will not
			// start being able to on a timer. The condition says what is
			// missing and what to do about it; the next environment change
			// brings this round again anyway.
			log.Info("the issuer maintains no redirect list for us",
				"claim", claim.Name, "issuer", cfg.Issuer)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
	}
	log.Info("reconciled oidc client claim", "claim", claim.Name,
		"client", handle.ID, "redirectURIs", len(claim.Status.RedirectURIs))
	return ctrl.Result{}, nil
}

// ensureOIDCClient makes sure the claim has a client at the issuer and a
// binding Secret the application can read, and returns the handle for
// managing it afterwards.
//
// The operator's own record of the client — in the platform namespace, next
// to the claim — is the source of truth, for the reason the preview gate's is:
// an issuer hands out a client secret once and never again, so a client whose
// credentials the cluster lost is a client nothing can use. Keeping the record
// means a binding Secret somebody deleted is rewritten from what the operator
// already has, rather than costing the application a new client id.
//
// The second return value says whether a client was registered *now*, which
// is what tells the caller the redirect list is already exactly right.
func (r *ResourceClaimReconciler) ensureOIDCClient(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	cfg idp.Config,
	registration idp.ClientRegistration,
	appNS string,
) (idp.ClientHandle, bool, error) {
	record := &corev1.Secret{}
	recordKey := types.NamespacedName{Namespace: claim.Namespace, Name: oidcRecordName(claim.Name)}
	err := r.Get(ctx, recordKey, record)
	switch {
	case err == nil && oidcRecordMatches(record, cfg):
		handle := idp.ClientHandle{
			ID:                string(record.Data[oidcKeyClientID]),
			RegistrationURI:   string(record.Data[oidcKeyRegistrationURI]),
			RegistrationToken: string(record.Data[oidcKeyRegistrationToken]),
		}
		if err := r.writeOIDCBinding(ctx, claim, appNS, cfg, handle.ID,
			string(record.Data[oidcKeyClientSecret])); err != nil {
			return idp.ClientHandle{}, false, err
		}
		return handle, false, nil
	case err != nil && !apierrors.IsNotFound(err):
		return idp.ClientHandle{}, false, err
	}

	// Either there is no client, or the one recorded belongs to an issuer
	// this platform no longer uses — a client at an issuer nobody signs in at
	// is not a client, so this registers again rather than keeping it.
	client, err := idp.New(cfg).Register(ctx, registration)
	if err != nil {
		return idp.ClientHandle{}, false, err
	}
	if err := r.writeOIDCRecord(ctx, claim, cfg, client); err != nil {
		// The client exists at the issuer and its credentials never reached
		// the cluster, which makes it unusable and orphaned. Say which one,
		// because removing it is now somebody's job by hand.
		logf.FromContext(ctx).Error(err, "registered an OAuth client the cluster could not keep",
			"clientId", client.ID, "issuer", cfg.Issuer, "claim", claim.Name)
		return idp.ClientHandle{}, false, err
	}
	if err := r.writeOIDCBinding(ctx, claim, appNS, cfg, client.ID, client.Secret); err != nil {
		return idp.ClientHandle{}, false, err
	}
	return client.Management, true, nil
}

// syncRedirectURIs keeps the registered redirect list level with the URLs the
// project's environments are reachable at.
//
// Nothing is sent when the list has not moved, which is what makes this safe
// to run on every environment change: the status records what was registered,
// so a reconcile that agrees with it costs the issuer nothing.
func (r *ResourceClaimReconciler) syncRedirectURIs(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	cfg idp.Config,
	handle idp.ClientHandle,
	registration idp.ClientRegistration,
	justRegistered bool,
) error {
	if justRegistered || slices.Equal(claim.Status.RedirectURIs, registration.RedirectURIs) {
		claim.Status.RedirectURIs = registration.RedirectURIs
		setClaimCondition(claim, condRedirectURIs, metav1.ConditionTrue, "InSync",
			describeRedirectURIs(registration.RedirectURIs))
		return nil
	}

	if err := idp.New(cfg).UpdateClient(ctx, handle, registration); err != nil {
		if errors.Is(err, idp.ErrNoClientManagement) {
			setClaimCondition(claim, condRedirectURIs, metav1.ConditionFalse, "IssuerCannotBeAsked",
				fmt.Sprintf("the issuer at %s offers no way to change a registered client, so this client "+
					"keeps the %d redirect URI(s) it was registered with and sign-in from any environment "+
					"added since will be refused by the issuer. Add %s at the identity provider by hand, "+
					"or run the platform's own — this is not retried, because an endpoint that is not "+
					"there does not appear",
					cfg.Issuer, len(claim.Status.RedirectURIs), strings.Join(
						missingRedirectURIs(claim.Status.RedirectURIs, registration.RedirectURIs), ", ")))
			return err
		}
		setClaimCondition(claim, condRedirectURIs, metav1.ConditionFalse, "UpdateFailed", err.Error())
		return err
	}

	claim.Status.RedirectURIs = registration.RedirectURIs
	setClaimCondition(claim, condRedirectURIs, metav1.ConditionTrue, "InSync",
		describeRedirectURIs(registration.RedirectURIs))
	return nil
}

// desiredRedirectURIs is every address the project's application can receive
// an authorization code at.
//
// Three things go into it, and the first is why an application can claim
// single sign-on before it has ever been deployed:
//
//   - **The production URL is computed, not observed.** It is the project's
//     name under the platform's base domain, which is knowable the moment the
//     project exists. Waiting for the Environment to publish it would
//     deadlock: the Environment cannot start without the variables the
//     binding Secret holds, and the binding Secret would be waiting for the
//     Environment's URL.
//   - **Every Environment's own URL**, which is what previews contribute —
//     and the only thing that knows a preview's URL is the Environment that
//     has one. An Environment with no URL (a preview the platform refuses to
//     publish, an environment on its way out) contributes nothing.
//   - **Every verified custom Domain** pointing at one of them, because
//     production sign-in happens at the address the visitor typed, and for
//     anybody with a domain of their own that is not the generated one.
//
// Each of those is crossed with the claim's callback paths, and the claim's
// own verbatim redirect URIs — a developer's localhost, typically — are added
// as they are. The result is sorted so that two reconciles of the same world
// produce the same list, which is what lets the status be compared against it
// rather than sent again.
func (r *ResourceClaimReconciler) desiredRedirectURIs(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	kitchen *kitchenv1alpha1.Kitchen,
) ([]string, error) {
	cfg := claim.OIDCClient()

	origins := map[string]struct{}{}
	if kitchen.Spec.BaseDomain != "" {
		origins[fmt.Sprintf("%s://%s", platformScheme(kitchen),
			projectHost(project.Name, kitchen.Spec.BaseDomain))] = struct{}{}
	}

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, client.InNamespace(claim.Namespace)); err != nil {
		return nil, err
	}
	ours := map[string]struct{}{}
	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.ProjectRef.Name != project.Name {
			continue
		}
		ours[env.Name] = struct{}{}
		if !env.DeletionTimestamp.IsZero() || env.Status.URL == "" {
			continue
		}
		origins[strings.TrimSuffix(env.Status.URL, "/")] = struct{}{}
	}

	domains := &kitchenv1alpha1.DomainList{}
	if err := r.List(ctx, domains, client.InNamespace(claim.Namespace)); err != nil {
		return nil, err
	}
	for i := range domains.Items {
		domain := &domains.Items[i]
		if _, ok := ours[domain.Spec.EnvironmentRef.Name]; !ok {
			continue
		}
		if !domain.Status.Verified || !domain.DeletionTimestamp.IsZero() {
			// An unverified domain serves nothing yet, and registering a
			// callback on a hostname somebody has not proven they own is the
			// one entry in this list that would be worth having wrong.
			continue
		}
		mode := domain.Status.TLSMode
		if mode == "" {
			mode = kitchen.Spec.TLS.Mode
		}
		origins[fmt.Sprintf("%s://%s", mode.Scheme(), domain.Spec.Hostname)] = struct{}{}
	}

	uris := map[string]struct{}{}
	for origin := range origins {
		for _, path := range cfg.CallbackPaths {
			uris[origin+"/"+strings.TrimPrefix(path, "/")] = struct{}{}
		}
	}
	for _, uri := range cfg.RedirectURIs {
		if trimmed := strings.TrimSpace(uri); trimmed != "" {
			uris[trimmed] = struct{}{}
		}
	}

	wanted := make([]string, 0, len(uris))
	for uri := range uris {
		wanted = append(wanted, uri)
	}
	slices.Sort(wanted)
	return wanted, nil
}

// deregisterOIDCClient takes the client away with the claim, and the
// operator's record of it with the client.
//
// An OAuth client is not data, so `deletionPolicy` has no say here: Retain
// exists to stop a deletion destroying a production database, and what this
// would leave behind is not a database but a credential that can still ask
// the platform to sign somebody in. Deregistering is the whole point of
// deleting the claim.
//
// A failure the issuer cannot be talked out of does not wedge the deletion —
// it is reported and the claim goes, because a claim that cannot be deleted
// blocks the project's teardown behind it. The client id is logged so that
// what is left at the issuer has a name.
func (r *ResourceClaimReconciler) deregisterOIDCClient(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	log := logf.FromContext(ctx)

	record := &corev1.Secret{}
	recordKey := types.NamespacedName{Namespace: claim.Namespace, Name: oidcRecordName(claim.Name)}
	if err := r.Get(ctx, recordKey, record); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	handle := idp.ClientHandle{
		ID:                string(record.Data[oidcKeyClientID]),
		RegistrationURI:   string(record.Data[oidcKeyRegistrationURI]),
		RegistrationToken: string(record.Data[oidcKeyRegistrationToken]),
	}
	cfg := idp.Config{
		Issuer:  string(record.Data[oidcKeyIssuer]),
		BaseURL: string(record.Data[oidcKeyInternalURL]),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = cfg.Issuer
	}
	// The credential and the address of the directory are the platform's as
	// it is now, not as it was when the client was registered: a rotated key
	// has to be the one that is used, and deregistering goes through the
	// private `/kitchen` prefix, whose address the record predates.
	if live, err := r.platformIDP(ctx); err == nil {
		cfg.ServiceKey = live.ServiceKey
		cfg.DirectoryURL = live.DirectoryURL
	}

	if err := idp.New(cfg).DeleteClient(ctx, handle); err != nil {
		log.Error(err, "could not deregister the claim's OAuth client; it is left at the issuer",
			"claim", claim.Name, "clientId", handle.ID, "issuer", cfg.Issuer)
	}
	if err := r.Delete(ctx, record); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// platformIDP reads the identity provider the platform runs now — the
// credential the operator authenticates with, and the two addresses it
// reaches the issuer at. It is looked up again at deletion rather than kept
// in the record: the record is about the client, and neither a rotated key
// nor a moved listener is the client changing.
func (r *ResourceClaimReconciler) platformIDP(ctx context.Context) (idp.Config, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return idp.Config{}, err
	}
	if kitchen.Spec.Auth.SecretRef == nil {
		return idp.Config{}, errors.New("the platform runs no identity provider")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: kitchen.Spec.Auth.SecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return idp.Config{}, err
	}
	return idp.ConfigFromSecret(secret)
}

// writeOIDCRecord stores what the operator needs to manage the client later,
// in the platform namespace and nowhere else. The registration token is what
// authorizes changing the client at an issuer that supports RFC 7592, so it
// is deliberately not part of the binding the application reads: an
// application that could edit its own redirect list could point it anywhere.
func (r *ResourceClaimReconciler) writeOIDCRecord(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	cfg idp.Config,
	client *idp.RegisteredClient,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      oidcRecordName(claim.Name),
		Namespace: claim.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{
			labelProject:      claim.Spec.ProjectRef.Name,
			labelClaim:        claim.Name,
			labelManagedByKey: labelManagedByValue,
		}
		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			oidcKeyIssuer:            cfg.Issuer,
			oidcKeyInternalURL:       internalURLOf(cfg),
			oidcKeyClientID:          client.ID,
			oidcKeyClientSecret:      client.Secret,
			oidcKeyRegistrationURI:   client.Management.RegistrationURI,
			oidcKeyRegistrationToken: client.Management.RegistrationToken,
		}
		return nil
	})
	return err
}

// writeOIDCBinding writes the three things an application needs to be an
// OpenID Connect client of the platform, in the vocabulary
// Project.spec.env's fromResourceClaim selects on.
func (r *ResourceClaimReconciler) writeOIDCBinding(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	appNS string,
	cfg idp.Config,
	clientID, clientSecret string,
) error {
	name := claimSecretName(claim.Name)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = map[string]string{
			labelProject:      claim.Spec.ProjectRef.Name,
			labelClaim:        claim.Name,
			labelManagedByKey: labelManagedByValue,
		}
		secret.Data = map[string][]byte{
			// The public issuer, never the address the operator reaches it
			// at: this one ends up in a browser's address bar.
			bindingKeyIssuer:       []byte(cfg.Issuer),
			bindingKeyClientID:     []byte(clientID),
			bindingKeyClientSecret: []byte(clientSecret),
		}
		return nil
	}); err != nil {
		return err
	}
	claim.Status.SecretName = name
	return nil
}

// oidcRecordMatches reports whether the operator's record is complete and
// still describes a client at the issuer the platform uses now.
func oidcRecordMatches(record *corev1.Secret, cfg idp.Config) bool {
	return string(record.Data[oidcKeyClientID]) != "" &&
		string(record.Data[oidcKeyClientSecret]) != "" &&
		string(record.Data[oidcKeyIssuer]) == cfg.Issuer &&
		string(record.Data[oidcKeyInternalURL]) == internalURLOf(cfg)
}

// oidcRecordName is the operator's record of one claim's client. It lives in
// the platform namespace beside the claim, and never in the application's.
func oidcRecordName(claim string) string {
	return claim + "-oidc-client"
}

// oidcClientName is what the client is called at the issuer, which is what a
// person sees on the consent screen: the project first, because that is the
// application they think they are signing in to.
func oidcClientName(project, claim string) string {
	if project == claim {
		return project
	}
	return fmt.Sprintf("%s (%s)", project, claim)
}

// describeRedirectURIs is the condition message for a list that is in step —
// short, and naming the URIs while there are few enough to read.
func describeRedirectURIs(uris []string) string {
	const named = 4
	if len(uris) == 0 {
		return "the client has no redirect URIs: the project publishes no URL yet"
	}
	if len(uris) <= named {
		return "the client accepts " + strings.Join(uris, ", ")
	}
	return fmt.Sprintf("the client accepts %s and %d more", strings.Join(uris[:named], ", "), len(uris)-named)
}

// missingRedirectURIs is what the desired list has that the registered one
// does not — the URIs somebody would have to add by hand.
func missingRedirectURIs(registered, wanted []string) []string {
	missing := make([]string, 0, len(wanted))
	for _, uri := range wanted {
		if !slices.Contains(registered, uri) {
			missing = append(missing, uri)
		}
	}
	return missing
}
