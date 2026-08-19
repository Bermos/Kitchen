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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
)

// condOperatorsConfigured answers "who are my operators, and why" without
// anyone having to reconstruct it. It is the one place the platform says
// whether the operator list was written by a person, seeded from the accounts
// that existed, or is still waiting for the first account to exist at all.
const condOperatorsConfigured = "OperatorsConfigured"

// operatorsInMessage is how many operators a condition message names before
// it stops and counts the rest. Long enough to read a small team back, short
// enough that `kubectl describe` stays readable.
const operatorsInMessage = 5

// reconcileAccess seeds the platform's operator list, once, from the accounts
// the identity provider holds.
//
// The rule is one rule, and it covers both of the cases it was written for.
// **An absent list means nobody has ever said who the operators are**, so the
// accounts that exist become the answer:
//
//   - On a fresh install, the only account is the one the bootstrap link
//     created, so this seeds exactly one operator — the first administrator,
//     which is what docs/AUTH.md says the bootstrap account is.
//   - On an installation upgrading into enforcement, every account today can
//     call every route, so every account read honestly *is* an operator, and
//     this seeds all of them. Enforcing against an empty list instead would
//     lock a platform out of itself on a minor version bump, with no way back
//     that does not involve kubectl.
//
// That is why there is one code path here and not two: the two cases differ
// only in how many accounts the identity provider happens to hold.
//
// Two things it will not do. It never writes an **empty** list: a fresh
// install reconciles before anybody has followed the bootstrap link, and
// writing `operators: []` there would turn "nobody has said yet" into
// "somebody said nobody" — after which the bootstrap account would never
// become an operator. And it never touches a list that is already there,
// empty included: an empty list is somebody narrowing the platform to nobody
// on purpose, which is a decision, not an accident.
func (r *KitchenReconciler) reconcileAccess(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	if kitchen.Spec.Access.Operators != nil {
		// Somebody has decided. Report what the decision is — including the
		// deliberate "nobody" — and go no further: this is the only write
		// this reconciler makes to the spec, and it makes it once.
		operators := kitchen.Spec.Access.Operators
		if len(operators) == 0 {
			setCond(condOperatorsConfigured, metav1.ConditionTrue, "NobodyIsAnOperator",
				"the operator list is empty: no account holds the operator role, and the list is left as it is")
			return true
		}
		setCond(condOperatorsConfigured, metav1.ConditionTrue, "OperatorsNamed",
			fmt.Sprintf("%s hold the operator role", describeOperators(operators)))
		return true
	}

	ref := kitchen.Spec.Auth.SecretRef
	if ref == nil {
		// No identity provider the operator can read accounts from, so there
		// is nothing to seed from and nothing to complain about on every
		// reconcile. The list stays absent, and stays seedable.
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condOperatorsConfigured)
		return true
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}, secret); err != nil {
		setCond(condOperatorsConfigured, metav1.ConditionFalse, "AuthSecretMissing", err.Error())
		return false
	}
	cfg, err := idp.ConfigFromSecret(secret)
	if err != nil {
		setCond(condOperatorsConfigured, metav1.ConditionFalse, "AuthSecretInvalid", err.Error())
		return false
	}

	accounts, err := idp.New(cfg).Accounts(ctx)
	if err != nil {
		setCond(condOperatorsConfigured, metav1.ConditionFalse, "DirectoryUnavailable", err.Error())
		return false
	}

	operators := operatorsFrom(accounts)
	if len(operators) == 0 {
		// A platform nobody has signed up to yet. Saying so is the point:
		// this is what a fresh install looks like between `helm install` and
		// somebody opening the bootstrap link, and the next reconcile seeds
		// it the moment that account exists.
		setCond(condOperatorsConfigured, metav1.ConditionFalse, "AwaitingFirstAccount",
			"the identity provider holds no accounts yet: the first account the bootstrap link creates "+
				"becomes the first operator")
		return false
	}

	if err := r.seedOperators(ctx, kitchen, operators); err != nil {
		setCond(condOperatorsConfigured, metav1.ConditionFalse, "SeedNotWritten", err.Error())
		return false
	}

	logf.FromContext(ctx).Info("seeded the platform's operator list from the accounts that exist",
		"operators", len(operators), "issuer", cfg.Issuer)
	setCond(condOperatorsConfigured, metav1.ConditionTrue, "OperatorsSeeded",
		fmt.Sprintf("no operators had ever been named, so the list was seeded from the %d account(s) the "+
			"identity provider holds: %s. Narrowing it is an edit to spec.access.operators",
			len(operators), describeOperators(operators)))
	return true
}

// operatorsFrom turns the account directory into operator entries, in the
// order the directory answered — oldest account first, so the bootstrap
// account leads the list on a fresh install.
//
// The subject is the issuer's `sub`, which is what an access entry is
// canonically written with; the address rides along so that `kubectl get
// kitchen -o yaml` and the settings screen read as names rather than as
// opaque strings. An account the issuer named no subject for is skipped: the
// CRD refuses an empty subject, and one bad row must not cost the whole seed.
func operatorsFrom(accounts []idp.Account) []kitchenv1alpha1.AccessSubject {
	operators := make([]kitchenv1alpha1.AccessSubject, 0, len(accounts))
	for _, account := range accounts {
		if account.Subject == "" {
			continue
		}
		operators = append(operators, kitchenv1alpha1.AccessSubject{
			Subject: account.Subject,
			Email:   account.Email,
		})
	}
	return operators
}

// seedOperators writes the list, as a merge patch carrying that one field.
//
// This is the only place the operator writes a Kitchen's *spec*, so there is
// no precedent to follow, and a full update is the wrong instrument: the
// Kitchen singleton is also edited through `PATCH /settings`, and an update
// would send back every other field as this reconcile happened to read them —
// quietly reverting a base domain somebody changed a moment ago. A merge
// patch of the one field cannot: it carries no resource version, so it never
// conflicts, and it names nothing it does not mean to set.
//
// The patch goes through a copy so that the status this reconcile has been
// assembling survives it — the API server answers a spec patch with the whole
// object, status included, and decoding that answer over the live one would
// throw away every condition set so far. The resource version comes back, so
// the status update at the end of the reconcile still lands.
func (r *KitchenReconciler) seedOperators(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	operators []kitchenv1alpha1.AccessSubject,
) error {
	seeding := kitchen.DeepCopy()
	patch := client.MergeFrom(seeding.DeepCopy())
	seeding.Spec.Access.Operators = operators
	if err := r.Patch(ctx, seeding, patch); err != nil {
		return err
	}
	kitchen.Spec.Access.Operators = seeding.Spec.Access.Operators
	kitchen.ResourceVersion = seeding.ResourceVersion
	return nil
}

// describeOperators names the operators for a condition message, by address
// where there is one and by subject otherwise, and counts the rest once the
// message has said enough.
func describeOperators(operators []kitchenv1alpha1.AccessSubject) string {
	names := make([]string, 0, operatorsInMessage)
	for _, operator := range operators {
		if len(names) == operatorsInMessage {
			return fmt.Sprintf("%s and %d more", strings.Join(names, ", "), len(operators)-operatorsInMessage)
		}
		if operator.Email != "" {
			names = append(names, operator.Email)
			continue
		}
		names = append(names, operator.Subject)
	}
	return strings.Join(names, ", ")
}
