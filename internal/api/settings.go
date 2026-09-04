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
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/ui"
)

// The settings endpoints are the Kitchen singleton as the UI sees it: the
// platform's runtime configuration, which is a custom resource precisely so
// it can be edited from here rather than through another `helm upgrade`.

type settingsView struct {
	BaseDomain       string `json:"baseDomain"`
	APIExternalURL   string `json:"apiExternalURL,omitempty"`
	GatewayClassName string `json:"gatewayClassName,omitempty"`
	AuthEnabled      bool   `json:"authEnabled"`
	AuthHost         string `json:"authHost,omitempty"`
	BuildStrategy    string `json:"buildStrategy,omitempty"`
	BuildConcurrency int32  `json:"buildConcurrency,omitempty"`
	// BuildCPU and BuildMemory are `spec.builds.resources`: the ceiling one
	// build runs under, reserved for it and capped at it. They are beside the
	// concurrency because the two are one decision — the concurrency times
	// the ceiling is the whole of what the platform's builds can take from
	// the cluster — and reading either on its own answers nothing.
	//
	// Empty is a ceiling the installation has cleared, which is the unbounded
	// build everything ran before the field existed. It is why these carry no
	// omitempty: a dashboard has to be able to tell "no ceiling" from "an API
	// too old to have one".
	BuildCPU    string `json:"buildCPU"`
	BuildMemory string `json:"buildMemory"`
	// BuildTimeoutMinutes is `spec.builds.timeoutMinutes`: how long one build
	// may run before the platform ends it, as the build would actually get it
	// — a singleton that names no deadline is reported as the platform's own
	// default rather than as an empty box, the way the ceiling is. No
	// omitempty, for the reason the two quantities above carry none: 0 is a
	// setting here — no deadline at all — and a dashboard has to be able to
	// tell it from an API too old to have the field.
	BuildTimeoutMinutes int32 `json:"buildTimeoutMinutes"`
	// No omitempty: 0 is a setting here — keep every release — not an absent
	// one, and the dashboard has to be able to tell the two apart.
	ReleaseRetention int32  `json:"releaseRetention"`
	LogRetentionDays int32  `json:"logRetentionDays,omitempty"`
	GatewayAddress   string `json:"gatewayAddress,omitempty"`
	// PublicAddresses is `spec.ingress.publicAddresses`: where the internet
	// reaches this platform, when a router forwards to the Gateway from an
	// address the cluster never sees. It is here beside the Gateway's own
	// address because the pair is what an operator diagnosing an unreachable
	// install needs to read together — and because a `dns.mismatch` finding
	// names the addresses it compared, which is only meaningful if the
	// settings page can say where they came from.
	PublicAddresses []string `json:"publicAddresses,omitempty"`
	// Backup is the scheduled backup as this route can edit it — the
	// schedule, the suspend and the retention — plus what it has been doing,
	// so that a PATCH answers with the state it produced rather than making
	// the caller fetch it from somewhere else.
	//
	// The destination is in it and its credential is not: writing a
	// credential is PUT /platform/backup/destination's, deliberately, because
	// this route must never carry one.
	Backup     backupScheduleView `json:"backup"`
	Conditions []conditionView    `json:"conditions,omitempty"`
	// Operators is `spec.access.operators`: who holds the platform role. It
	// is here rather than on a surface of its own because this route already
	// carries the base domain, the issuer and the gateway address, and is
	// already the operator's for that reason — and because a list that is
	// enforced against, seeded on upgrade and served by nothing is a list
	// somebody has to reach for kubectl to read.
	//
	// No omitempty, which matters here as much as it does on the field
	// itself: `null` is "nobody has ever said who the operators are, and the
	// reconciler will seed the list", `[]` is "somebody narrowed it to
	// nobody". A dashboard cannot tell those apart from an absent key.
	Operators []operatorView `json:"operators"`
}

// operatorView is one entry of the platform's operator list. It is the
// membership view minus the two things a platform grant does not have: there
// is exactly one platform role worth writing down, so there is no role to
// report, and the list holds people rather than machine accounts.
type operatorView struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
}

// operatorViews is the list as the dashboard reads it, in the order it is
// written down — the order somebody editing the object sees, for the same
// reason listMembers keeps `spec.access`'s.
//
// A nil list stays nil so that it marshals as `null` rather than as `[]`.
func operatorViews(operators []kitchenv1alpha1.AccessSubject) []operatorView {
	if operators == nil {
		return nil
	}
	views := make([]operatorView, 0, len(operators))
	for _, operator := range operators {
		views = append(views, operatorView{Subject: operator.Subject, Email: operator.Email})
	}
	return views
}

func newSettingsView(kitchen *kitchenv1alpha1.Kitchen) settingsView {
	view := settingsView{
		BaseDomain:          kitchen.Spec.BaseDomain,
		APIExternalURL:      externalURL(kitchen),
		GatewayClassName:    kitchen.Spec.Ingress.GatewayClassName,
		AuthEnabled:         kitchen.Spec.Auth.Enabled,
		BuildStrategy:       string(kitchen.Spec.Builds.DefaultStrategy),
		BuildConcurrency:    kitchen.Spec.Builds.Concurrency,
		BuildCPU:            kitchen.Spec.Builds.Resources.CPU,
		BuildMemory:         kitchen.Spec.Builds.Resources.Memory,
		BuildTimeoutMinutes: buildTimeoutMinutes(kitchen),
		ReleaseRetention:    kitchen.Spec.Builds.ReleaseRetention,
		LogRetentionDays:    kitchen.Spec.Observability.ClickHouse.RetentionDays,
		GatewayAddress:      kitchen.Status.GatewayAddress,
		PublicAddresses:     kitchen.Spec.Ingress.PublicAddresses,
		Backup:              newBackupScheduleView(kitchen),
		Conditions:          conditionViews(kitchen.Status.Conditions),
		Operators:           operatorViews(kitchen.Spec.Access.Operators),
	}
	if cfg, err := issuerFor(kitchen); err == nil {
		view.AuthHost = cfg.issuer
	}
	return view
}

// buildTimeoutMinutes is the deadline a build started now would get: the
// platform's own setting, or the operator's compiled default where the
// singleton names none — which is what an installation that predates the field
// has, and what the reconciler gives it.
func buildTimeoutMinutes(kitchen *kitchenv1alpha1.Kitchen) int32 {
	if kitchen.Spec.Builds.TimeoutMinutes == nil {
		return controller.DefaultBuildTimeoutMinutes
	}
	return *kitchen.Spec.Builds.TimeoutMinutes
}

func (s *Server) getKitchen(req *http.Request) (*kitchenv1alpha1.Kitchen, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	err := s.Client.Get(req.Context(), types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen)
	return kitchen, err
}

func (s *Server) getSettings(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSettingsView(kitchen))
}

// patchSettingsRequest carries the fields the UI can change. Everything else
// on the singleton — the base domain, the issuer, the ingress — shapes URLs
// and credentials the platform has already handed out, so changing those
// stays a deliberate kubectl operation for now.
type patchSettingsRequest struct {
	BuildStrategy    *string `json:"buildStrategy"`
	BuildConcurrency *int32  `json:"buildConcurrency"`
	// BuildCPU and BuildMemory are the per-build ceiling, as Kubernetes
	// quantities. They are pointers for the reason `operators` is one: the
	// empty string is a setting here — the installation that has decided its
	// builds are unbounded — and a request that does not mention the field
	// must not be able to clear it.
	BuildCPU    *string `json:"buildCPU"`
	BuildMemory *string `json:"buildMemory"`
	// BuildTimeoutMinutes is how long a build may run, in minutes. A pointer
	// for the same reason: 0 is the installation that has decided its builds
	// have no deadline, and a request that does not mention the field must
	// not be able to clear it.
	BuildTimeoutMinutes *int32 `json:"buildTimeoutMinutes"`
	ReleaseRetention    *int32 `json:"releaseRetention"`
	LogRetentionDays    *int32 `json:"logRetentionDays"`
	// BackupSchedule, BackupSuspend, BackupKeepLast and BackupKeepDays are
	// the scheduled backup's ordinary settings: when it runs, whether it is
	// paused, and how much of the destination survives a prune. The
	// destination itself is not here and never will be — it carries a
	// credential, and this route must not.
	//
	// Pointers, like everything else on this request: the empty schedule is a
	// setting (no scheduled backup at all), and a request that does not
	// mention the field must not be able to clear it.
	BackupSchedule *string `json:"backupSchedule"`
	BackupSuspend  *bool   `json:"backupSuspend"`
	BackupKeepLast *int32  `json:"backupKeepLast"`
	BackupKeepDays *int32  `json:"backupKeepDays"`
	// Operators replaces the whole platform access list, and is a pointer so
	// that a request which does not mention it cannot disturb it — the
	// difference between an absent list and an empty one is load-bearing on
	// this field (AccessSpec), and only a pointer keeps both readable here.
	Operators *[]operatorRequest `json:"operators"`
}

// operatorRequest names one account to make an operator, the same two ways a
// membership write names one: an `email` the platform resolves at the identity
// provider, or a `subject` the caller already holds. Exactly one of them, and
// the resolution is resolveMember's — deciding who an address belongs to has
// one implementation, and it is members.go's.
type operatorRequest struct {
	Email   string `json:"email,omitempty"`
	Subject string `json:"subject,omitempty"`
}

func (s *Server) patchSettings(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := patchSettingsRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	patch := settingsPatch(kitchen, body)
	if body.BuildStrategy != nil {
		strategy := kitchenv1alpha1.BuildStrategy(strings.TrimSpace(*body.BuildStrategy))
		switch strategy {
		case kitchenv1alpha1.BuildStrategyAuto, kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks:
			kitchen.Spec.Builds.DefaultStrategy = strategy
		default:
			badRequest(w, "buildStrategy must be auto, dockerfile or buildpacks (got %q)", *body.BuildStrategy)
			return
		}
	}
	if body.BuildConcurrency != nil {
		if *body.BuildConcurrency < 1 {
			badRequest(w, "buildConcurrency must be at least 1 (got %d)", *body.BuildConcurrency)
			return
		}
		kitchen.Spec.Builds.Concurrency = *body.BuildConcurrency
	}
	if body.BuildCPU != nil {
		ceiling, ok := buildCeiling(w, "buildCPU", *body.BuildCPU)
		if !ok {
			return
		}
		kitchen.Spec.Builds.Resources.CPU = ceiling
	}
	if body.BuildMemory != nil {
		ceiling, ok := buildCeiling(w, "buildMemory", *body.BuildMemory)
		if !ok {
			return
		}
		kitchen.Spec.Builds.Resources.Memory = ceiling
	}
	if body.BuildTimeoutMinutes != nil {
		// Zero is a setting here and not a mistake: an installation that
		// would rather end a runaway build by hand than lose an hour's work
		// to a number can say so. Negative is not a setting.
		if *body.BuildTimeoutMinutes < 0 {
			badRequest(w, "buildTimeoutMinutes cannot be negative (got %d); 0 means no deadline",
				*body.BuildTimeoutMinutes)
			return
		}
		kitchen.Spec.Builds.TimeoutMinutes = body.BuildTimeoutMinutes
	}
	if body.ReleaseRetention != nil {
		// Zero is the one setting here that means "no bound": every release a
		// project ever built is kept, which is what the platform did before
		// there was a count at all.
		if *body.ReleaseRetention < 0 {
			badRequest(w, "releaseRetention cannot be negative (got %d); 0 keeps every release", *body.ReleaseRetention)
			return
		}
		kitchen.Spec.Builds.ReleaseRetention = *body.ReleaseRetention
	}
	if body.LogRetentionDays != nil {
		if *body.LogRetentionDays < 1 {
			badRequest(w, "logRetentionDays must be at least 1 (got %d)", *body.LogRetentionDays)
			return
		}
		kitchen.Spec.Observability.ClickHouse.RetentionDays = *body.LogRetentionDays
	}
	if !s.applyBackupSettings(w, kitchen, body) {
		return
	}
	was := kitchen.Spec.Access.Operators
	if body.Operators != nil {
		operators, ok := s.resolveOperators(req.Context(), w, *body.Operators)
		if !ok {
			return
		}
		kitchen.Spec.Access.Operators = operators
	}

	// Platform settings are the operator's own configuration, so a change to
	// them is recorded like any other: what moved, and who moved it. Deciding
	// that an account owns the platform is at least as consequential as a
	// project grant, so the operator list is recorded the way membership is —
	// by who came on and who came off, named as a person reads them.
	fields := changedSettingsFields(body)
	transition := audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindKitchen,
		Operation: clickhouse.AuditUpdate,
		Reason:    "platform settings were changed",
		Details:   map[string]any{"fields": fields},
	}
	if added, removed := operatorsMoved(was, kitchen.Spec.Access.Operators); len(added)+len(removed) > 0 {
		transition.From, transition.To = describeOperators(was), describeOperators(kitchen.Spec.Access.Operators)
		transition.Reason = describeOperatorChange(added, removed)
		if len(fields) > 1 {
			// The request carried more than the list, so the record says both
			// rather than letting the louder half hide the other.
			transition.Reason = "platform settings were changed, and " + transition.Reason
		}
		transition.Details["operators"] = map[string]any{
			"added":   subjectsOf(added),
			"removed": subjectsOf(removed),
			"change":  "operators",
		}
		// Changing who owns the platform is the highest-consequence write
		// this API has, so the record is classified `access` and separable
		// from the base-domain edit it may have arrived alongside.
		transition.Privileged = audit.PrivilegeAccess
	}
	if !s.recorded(w, req, transition) {
		return
	}
	if err := s.Client.Patch(req.Context(), kitchen, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(req.Context())
	s.log().Info("platform settings changed through the api", "caller", callerName(caller))
	if body.Operators != nil {
		s.log().Info("the platform's operator list was changed through the api",
			"operators", len(kitchen.Spec.Access.Operators), "caller", callerName(caller))
	}
	writeJSON(w, http.StatusOK, newSettingsView(kitchen))
}

// settingsPatch is how a settings change reaches the cluster, and whether it
// carries the caller's resourceVersion depends on what the request is
// changing.
//
// A request that names `operators` gets the optimistic lock, for exactly
// membershipPatch's reason and with more at stake. The list is replaced
// wholesale, and the decision that it may be — the last-operator check — was
// made against the list this handler read. Two operators removing each other
// at the same time is the case that rule exists to prevent, and a lost update
// is how it gets through: from [A, B, C], A removing C and B removing A land
// as [B, C], with C back on a list they were taken off and the check having
// run against a list that no longer exists. A conflict answers 409, which is
// the client's cue to re-read and try again.
//
// A request that does not name it gets a plain merge patch. The other fields
// are independent scalars, each written from the caller's own body and
// decided against nothing that was read: failing "set the build concurrency to
// 4" because somebody else changed the log retention a moment earlier would be
// a conflict about nothing, on the platform's busiest object.
func settingsPatch(kitchen *kitchenv1alpha1.Kitchen, body patchSettingsRequest) client.Patch {
	base := kitchen.DeepCopy()
	if body.Operators == nil {
		return client.MergeFrom(base)
	}
	return client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
}

// buildCeiling is one half of what a build may take, as the caller wrote it:
// a Kubernetes quantity, or the empty string for no ceiling at all.
//
// Clearing it is allowed and is not a mistake to be talked out of — an
// installation with dedicated build capacity is exactly the one that should be
// able to say so — but it is the only way to end up unbounded, and it has to
// be written rather than fallen into. Zero is refused for that reason: a
// ceiling of nothing is not "no ceiling", it is a build that cannot start.
func buildCeiling(w http.ResponseWriter, field, value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", true
	}
	quantity, err := resource.ParseQuantity(trimmed)
	if err != nil {
		badRequest(w, "%s must be a Kubernetes quantity like 2, 500m or 4Gi (got %q), "+
			"or empty for no ceiling at all", field, value)
		return "", false
	}
	if quantity.Sign() <= 0 {
		badRequest(w, "%s must be more than zero (got %q); empty is how a build is left unbounded",
			field, value)
		return "", false
	}
	return trimmed, true
}

// lastOperatorRefusal is what a write that would leave the platform with no
// operator is answered with. It follows the rule every refusal here follows —
// say what is wrong *and* what would fix it — and it is the platform's version
// of lastAdminRefusal, for the same reason: nobody is left who can appoint the
// next one, and the way back is the kubectl this platform exists to avoid.
const lastOperatorRefusal = "the operator list cannot be emptied: a platform with no operator has nobody " +
	"left who can appoint one, and the only way back is editing the Kitchen object with kubectl. " +
	"Name whoever is to stay — remove the others, and keep the last"

// resolveOperators turns what a caller named into the entries that will be
// written, answering the request itself on every path that is not a list.
//
// Each entry goes through resolveMember, which is members.go's: an address is
// resolved to the issuer's `sub` at write time, a subject is taken as given,
// and a subject that is really an address is refused. That is deliberate reuse
// rather than a second resolver — an account is the same account whether it is
// being put on a project or on the platform, and two implementations of "who
// holds anna@example.com" is two answers waiting to disagree.
func (s *Server) resolveOperators(
	ctx context.Context,
	w http.ResponseWriter,
	requested []operatorRequest,
) ([]kitchenv1alpha1.AccessSubject, bool) {
	if len(requested) == 0 {
		writeJSON(w, http.StatusConflict, errorBody{Error: lastOperatorRefusal})
		return nil, false
	}

	operators := make([]kitchenv1alpha1.AccessSubject, 0, len(requested))
	for _, entry := range requested {
		grant, ok := s.resolveMember(ctx, w, addMemberRequest{
			Email:   strings.TrimSpace(entry.Email),
			Subject: strings.TrimSpace(entry.Subject),
		})
		if !ok {
			return nil, false
		}
		for _, already := range operators {
			if sameSubject(already.Subject, grant.Subject) {
				badRequest(w, "%s is named twice: the operator list is a set, "+
					"and one account on it twice is one entry nobody can remove",
					describeMember(grant))
				return nil, false
			}
		}
		operators = append(operators, grant.AccessSubject)
	}
	return operators, true
}

// operatorsMoved is who came on and who came off, comparing by the spelling of
// the subject the way membership does (sameSubject). An entry whose address
// the identity provider spells differently today has not moved.
func operatorsMoved(was, now []kitchenv1alpha1.AccessSubject) (added, removed []kitchenv1alpha1.AccessSubject) {
	for _, entry := range now {
		if !holdsOperator(was, entry.Subject) {
			added = append(added, entry)
		}
	}
	for _, entry := range was {
		if !holdsOperator(now, entry.Subject) {
			removed = append(removed, entry)
		}
	}
	return added, removed
}

func holdsOperator(operators []kitchenv1alpha1.AccessSubject, subject string) bool {
	for _, entry := range operators {
		if sameSubject(entry.Subject, subject) {
			return true
		}
	}
	return false
}

// describeOperatorChange is the audit record's sentence: who now owns the
// platform and who no longer does, by address where there is one.
func describeOperatorChange(added, removed []kitchenv1alpha1.AccessSubject) string {
	parts := make([]string, 0, 2)
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("%s now holds the operator role", describeOperators(added)))
	}
	if len(removed) > 0 {
		parts = append(parts, fmt.Sprintf("%s no longer holds the operator role", describeOperators(removed)))
	}
	return strings.Join(parts, "; ")
}

// describeOperators names a list the way describeMember names one entry: by
// address where the entry carries one, and by the opaque subject where it does
// not.
func describeOperators(operators []kitchenv1alpha1.AccessSubject) string {
	names := make([]string, 0, len(operators))
	for _, operator := range operators {
		names = append(names, describeMember(kitchenv1alpha1.AccessGrant{AccessSubject: operator}))
	}
	return strings.Join(names, ", ")
}

// subjectsOf is the canonical identifiers alone, for the audit record's
// details: the addresses are already in the sentence, and the `sub` is what
// anything reading the record back would look the account up by.
func subjectsOf(operators []kitchenv1alpha1.AccessSubject) []string {
	subjects := make([]string, 0, len(operators))
	for _, operator := range operators {
		subjects = append(subjects, operator.Subject)
	}
	return subjects
}

// UIConfig resolves the dashboard's bootstrap configuration off the Kitchen
// object, with the same derivations the API's own token check uses — so the
// login the UI starts is always aimed at the issuer the API will accept a
// token from.
func UIConfig(c client.Client, clientID string) func(ctx context.Context) (ui.Config, error) {
	return func(ctx context.Context) (ui.Config, error) {
		kitchen := &kitchenv1alpha1.Kitchen{}
		if err := c.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
			return ui.Config{}, err
		}
		cfg := ui.Config{ClientID: clientID, APIURL: externalURL(kitchen)}
		if issuer, err := issuerFor(kitchen); err == nil {
			cfg.Issuer = issuer.issuer
		}
		return cfg, nil
	}
}

// changedSettingsFields names the settings a PATCH carried, for the audit
// record's details.
func changedSettingsFields(body patchSettingsRequest) []string {
	fields := []string{}
	for _, field := range []struct {
		name    string
		changed bool
	}{
		{"buildStrategy", body.BuildStrategy != nil},
		{"buildConcurrency", body.BuildConcurrency != nil},
		{"buildCPU", body.BuildCPU != nil},
		{"buildMemory", body.BuildMemory != nil},
		{"buildTimeoutMinutes", body.BuildTimeoutMinutes != nil},
		{"releaseRetention", body.ReleaseRetention != nil},
		{"logRetentionDays", body.LogRetentionDays != nil},
		{"backupSchedule", body.BackupSchedule != nil},
		{"backupSuspend", body.BackupSuspend != nil},
		{"backupKeepLast", body.BackupKeepLast != nil},
		{"backupKeepDays", body.BackupKeepDays != nil},
		{"operators", body.Operators != nil},
	} {
		if field.changed {
			fields = append(fields, field.name)
		}
	}
	return fields
}

// applyBackupSettings writes the scheduled backup's ordinary settings, and
// refuses the two combinations admission would refuse.
//
// Both refusals are enforced here as well as by the CRD's own CEL rules, and
// that is not belt and braces: admission answers with a rule's message, and
// this answers with the field, the reason and the route that fixes it. A
// caller who set a schedule with nowhere to write to should be told what to do
// about it by the thing they were talking to.
func (s *Server) applyBackupSettings(
	w http.ResponseWriter,
	kitchen *kitchenv1alpha1.Kitchen,
	body patchSettingsRequest,
) bool {
	spec := &kitchen.Spec.Backup
	if body.BackupSchedule != nil {
		schedule := strings.TrimSpace(*body.BackupSchedule)
		if schedule != "" {
			if fields := len(strings.Fields(schedule)); fields != 5 {
				badRequest(w, "backupSchedule is a five-field cron expression in UTC, like \"0 3 * * *\" "+
					"(got %q, which has %d fields). An empty schedule turns the scheduled backup off.",
					schedule, fields)
				return false
			}
			if spec.Destination == nil {
				badRequest(w, "backupSchedule needs somewhere to write to: an archive on a volume on "+
					"this cluster does not survive the loss of this cluster, so there is deliberately "+
					"no local destination. Set one with PUT /api/v1/platform/backup/destination first.")
				return false
			}
		}
		spec.Schedule = schedule
	}
	if body.BackupSuspend != nil {
		spec.Suspend = *body.BackupSuspend
	}
	for _, bound := range []struct {
		name  string
		value *int32
		into  **int32
	}{
		{"backupKeepLast", body.BackupKeepLast, &spec.Retention.KeepLast},
		{"backupKeepDays", body.BackupKeepDays, &spec.Retention.KeepDays},
	} {
		if bound.value == nil {
			continue
		}
		switch {
		case *bound.value < 0:
			badRequest(w, "%s cannot be negative (got %d); 0 removes the bound, which keeps every archive",
				bound.name, *bound.value)
			return false
		case *bound.value == 0:
			// Removing a bound is a real setting and the only way back to
			// keeping everything, so it has to be expressible.
			*bound.into = nil
		default:
			*bound.into = ptr.To(*bound.value)
		}
	}
	if (spec.Retention.KeepLast != nil || spec.Retention.KeepDays != nil) && spec.Destination == nil {
		badRequest(w, "a backup retention needs a destination to apply to: it prunes what is at the "+
			"destination, and with no destination it overrides nothing.")
		return false
	}
	return true
}
