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
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
	"github.com/Bermos/Kitchen/internal/provider"
	"github.com/Bermos/Kitchen/internal/retention"
)

// Assembling the pack.
//
// The shape of this file is the performance answer as much as the correctness
// one: everything the document needs is read **once**, in a fixed number of
// calls that does not grow with the project's history — five list calls
// against the manager's cache, three store queries, and two narrow store
// queries per deployed environment for the drift derivation. Nothing here
// fans out per artifact, per attestation or per decision, which is what keeps
// a project's whole quarter inside the minute the acceptance criterion
// allows.
//
// Every list is sorted before it is written, by a key that is total: a name,
// or a timestamp with a name behind it. Nothing is left in the order a map
// iterated or a store answered in, because that is the one class of bug that
// makes a document reproduce ninety-nine times out of a hundred.

// assembly is everything one pack is built from, gathered so the assembler
// takes one argument rather than eight.
type assembly struct {
	project *kitchenv1alpha1.Project
	kitchen *kitchenv1alpha1.Kitchen
	store   logReader
	from    time.Time
	to      time.Time
	key     *attestation.ECDSAKey
	keyErr  error
}

// assembleAuditPack builds the document. It writes nothing and decides
// nothing: every verdict in it was decided somewhere else and is being
// reported.
func (s *Server) assembleAuditPack(
	ctx context.Context, req *http.Request, in assembly,
) (auditPack, error) {
	name := in.project.Name

	graph, err := s.readGraph(ctx)
	if err != nil {
		return auditPack{}, err
	}

	releases := &kitchenv1alpha1.ReleaseList{}
	if err := s.Client.List(ctx, releases, client.InNamespace(s.Namespace)); err != nil {
		return auditPack{}, err
	}
	builds := &kitchenv1alpha1.BuildList{}
	if err := s.Client.List(ctx, builds, client.InNamespace(s.Namespace)); err != nil {
		return auditPack{}, err
	}
	promotions := &kitchenv1alpha1.PromotionList{}
	if err := s.Client.List(ctx, promotions, client.InNamespace(s.Namespace)); err != nil {
		return auditPack{}, err
	}
	exceptions := &kitchenv1alpha1.ExceptionList{}
	if err := s.Client.List(ctx, exceptions, client.InNamespace(s.Namespace)); err != nil {
		return auditPack{}, err
	}
	reviews := &kitchenv1alpha1.AccessReviewList{}
	if err := s.Client.List(ctx, reviews, client.InNamespace(s.Namespace)); err != nil {
		return auditPack{}, err
	}

	pack := auditPack{
		Schema:  AuditPackSchema,
		Project: name,
		Range: auditPackRange{
			From:     in.from.Format(time.RFC3339),
			To:       in.to.Format(time.RFC3339),
			HalfOpen: "from is included, to is not: [from, to)",
		},
		Platform:        packPlatform(in.kitchen, s.Version),
		Reproducibility: packReproducibility(),
		Retention:       packRetention(in.kitchen, in.from),
		Verification:    s.packVerification(in),
	}

	environments := graph.environmentsOf(name)
	pack.Inventory = s.packInventory(graph, in, environments)
	pack.Access = packAccess(in, reviews.Items)

	// The promotions come first among the range-bound sections, because the
	// set of releases the rest of the document is about is partly defined by
	// them: a rollback to a release cut last year is a change inside this
	// window, and a change log that only listed releases *created* in the
	// window would not carry it.
	pack.Promotions = packPromotions(name, promotions.Items, in.from, in.to)

	inScope := releasesInScope(name, releases.Items, environments, pack.Promotions, in.from, in.to)
	pack.Inventory.Releases = inScope.views
	pack.Inventory.Scope = auditPackInventoryScope{
		Releases: "the releases this window is about: cut inside it, promoted inside it, " +
			"or still deployed on one of this project's environments. Not every release " +
			"the project has ever cut",
		Depth: mapDepth,
	}

	buildsByName := map[string]*kitchenv1alpha1.Build{}
	for i := range builds.Items {
		buildsByName[builds.Items[i].Name] = &builds.Items[i]
	}
	pack.ChangeLog = packChangeLog(inScope, buildsByName, environments, in.from, in.to)

	decisions, truncated, err := packDecisions(ctx, in)
	if err != nil {
		return auditPack{}, err
	}
	pack.Decisions = decisions
	pack.Decisions.Truncated = truncated

	pack.Attestations = packAttestations(inScope, buildsByName, environments, decisions.Items)
	pack.Exceptions = packExceptions(name, exceptions.Items, in.from, in.to)

	drift, err := s.packDrift(req, in.store, environments, decisions.Items)
	if err != nil {
		return auditPack{}, err
	}
	pack.Drift = drift

	log, err := s.packAuditLog(ctx, in)
	if err != nil {
		return auditPack{}, err
	}
	pack.AuditLog = log

	records, err := packSignedRecords(ctx, in, pack.Access.Cycles)
	if err != nil {
		return auditPack{}, err
	}
	pack.SignedRecords = records

	return pack, nil
}

// packPlatform is who produced the document and under what conditions.
func packPlatform(kitchen *kitchenv1alpha1.Kitchen, version string) auditPackPlatform {
	platform := auditPackPlatform{
		Name:        "kitchen.bermos.dev",
		Version:     version,
		ClusterName: kitchen.Spec.ClusterName,
		BaseDomain:  kitchen.Spec.BaseDomain,
	}
	if status := kitchen.Status.Compliance; status != nil {
		if status.Audit != nil {
			platform.AuditRecording = status.Audit.Recording
			platform.AuditImmutable = status.Audit.Immutable
			platform.ImmutabilityMessage = status.Audit.ImmutabilityMessage
			platform.AuditMessage = status.Audit.Message
		}
		if status.Policy != nil {
			platform.DecisionsStored = status.Policy.Storing
			platform.DecisionsMessage = status.Policy.Message
		}
		if status.Rescan != nil {
			platform.Rescanning = status.Rescan.Running
			platform.RescanMessage = status.Rescan.Message
		}
	}
	if !platform.Rescanning && platform.RescanMessage == "" && !kitchen.Spec.Compliance.Rescan.Enabled {
		platform.RescanMessage = "continuous re-evaluation is off, so nothing in the drift section has " +
			"been re-checked since it was promoted"
	}
	if sync := kitchen.Status.ClockSync; sync != nil {
		clock := &auditPackClock{
			Method:           sync.Method,
			Nodes:            sync.Nodes,
			Drifted:          sync.Drifted,
			MaxDriftSeconds:  sync.MaxDriftSeconds,
			WorstNode:        sync.WorstNode,
			WorstDriftMillis: sync.WorstDriftMillis,
			Message:          sync.Message,
		}
		if sync.Checked != nil {
			checked := sync.Checked.Time.UTC()
			clock.Checked = &checked
		}
		platform.ClockSync = clock
	}
	return platform
}

// packReproducibility is a constant, and that is the point: it is the same
// sentence in every pack, so two packs can be compared without a reader
// having to work out what each one meant by "reproducible".
func packReproducibility() auditPackReproducibility {
	return auditPackReproducibility{
		Claim: "Every byte of this document is determined by the range and by the evidence the " +
			"platform holds. Nothing in it is read off a clock, no list is in the order a store " +
			"answered in, and every phase that would otherwise be judged \"now\" is judged at the " +
			"end of the range. Two exports of the same range are the same bytes unless the " +
			"evidence itself changed. The two lists below say which sections the window alone " +
			"decides, and which also read the estate as it stands — a change log entry's content " +
			"is entirely historical, for instance, but a release running since before the window " +
			"is in the document because it was running during it.",
		RangeBound: []string{
			"promotions", "decisions", "exceptions", "drift.history",
			"auditLog", "access.cycles",
		},
		CurrentState: []string{
			"inventory", "changeLog", "attestations", "access.grants",
			"drift.current", "signedRecords", "platform", "retention",
		},
		Excluded: []string{
			"The signature is not part of these bytes. It is a DSSE envelope served at " +
				"?format=dsse whose subject is this document's sha256, and it carries the export's " +
				"own timestamp — an ECDSA signature has a nonce, so two signings of identical bytes " +
				"are two different envelopes and neither is the document.",
			"The HTML rendering at ?format=html is derived from this document and is not signed. " +
				"It carries the digest so a printout can be tied back to the bytes.",
		},
	}
}

// packRetention answers whether the store can still speak to the whole range.
func packRetention(kitchen *kitchenv1alpha1.Kitchen, from time.Time) auditPackRetention {
	model := retention.Resolve(kitchen)
	view := auditPackRetention{
		AuditDays:  model.Days(retention.ClassAudit),
		FloorDays:  retention.AuditFloorDays,
		Overridden: model.AuditBelowFloor(),
		Note: "Only the audit log and the decision register are retention-bounded here — they " +
			"share one class, because the decisions follow the audit knob. The signed records " +
			"are kept under no TTL at all. Everything in the inventory is a live object in the " +
			"cluster and has no history to expire: an environment deleted last March is not in " +
			"this document, and never was.",
	}
	if override := kitchen.Spec.Retention.AuditFloorOverride; override != nil {
		view.OverrideReason = override.Reason
		view.OverrideApprovedBy = override.ApprovedBy
	}

	status := kitchen.Status.Retention
	if status == nil {
		view.Message = "no retention sweep has run on this platform, so how far back the audit log " +
			"actually goes has not been measured — the number above is what is configured, not " +
			"what is there"
		return view
	}
	if status.LastSweep != nil {
		swept := status.LastSweep.Time.UTC()
		view.LastSweep = &swept
	}
	for _, entry := range status.Classes {
		if entry.Class != string(retention.ClassAudit) || entry.Oldest == nil {
			continue
		}
		oldest := entry.Oldest.Time.UTC()
		view.Oldest = &oldest
		if oldest.After(from) {
			view.Truncated = true
			view.CoveredFrom = oldest.Format(time.RFC3339)
			view.Message = fmt.Sprintf(
				"this pack was asked for %s onwards, and the oldest audit record the store still "+
					"holds is from %s: retention has already removed part of the window. The "+
					"sections drawn from the log and the decision register answer from %s, not "+
					"from the date at the top of this document",
				from.Format(time.RFC3339), oldest.Format(time.RFC3339), oldest.Format(time.RFC3339))
			return view
		}
	}
	if view.Oldest == nil {
		view.Message = "the last sweep measured no audit records at all, so there is nothing to " +
			"say about how far back the log goes"
		return view
	}
	view.CoveredFrom = from.Format(time.RFC3339)
	view.Message = "the whole of the requested window is inside what the store still holds"
	return view
}

// packVerification is the procedure, written out so it travels with the file.
func (s *Server) packVerification(in assembly) auditPackVerification {
	view := auditPackVerification{
		PredicateType: attestation.PredicateAuditPack,
		PayloadType:   attestation.PayloadType,
		Procedure: []string{
			"1. Save the two documents beside each other: the pack (?format=json) as pack.json, " +
				"and the signature (?format=dsse) as pack.dsse.json.",
			"2. Check the digest: `sha256sum pack.json` — it must match the `sha256` entry under " +
				"`subject[0].digest` in the statement, which step 3 decodes.",
			"3. Decode the signed statement: " +
				"`jq -r .payload pack.dsse.json | base64 -d > statement.json`",
			"4. Rebuild DSSE's pre-authentication encoding and check the signature against the " +
				"public key: " +
				"`printf 'DSSEv1 28 application/vnd.in-toto+json %d ' \"$(wc -c < statement.json)\" > pae.bin " +
				"&& cat statement.json >> pae.bin " +
				"&& jq -r '.signatures[0].sig' pack.dsse.json | base64 -d > sig.bin " +
				"&& openssl dgst -sha256 -verify public.pem -signature sig.bin pae.bin`",
		},
		Warning: "The public key below is here so the pack reads as one document. It is not where " +
			"trust comes from: a key taken out of the same file as the signature proves only that " +
			"the file is internally consistent. Verify against the key your institution kept when " +
			"the platform was installed, or against GET /api/v1/compliance on a platform you " +
			"trust.",
	}
	switch {
	case in.keyErr != nil:
		view.Message = "this pack is unsigned: the platform's signing key could not be read, so " +
			"the document stands on the audit log and on nothing portable"
	case in.key == nil:
		view.Message = "this pack is unsigned: attestation is switched off on this platform, so " +
			"there is no key to sign it with. The evidence inside it is unchanged; what is " +
			"missing is the means to check it somewhere else"
	default:
		view.Signed = true
		view.KeyID = in.key.KeyID()
		if pem, err := in.key.PublicPEM(); err == nil {
			view.PublicKey = string(pem)
		}
	}
	return view
}

// packInventory is the estate, current. It reuses the criticality map's graph
// rather than walking the objects again, so the pack and GET
// /compliance/criticality cannot come to disagree about what stands behind a
// project.
func (s *Server) packInventory(
	graph *complianceGraph, in assembly, environments []*kitchenv1alpha1.Environment,
) auditPackInventory {
	project := in.project
	defaultResidency := in.kitchen.Spec.Residency

	inventory := auditPackInventory{
		Project: auditPackProject{
			Name:               project.Name,
			CreatedAt:          project.CreationTimestamp.Time.UTC(),
			DataClass:          orWord(string(project.Spec.DataClass), inventoryUnclassified),
			Criticality:        orWord(string(project.Spec.Criticality), criticalityUndesignated),
			RTO:                string(project.Spec.RTO),
			RPO:                string(project.Spec.RPO),
			Repository:         project.Spec.Source.Repo,
			Branch:             project.Spec.Source.ProductionBranch,
			RequirePullRequest: project.Spec.Source.RequirePullRequest,
			SourceConnection:   project.Spec.Source.ConnectionRef.Name,
			RegistryConnection: project.Spec.Registry.ConnectionRef.Name,
		},
		Environments: []auditPackEnvironment{},
		Releases:     []auditPackRelease{},
		Claims:       []auditPackClaim{},
		Connections:  []auditPackConnection{},
		Domains:      []auditPackDomain{},
		ThirdParties: []string{},
	}

	for _, env := range environments {
		continuity := kitchenv1alpha1.EffectiveContinuity(project, env)
		residency := env.Spec.Residency
		if residency == "" {
			residency = defaultResidency
		}
		release := env.Status.ObservedRelease
		if release == "" {
			release = env.Spec.ReleaseRef.Name
		}
		row := auditPackEnvironment{
			Name:        env.Name,
			Type:        string(env.Spec.Type),
			DataClass:   orWord(string(env.Spec.DataClass), inventoryUnclassified),
			Residency:   orWord(residency, inventoryUnknown),
			Criticality: orWord(string(continuity.Criticality), criticalityUndesignated),
			RTO:         string(continuity.RTO),
			RPO:         string(continuity.RPO),
			Inherited:   continuity.Inherited,
			URL:         env.Status.URL,
			Phase:       string(env.Status.Phase),
			Release:     release,
			Image:       graph.images[release],
			Owners:      append([]string{}, env.Spec.Owners...),
			Domains:     graph.domainsOf(env.Name),
			CreatedAt:   env.CreationTimestamp.Time.UTC(),
		}
		sort.Strings(row.Owners)
		if requirements := env.Spec.Requirements; requirements != nil {
			row.BundleDigest = requirements.BundleDigest
			row.Parameters = requirements.Parameters
		}
		inventory.Environments = append(inventory.Environments, row)
	}
	sort.Slice(inventory.Environments, func(i, j int) bool {
		return inventory.Environments[i].Name < inventory.Environments[j].Name
	})

	providers := map[string]struct{}{}
	for _, claim := range graph.claimsOf(project.Name) {
		row := auditPackClaim{
			Name:       claim.Name,
			Type:       claim.Spec.Type,
			Phase:      string(claim.Status.Phase),
			DataClass:  orWord(string(claim.Spec.DataClass), inventoryUnclassified),
			Provenance: orWord(claimProvenance(claim), inventoryUndeclared),
			Residency:  orWord(claim.Status.Residency, inventoryUnknown),
			CreatedAt:  claim.CreationTimestamp.Time.UTC(),
		}
		if ref := claim.Spec.ConnectionRef; ref != nil {
			row.Connection = ref.Name
			if conn, ok := graph.connections[ref.Name]; ok {
				row.Provider = conn.Spec.Provider
			}
		} else {
			row.Provider = "platform identity provider"
		}
		if row.Provider != "" {
			providers[row.Provider] = struct{}{}
		}
		inventory.Claims = append(inventory.Claims, row)
	}

	for name, reasons := range graph.connectionUses(project) {
		row := auditPackConnection{
			Name:    name,
			UsedFor: reasons,
			Credential: "held by the platform, never in this document — the API does not read a " +
				"credential back and an export is not an exception",
		}
		if conn, ok := graph.connections[name]; ok {
			row.Provider = conn.Spec.Provider
			providers[conn.Spec.Provider] = struct{}{}
			for _, capability := range conn.Status.Capabilities {
				row.Capabilities = append(row.Capabilities, string(capability))
			}
			sort.Strings(row.Capabilities)
		}
		inventory.Connections = append(inventory.Connections, row)
	}
	sort.Slice(inventory.Connections, func(i, j int) bool {
		return inventory.Connections[i].Name < inventory.Connections[j].Name
	})

	owned := map[string]struct{}{}
	for _, env := range environments {
		owned[env.Name] = struct{}{}
	}
	for i := range graph.domains {
		domain := &graph.domains[i]
		if _, ours := owned[domain.Spec.EnvironmentRef.Name]; !ours {
			continue
		}
		inventory.Domains = append(inventory.Domains, auditPackDomain{
			Hostname:    domain.Spec.Hostname,
			Environment: domain.Spec.EnvironmentRef.Name,
			Verified:    domain.Status.Verified,
			TLSMode:     string(domain.Status.TLSMode),
			CreatedAt:   domain.CreationTimestamp.Time.UTC(),
		})
	}
	sort.Slice(inventory.Domains, func(i, j int) bool {
		return inventory.Domains[i].Hostname < inventory.Domains[j].Hostname
	})

	for name := range providers {
		// A provider that is this platform is not a third party — see
		// provider.ThirdParty.
		if provider.ThirdParty(name) {
			inventory.ThirdParties = append(inventory.ThirdParties, name)
		}
	}
	sort.Strings(inventory.ThirdParties)
	return inventory
}

// packAccess is who holds what on the project, and every recertification
// cycle that has looked at it.
func packAccess(in assembly, reviews []kitchenv1alpha1.AccessReview) auditPackAccess {
	access := auditPackAccess{
		Grants: []auditPackGrant{},
		Cycles: []auditPackReview{},
		Note: "The grants are this project's own, from spec.access — the platform's operators " +
			"hold admin on every project and are not listed here. The cycles are the ones whose " +
			"scope covers this project; each cycle's whole artefact is in signedRecords, and it " +
			"covers grants on other projects too, which is why the entries beside it are only " +
			"the ones naming this project or the platform role.",
	}
	for _, grant := range in.project.Spec.Access {
		access.Grants = append(access.Grants, auditPackGrant{
			Subject: grant.Subject,
			Email:   grant.Email,
			Role:    string(grant.Role),
		})
	}
	sort.Slice(access.Grants, func(i, j int) bool {
		if access.Grants[i].Subject != access.Grants[j].Subject {
			return access.Grants[i].Subject < access.Grants[j].Subject
		}
		return access.Grants[i].Role < access.Grants[j].Role
	})

	for i := range reviews {
		review := &reviews[i]
		if !reviewCovers(review, in.project.Name) || !reviewOverlaps(review, in.from, in.to) {
			continue
		}
		access.Cycles = append(access.Cycles, packReview(review, in.project.Name, in.to))
	}
	// Newest first, then by name: the cycle that last looked at this project
	// is the one an examiner reads.
	sort.Slice(access.Cycles, func(i, j int) bool {
		if !access.Cycles[i].DueBy.Equal(access.Cycles[j].DueBy) {
			return access.Cycles[i].DueBy.After(access.Cycles[j].DueBy)
		}
		return access.Cycles[i].Name < access.Cycles[j].Name
	})
	return access
}

// reviewCovers reports whether a cycle's scope includes this project.
func reviewCovers(review *kitchenv1alpha1.AccessReview, project string) bool {
	switch review.Spec.Scope {
	case kitchenv1alpha1.AccessReviewProject:
		return review.Spec.ProjectRef != nil && review.Spec.ProjectRef.Name == project
	case kitchenv1alpha1.AccessReviewPlatform:
		// A platform cycle reviews the operator list, and an operator holds
		// admin on this project. It covers it.
		return true
	default:
		return true
	}
}

// reviewOverlaps reports whether a cycle had anything to do with the window:
// it was opened before the window ended, and it was still open when the
// window began — or it closed inside it.
func reviewOverlaps(review *kitchenv1alpha1.AccessReview, from, to time.Time) bool {
	opened := review.CreationTimestamp.Time
	if at := review.Status.OpenedAt; at != nil {
		opened = at.Time
	}
	if !opened.Before(to) {
		return false
	}
	if at := review.Status.ClosedAt; at != nil {
		return !at.Time.Before(from)
	}
	return true
}

// packReview renders one cycle, judged at the range's end.
func packReview(
	review *kitchenv1alpha1.AccessReview, project string, at time.Time,
) auditPackReview {
	view := auditPackReview{
		Name:         review.Name,
		Scope:        string(review.Spec.Scope),
		OpenedBy:     review.Spec.OpenedBy,
		ClosedBy:     review.Status.ClosedBy,
		Reason:       review.Spec.Reason,
		Reviewers:    []string{},
		DueBy:        review.Spec.DueBy.Time.UTC(),
		Phase:        string(review.EffectivePhase(at)),
		Pending:      review.Status.Pending,
		Confirmed:    review.Status.Confirmed,
		Revoked:      review.Status.Revoked,
		SelfReviewed: review.Status.SelfReviewed,
		Orphaned:     review.Status.Orphaned,
		Entries:      []auditPackReviewEntry{},
		EntriesTotal: int32(len(review.Status.Entries)),
	}
	if review.Spec.ProjectRef != nil {
		view.Project = review.Spec.ProjectRef.Name
	}
	for _, reviewer := range review.Spec.Reviewers {
		name := reviewer.Email
		if name == "" {
			name = reviewer.Subject
		}
		view.Reviewers = append(view.Reviewers, name)
	}
	sort.Strings(view.Reviewers)
	for _, stamp := range []struct {
		from *metav1.Time
		into **time.Time
	}{
		{review.Status.OpenedAt, &view.OpenedAt},
		{review.Status.SnapshotAt, &view.SnapshotAt},
		{review.Status.ClosedAt, &view.ClosedAt},
	} {
		if stamp.from != nil {
			moment := stamp.from.Time.UTC()
			*stamp.into = &moment
		}
	}

	omitted := 0
	for i := range review.Status.Entries {
		entry := &review.Status.Entries[i]
		// A platform grant is a grant on this project — an operator holds
		// admin everywhere — so both belong here and nothing else does.
		if entry.Grant != project && entry.Grant != access.PlatformGrant {
			omitted++
			continue
		}
		row := auditPackReviewEntry{
			Subject:    entry.Subject,
			Email:      entry.Email,
			Grant:      entry.Grant,
			Role:       entry.Role,
			Decision:   string(entry.Decision),
			DecidedBy:  entry.DecidedBy,
			Note:       entry.Note,
			SelfReview: entry.SelfReview,
			Inactive:   entry.Inactive,
			Orphaned:   entry.Orphaned,
			Applied:    entry.Applied,
		}
		if row.Decision == "" {
			// An undecided grant is part of the record, not an omission from
			// it: "nobody looked at this one" is what an examiner is reading
			// the artefact for.
			row.Decision = "undecided"
		}
		if at := entry.DecidedAt; at != nil {
			decided := at.Time.UTC()
			row.DecidedAt = &decided
		}
		view.Entries = append(view.Entries, row)
	}
	sort.Slice(view.Entries, func(i, j int) bool {
		if view.Entries[i].Grant != view.Entries[j].Grant {
			return view.Entries[i].Grant < view.Entries[j].Grant
		}
		return view.Entries[i].Subject < view.Entries[j].Subject
	})
	if omitted > 0 {
		view.EntriesNote = fmt.Sprintf(
			"%d further decisions in this cycle are about grants on other projects. They are in "+
				"the cycle's signed artefact, which is carried whole in signedRecords", omitted)
	}

	if artifact := review.Status.Artifact; artifact != nil {
		view.RecordID = artifact.RecordID
		view.Subject = artifact.Subject
		view.PredicateType = artifact.PredicateType
		view.ArtifactNote = artifact.Message
		if at := artifact.SignedAt; at != nil {
			signed := at.Time.UTC()
			view.SignedAt = &signed
		}
	} else if view.Phase == string(kitchenv1alpha1.AccessReviewClosed) {
		view.ArtifactNote = "this cycle closed without a retained artefact"
	}
	return view
}

// releasesInScope is which releases this window is about, and the index the
// change log and the attestation section are both built from.
type packReleases struct {
	views []auditPackRelease
	// objects is the same set, by name, so the two sections that need the
	// object rather than the row do not list again.
	objects map[string]*kitchenv1alpha1.Release
	// order is the names in the views' order, which is the total order both
	// derived sections inherit.
	order []string
}

func releasesInScope(
	project string,
	releases []kitchenv1alpha1.Release,
	environments []*kitchenv1alpha1.Environment,
	promotions []auditPackPromotion,
	from, to time.Time,
) packReleases {
	wanted := map[string]struct{}{}
	for _, promotion := range promotions {
		wanted[promotion.Release] = struct{}{}
	}
	for _, env := range environments {
		if name := env.Spec.ReleaseRef.Name; name != "" {
			wanted[name] = struct{}{}
		}
		if name := env.Status.ObservedRelease; name != "" {
			wanted[name] = struct{}{}
		}
		for _, entry := range env.Status.History {
			// A release that stopped being current inside the window was
			// running inside the window, which is what the change log is
			// about.
			if entry.To.Time.Before(from) || !entry.From.Time.Before(to) {
				continue
			}
			wanted[entry.Release] = struct{}{}
		}
	}

	scope := packReleases{objects: map[string]*kitchenv1alpha1.Release{}}
	for i := range releases {
		release := &releases[i]
		if release.Spec.ProjectRef.Name != project {
			continue
		}
		created := release.CreationTimestamp.Time
		inRange := !created.Before(from) && created.Before(to)
		if _, asked := wanted[release.Name]; !inRange && !asked {
			continue
		}
		scope.objects[release.Name] = release
		scope.views = append(scope.views, auditPackRelease{
			Name:      release.Name,
			Build:     release.Spec.BuildRef.Name,
			Image:     release.Spec.Image,
			CreatedAt: created.UTC(),
			InRange:   inRange,
		})
	}
	// Oldest first, then by name: a change log reads forwards, and the name
	// is the tie-break that makes two releases cut in the same second order
	// the same way twice.
	sort.Slice(scope.views, func(i, j int) bool {
		if !scope.views[i].CreatedAt.Equal(scope.views[j].CreatedAt) {
			return scope.views[i].CreatedAt.Before(scope.views[j].CreatedAt)
		}
		return scope.views[i].Name < scope.views[j].Name
	})
	for _, view := range scope.views {
		scope.order = append(scope.order, view.Name)
	}
	return scope
}

// packChangeLog is the change history: one entry per release in scope, with
// who wrote the commit, who approved it, and where it landed.
func packChangeLog(
	scope packReleases,
	builds map[string]*kitchenv1alpha1.Build,
	environments []*kitchenv1alpha1.Environment,
	from, to time.Time,
) []auditPackChange {
	log := []auditPackChange{}
	for _, name := range scope.order {
		release := scope.objects[name]
		change := auditPackChange{
			Release:   release.Name,
			Build:     release.Spec.BuildRef.Name,
			CreatedAt: release.CreationTimestamp.Time.UTC(),
			Image:     release.Spec.Image,
		}
		build := builds[release.Spec.BuildRef.Name]
		if build == nil {
			change.ReviewNote = "the build this release came from is no longer in the cluster, so " +
				"neither the commit nor its review can be read back"
			log = append(log, change)
			continue
		}
		change.Commit = build.Spec.Git.SHA
		change.Branch = build.Spec.Git.Branch
		change.Message = build.Spec.Git.Message
		change.Author = build.Spec.Git.Author
		if artifact := build.Status.Artifact; artifact != nil {
			change.Digest = artifact.Digest
		}
		change.Review, change.ReviewNote = packReviewProvenance(build)
		change.Deployments = deploymentsOf(release.Name, environments, from, to)
		log = append(log, change)
	}
	return log
}

// packReviewProvenance is §8 for one build, and the sentence that stands in
// for it when there is nothing.
func packReviewProvenance(build *kitchenv1alpha1.Build) (*auditPackReviewProvenance, string) {
	source := build.Status.Source
	if source == nil {
		return nil, "the git provider was never asked about this commit — either the project does " +
			"not require a reviewed pull request, or the build predates the check"
	}
	view := &auditPackReviewProvenance{
		Provider:        source.Provider,
		PullRequest:     source.PullRequest,
		Title:           source.Title,
		Author:          source.Author,
		MergedBy:        source.MergedBy,
		Approvers:       append([]string{}, source.Approvers...),
		SelfApproved:    source.SelfApproved,
		Independent:     source.Independent,
		Required:        source.Required,
		MachineIdentity: source.MachineIdentity,
		Exception:       source.Exception,
		Message:         source.Message,
	}
	sort.Strings(view.Approvers)
	if view.Approvers == nil {
		view.Approvers = []string{}
	}
	if at := source.CheckedAt; at != nil {
		checked := at.Time.UTC()
		view.CheckedAt = &checked
	}
	return view, ""
}

// deploymentsOf is where a release ran, out of the environments' own history
// plus wherever it is current.
func deploymentsOf(
	release string, environments []*kitchenv1alpha1.Environment, from, to time.Time,
) []auditPackDeployment {
	deployments := []auditPackDeployment{}
	for _, env := range environments {
		for _, entry := range env.Status.History {
			if entry.Release != release {
				continue
			}
			if entry.To.Time.Before(from) || !entry.From.Time.Before(to) {
				continue
			}
			finished := entry.To.Time.UTC()
			deployments = append(deployments, auditPackDeployment{
				Environment: env.Name,
				From:        entry.From.Time.UTC(),
				To:          &finished,
				Reason:      string(entry.Reason),
				By:          entry.By,
			})
		}
		current := env.Status.ObservedRelease
		if current == "" {
			current = env.Spec.ReleaseRef.Name
		}
		if current == release {
			deployments = append(deployments, auditPackDeployment{
				Environment: env.Name,
				From:        env.CreationTimestamp.Time.UTC(),
				Current:     true,
			})
		}
	}
	sort.Slice(deployments, func(i, j int) bool {
		if deployments[i].Environment != deployments[j].Environment {
			return deployments[i].Environment < deployments[j].Environment
		}
		return deployments[i].From.Before(deployments[j].From)
	})
	return deployments
}

// packPromotions is every request for a release to land that was made inside
// the window, with the verdict that answered it.
func packPromotions(
	project string, promotions []kitchenv1alpha1.Promotion, from, to time.Time,
) []auditPackPromotion {
	views := []auditPackPromotion{}
	for i := range promotions {
		promotion := &promotions[i]
		if promotion.Spec.ProjectRef.Name != project {
			continue
		}
		created := promotion.CreationTimestamp.Time
		if created.Before(from) || !created.Before(to) {
			continue
		}
		view := auditPackPromotion{
			Name:        promotion.Name,
			Environment: promotion.Spec.EnvironmentRef.Name,
			Release:     promotion.Spec.ReleaseRef.Name,
			RequestedBy: promotion.Spec.RequestedBy,
			Trigger:     string(promotion.Spec.Trigger),
			Reason:      promotion.Spec.Reason,
			CreatedAt:   created.UTC(),
			Phase:       string(promotion.Status.Phase),
			Verdict:     promotion.Status.Verdict,
			UnmetRules:  promotion.Status.UnmetRules,
			DecisionID:  promotion.Status.DecisionID,
			Message:     promotion.Status.Message,
		}
		if at := promotion.Status.AppliedAt; at != nil {
			applied := at.Time.UTC()
			view.AppliedAt = &applied
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.Before(views[j].CreatedAt)
		}
		return views[i].Name < views[j].Name
	})
	return views
}

// packDecisions reads the register's slice of the window, whole.
func packDecisions(ctx context.Context, in assembly) (auditPackDecisions, bool, error) {
	stored, err := in.store.QueryDecisions(ctx, clickhouse.DecisionQuery{
		Project: in.project.Name,
		Since:   in.from,
		Until:   in.to,
		Limit:   maxAuditPackDecisions,
	})
	if err != nil {
		return auditPackDecisions{}, false, fmt.Errorf("the decision register could not be read: %w", err)
	}

	section := auditPackDecisions{
		Items: []auditPackDecision{},
		Limit: maxAuditPackDecisions,
		Note: "Every decision the policy engine stored about this project inside the window, with " +
			"the two digests and the full canonical input that make it reproducible. `POST " +
			"/api/v1/decisions/{id}/replay` re-runs one against the bundle its digest names and " +
			"compares the verdicts; the bundle bytes are kept beside the decision, so a replay " +
			"does not depend on a ConfigMap that may since have been edited.",
	}
	for _, decision := range stored {
		section.Items = append(section.Items, auditPackDecision{
			ID:           decision.ID,
			Timestamp:    decision.Timestamp.UTC(),
			Kind:         decision.Kind,
			Environment:  decision.Environment,
			Release:      decision.Release,
			Artifact:     decision.Artifact,
			BundleDigest: decision.BundleDigest,
			InputDigest:  decision.InputDigest,
			DataSnapshot: decision.DataSnapshot,
			Verdict:      decision.Verdict,
			DecidedBy:    decision.DecidedBy,
			RulesFired:   rawIfValid(decision.RulesFired),
			Input:        rawIfValid(decision.Input),
		})
	}
	// The store answers newest first; a document reads forwards.
	sort.Slice(section.Items, func(i, j int) bool {
		if !section.Items[i].Timestamp.Equal(section.Items[j].Timestamp) {
			return section.Items[i].Timestamp.Before(section.Items[j].Timestamp)
		}
		return section.Items[i].ID < section.Items[j].ID
	})

	truncated := len(stored) >= maxAuditPackDecisions
	if truncated {
		section.Message = fmt.Sprintf(
			"this window holds at least %d decisions, which is the most one read returns: the "+
				"newest %d are here and older ones inside the same window are not. Narrow the "+
				"range and take two packs",
			maxAuditPackDecisions, maxAuditPackDecisions)
	}
	return section, truncated, nil
}

// packAttestations indexes what is attached to each artifact in scope.
func packAttestations(
	scope packReleases,
	builds map[string]*kitchenv1alpha1.Build,
	environments []*kitchenv1alpha1.Environment,
	decisions []auditPackDecision,
) []auditPackArtifact {
	running := map[string][]string{}
	for _, env := range environments {
		current := env.Status.ObservedRelease
		if current == "" {
			current = env.Spec.ReleaseRef.Name
		}
		if current != "" {
			running[current] = append(running[current], env.Name)
		}
	}
	for release := range running {
		sort.Strings(running[release])
	}
	scans := newestScans(decisions)

	artifacts := []auditPackArtifact{}
	for _, name := range scope.order {
		release := scope.objects[name]
		view := auditPackArtifact{
			Release:      release.Name,
			Build:        release.Spec.BuildRef.Name,
			Image:        release.Spec.Image,
			Evidence:     []auditPackEvidence{},
			Environments: running[release.Name],
		}
		if scan, found := scans[release.Name]; found {
			view.NewestScan = &scan
		}
		build := builds[release.Spec.BuildRef.Name]
		if build == nil {
			view.Message = "the build this release came from is no longer in the cluster, so what " +
				"was attached to its artifact cannot be indexed from here — the attestations " +
				"themselves are still in the registry, against the digest in `image`"
			artifacts = append(artifacts, view)
			continue
		}
		artifact := build.Status.Artifact
		if artifact == nil || artifact.Digest == "" {
			view.Message = "this build produced no artifact digest, so there is nothing evidence " +
				"could have been attached to"
			artifacts = append(artifacts, view)
			continue
		}
		view.Repository = artifact.Repository
		view.Digest = artifact.Digest
		view.KeyID = artifact.KeyID
		view.Message = artifact.Message
		if at := artifact.AttestedAt; at != nil {
			attested := at.Time.UTC()
			view.AttestedAt = &attested
		}
		for _, evidence := range artifact.Evidence {
			view.Evidence = append(view.Evidence, auditPackEvidence{
				PredicateType: evidence.PredicateType,
				Manifest:      evidence.Manifest,
				Source:        evidence.Source,
			})
		}
		sort.Slice(view.Evidence, func(i, j int) bool {
			if view.Evidence[i].PredicateType != view.Evidence[j].PredicateType {
				return view.Evidence[i].PredicateType < view.Evidence[j].PredicateType
			}
			return view.Evidence[i].Manifest < view.Evidence[j].Manifest
		})
		for _, gate := range build.Status.Gates {
			row := auditPackGate{
				Name:          gate.Name,
				Phase:         string(gate.Phase),
				Source:        gate.Source,
				ReportedBy:    gate.ReportedBy,
				PredicateType: gate.PredicateType,
				Message:       gate.Message,
			}
			if at := gate.Attested; at != nil {
				attested := at.Time.UTC()
				row.Attested = &attested
			}
			if at := gate.FinishedAt; at != nil {
				finished := at.Time.UTC()
				row.FinishedAt = &finished
			}
			view.Gates = append(view.Gates, row)
		}
		sort.Slice(view.Gates, func(i, j int) bool { return view.Gates[i].Name < view.Gates[j].Name })
		for _, statement := range build.Status.VEX {
			row := auditPackVEX{
				Author:      statement.Author,
				SubmittedBy: statement.SubmittedBy,
				Statements:  statement.Statements,
				Digest:      statement.Manifest,
			}
			if at := statement.IngestedAt; at != nil {
				ingested := at.Time.UTC()
				row.SubmittedAt = &ingested
			}
			view.VEX = append(view.VEX, row)
		}
		sort.Slice(view.VEX, func(i, j int) bool { return view.VEX[i].Digest < view.VEX[j].Digest })
		if view.Repository != "" {
			view.Fetch = fmt.Sprintf("cosign verify-attestation --key public.pem %s@%s",
				view.Repository, view.Digest)
		}
		artifacts = append(artifacts, view)
	}
	return artifacts
}

// newestScans is the pack saying what the policy says: an artifact is judged
// on its newest scan (policy.NewestVulnerabilityScan), so this reports the
// newest re-evaluation of each release rather than a list a reader would have
// to order for themselves. The tie-break is the decision id, exactly as the
// engine's is the attestation digest — two scans stamped the same second must
// not be able to answer differently on two reads.
func newestScans(decisions []auditPackDecision) map[string]auditPackScan {
	newest := map[string]auditPackScan{}
	for _, decision := range decisions {
		if decision.Kind != policy.KindRescan || decision.Release == "" {
			continue
		}
		candidate := auditPackScan{
			DecisionID:   decision.ID,
			ScannedAt:    decision.Timestamp,
			DataSnapshot: decision.DataSnapshot,
			Verdict:      decision.Verdict,
			Environment:  decision.Environment,
		}
		standing, found := newest[decision.Release]
		if !found ||
			candidate.ScannedAt.After(standing.ScannedAt) ||
			(candidate.ScannedAt.Equal(standing.ScannedAt) && candidate.DecisionID > standing.DecisionID) {
			newest[decision.Release] = candidate
		}
	}
	return newest
}

// packExceptions is every break-glass grant whose life overlapped the window.
func packExceptions(
	project string, exceptions []kitchenv1alpha1.Exception, from, to time.Time,
) []auditPackException {
	views := []auditPackException{}
	for i := range exceptions {
		exception := &exceptions[i]
		if exception.Spec.ProjectRef.Name != project {
			continue
		}
		created := exception.CreationTimestamp.Time
		if !created.Before(to) {
			continue
		}
		// The grant's life ends when it was resolved, or when it expired.
		// One that was still standing at the start of the window is in the
		// window whether or not anything happened to it inside.
		ended := exception.Spec.ExpiresAt.Time
		if at := exception.Status.ResolvedAt; at != nil && at.Time.Before(ended) {
			ended = at.Time
		}
		if ended.Before(from) {
			continue
		}
		phase := string(exception.EffectivePhase(to))
		view := auditPackException{
			Name:             exception.Name,
			Environment:      exception.Spec.EnvironmentRef.Name,
			RuleIDs:          exception.Spec.RuleIDs,
			Reason:           exception.Spec.Reason,
			RequestedBy:      exception.Spec.RequestedBy,
			ApprovedBy:       exception.Spec.ApprovedBy,
			IncidentRef:      exception.Spec.IncidentRef,
			CreatedAt:        created.UTC(),
			ExpiresAt:        exception.Spec.ExpiresAt.Time.UTC(),
			AutoRollback:     exception.Spec.AutoRollback,
			ResolvedBy:       exception.Status.ResolvedBy,
			Phase:            phase,
			UsedBy:           exception.Status.UsedBy,
			ActiveAtRangeEnd: phase == string(kitchenv1alpha1.ExceptionActive),
		}
		if exception.Spec.ReleaseRef != nil {
			view.Release = exception.Spec.ReleaseRef.Name
		}
		if at := exception.Status.ResolvedAt; at != nil {
			resolved := at.Time.UTC()
			view.ResolvedAt = &resolved
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.Before(views[j].CreatedAt)
		}
		return views[i].Name < views[j].Name
	})
	return views
}

// packDrift is the same derivation GET /compliance/drift makes, restricted to
// this project, plus every re-evaluation stored inside the window.
func (s *Server) packDrift(
	req *http.Request,
	store logReader,
	environments []*kitchenv1alpha1.Environment,
	decisions []auditPackDecision,
) (auditPackDrift, error) {
	drift := auditPackDrift{
		Current: []driftItemView{},
		Counts:  map[string]int{},
		History: []auditPackDriftEvent{},
		Note: "`current` is what the estate looks like now, not at the end of the range: the " +
			"platform reconciles the graph rather than versioning it, and re-evaluating a " +
			"historical state is what `decisions` and their stored inputs are for. `history` is " +
			"every re-evaluation the engine stored inside the range, oldest first.",
	}
	for _, env := range environments {
		if env.Spec.ReleaseRef.Name == "" {
			continue
		}
		item, err := s.driftFor(req, store, env)
		if err != nil {
			return auditPackDrift{}, fmt.Errorf("the drift derivation failed: %w", err)
		}
		drift.Counts[item.Status]++
		drift.Current = append(drift.Current, item)
	}
	sort.Slice(drift.Current, func(i, j int) bool {
		return drift.Current[i].Environment < drift.Current[j].Environment
	})

	for _, decision := range decisions {
		if decision.Kind != policy.KindRescan {
			continue
		}
		event := auditPackDriftEvent{
			DecisionID:   decision.ID,
			Timestamp:    decision.Timestamp,
			Environment:  decision.Environment,
			Release:      decision.Release,
			Verdict:      decision.Verdict,
			DataSnapshot: decision.DataSnapshot,
		}
		for _, rule := range decodeFired(string(decision.RulesFired)) {
			if !rule.Waived {
				event.Unwaived++
				continue
			}
			if rule.Exception != "" {
				event.Waived = append(event.Waived, rule.Exception)
			}
		}
		sort.Strings(event.Waived)
		event.Waived = deduplicate(event.Waived)
		drift.History = append(drift.History, event)
	}
	return drift, nil
}

// deduplicate collapses a sorted list. One grant waiving four rules is one
// grant, and naming it four times would read as four waivers.
func deduplicate(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	unique := sorted[:1]
	for _, value := range sorted[1:] {
		if value != unique[len(unique)-1] {
			unique = append(unique, value)
		}
	}
	return unique
}

// packAuditLog is the tamper-evident log's slice of the window, with the
// chain fields kept.
func (s *Server) packAuditLog(ctx context.Context, in assembly) (auditPackAuditLog, error) {
	records, err := in.store.QueryAuditRecords(ctx, clickhouse.AuditQuery{
		Project: in.project.Name,
		Since:   in.from,
		Until:   in.to,
		Limit:   maxAuditPackAuditRecords,
	})
	if err != nil {
		return auditPackAuditLog{}, fmt.Errorf("the audit log could not be read: %w", err)
	}

	section := auditPackAuditLog{
		Items:  []auditRecordBody{},
		Limit:  maxAuditPackAuditRecords,
		Anchor: s.auditAnchor(ctx),
		Note: "Every record here names this project. Changes to the platform itself — a setting, " +
			"a connection, an upgrade — carry no project and are not in a project's pack; they " +
			"are in the log, at `GET /api/v1/audit`. Each record carries its own hash and the " +
			"hash before it, so a record here can be located in the chain and checked; the " +
			"verification itself is a statement about the whole log and is `GET " +
			"/api/v1/audit/verify`. `anchor` is where the chain ends according to an object " +
			"outside the table, which is the only way a tail cut off the end is visible at all.",
	}
	for _, record := range records {
		body := auditBody(record)
		if body.Privileged {
			section.Privileged++
		}
		section.Items = append(section.Items, body)
	}
	sort.Slice(section.Items, func(i, j int) bool {
		return section.Items[i].Sequence < section.Items[j].Sequence
	})
	if len(records) >= maxAuditPackAuditRecords {
		section.Truncated = true
		section.Message = fmt.Sprintf(
			"this window holds at least %d records for this project, which is the most one read "+
				"returns: the newest %d are here. Narrow the range and take two packs",
			maxAuditPackAuditRecords, maxAuditPackAuditRecords)
	}
	return section, nil
}

// packSignedRecords carries the envelopes that have no registry to live in,
// whole: this project's claims' data-class declarations, and the artefact of
// every recertification cycle in the pack.
//
// Two queries rather than one per cycle. The project's own records come back
// in one read; the cycles' artefacts are looked up by predicate type in a
// second and matched against the subjects the cycles named, because a
// platform-scoped cycle carries no project and a per-cycle query would put a
// round trip in a loop for no gain.
func packSignedRecords(
	ctx context.Context, in assembly, cycles []auditPackReview,
) (auditPackRecords, error) {
	section := auditPackRecords{
		Items: []auditPackRecord{},
		Note: "The envelopes are here byte for byte, not summarized: the payload inside each one " +
			"is what its signature covers, so re-encoding it would break it. Each verifies on " +
			"its own with the platform's public key and with Kitchen out of the loop, by the " +
			"same procedure as the pack — decode `payload`, rebuild DSSE's pre-authentication " +
			"encoding, check the signature. This table carries no retention at all. " +
			"Two kinds of record are here and they are selected differently: a recertification " +
			"cycle's artefact is here because the cycle is, and so follows the window; this " +
			"project's own declarations are every one the platform holds, because they are the " +
			"evidence behind the claims the inventory makes and an inventory row whose signed " +
			"declaration predated the window would otherwise stand unsupported.",
	}

	mine, err := in.store.QuerySignedRecords(ctx, clickhouse.SignedRecordQuery{
		Project: in.project.Name,
		Limit:   maxAuditPackRecords,
	})
	if err != nil {
		return auditPackRecords{}, fmt.Errorf("the signed records could not be read: %w", err)
	}

	wanted := map[string]struct{}{}
	for _, cycle := range cycles {
		if cycle.Subject != "" {
			wanted[cycle.Subject] = struct{}{}
		}
	}
	artefacts := []clickhouse.SignedRecord{}
	if len(wanted) > 0 {
		artefacts, err = in.store.QuerySignedRecords(ctx, clickhouse.SignedRecordQuery{
			Type:  attestation.PredicateAccessReview,
			Limit: maxAuditPackRecords,
		})
		if err != nil {
			return auditPackRecords{}, fmt.Errorf("the recertification artefacts could not be read: %w", err)
		}
	}

	seen := map[string]struct{}{}
	add := func(record clickhouse.SignedRecord) {
		if _, already := seen[record.ID]; already {
			return
		}
		seen[record.ID] = struct{}{}
		section.Items = append(section.Items, auditPackRecord{
			ID:        record.ID,
			Timestamp: record.Timestamp.UTC(),
			Type:      record.Type,
			Subject:   record.Subject,
			Project:   record.Project,
			Envelope:  rawIfValid(record.Envelope),
		})
	}
	for _, record := range mine {
		add(record)
	}
	for _, record := range artefacts {
		if _, ours := wanted[record.Subject]; ours {
			add(record)
		}
	}
	sort.Slice(section.Items, func(i, j int) bool {
		if !section.Items[i].Timestamp.Equal(section.Items[j].Timestamp) {
			return section.Items[i].Timestamp.Before(section.Items[j].Timestamp)
		}
		return section.Items[i].ID < section.Items[j].ID
	})

	if len(mine) >= maxAuditPackRecords || len(artefacts) >= maxAuditPackRecords {
		section.Truncated = true
		section.Message = fmt.Sprintf(
			"at least %d signed records matched, which is the most one read returns", maxAuditPackRecords)
	}
	return section, nil
}
