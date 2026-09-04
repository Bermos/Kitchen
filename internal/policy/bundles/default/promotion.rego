# The built-in promotion policy: the rules an environment can require without
# writing a bundle of its own.
#
# Every rule here is either opted into through input.parameters or inert until
# the facts it judges are present, so pinning this bundle with no parameters
# demands nothing — the bar is whatever the environment's owners turned on.
# Rule ids are stable strings: requirements, exceptions and stored decisions
# all name rules by these ids, and renaming one silently disconnects them.
#
# Everything is read defensively with object.get, because the input is
# materialized from whatever evidence exists: an absent field is an ordinary
# state of the world, never an evaluation error.

package kitchen.promotion

# --- shared vocabulary ------------------------------------------------------

parameters := object.get(input, "parameters", {})

evidence := object.get(input, "evidence", [])

# enabled(name) is how a boolean parameter opts a rule in: the parameter named
# after the rule id, set to "true".
enabled(name) if lower(trim_space(object.get(parameters, name, ""))) == "true"

provenance_types := {
	"https://slsa.dev/provenance/v1",
	"https://slsa.dev/provenance/v0.2",
}

sbom_types := {
	"https://spdx.dev/Document",
	"https://cyclonedx.org/bom",
}

quality_gate_type := "https://kitchen.bermos.dev/attestation/quality-gate/v1"

pull_request_type := "https://kitchen.bermos.dev/attestation/pull-request-approval/v1"

vulnerability_scan_type := "https://kitchen.bermos.dev/attestation/vulnerability-scan/v1"

# --- vendored software ------------------------------------------------------
# Every image of this release that somebody else built (#309). The field is
# absent for a release of images the platform built, so `vendored` is empty
# and every rule below is inert — which is what "inert unless the facts it
# judges are present" means here.
#
# A unit can be half vendored: an upstream image as one workload and a sidecar
# built from a repository as another. So this is a list, each entry naming its
# workload, and the refusals below name the workload rather than the release.
vendored := object.get(object.get(input, "release", {}), "vendored", [])

anything_vendored if count(vendored) > 0

# The name a message calls one of them. `web` is the project's own image,
# which is what the process list, the Release and the dashboard all call it.
vendored_name(artifact) := name if {
	name := object.get(artifact, "workload", "")
	name != ""
}

vendored_name(artifact) := "web" if object.get(artifact, "workload", "") == ""

# What a vendored unit is, in the words a refusal uses. It lists the images
# rather than saying "this release", because on a mixed unit the answer to
# "which part of this was not built here" is the whole of the finding.
vendored_images := concat(", ", sort([name | some artifact in vendored; name := vendored_name(artifact)]))

# --- require-provenance -----------------------------------------------------
# Demands: the artifact carries builder provenance (SLSA v1 or v0.2) among its
# attested evidence. Tuned by: parameters["require-provenance"] = "true".
deny contains {
	"rule": "require-provenance",
	"message": "the artifact carries no builder provenance (SLSA) attestation",
} if {
	enabled("require-provenance")
	not has_provenance
}

has_provenance if {
	some entry in evidence
	object.get(entry, "predicateType", "") in provenance_types
}

# --- require-sbom -----------------------------------------------------------
# Demands: the artifact carries a bill of materials (SPDX or CycloneDX).
# Tuned by: parameters["require-sbom"] = "true".
deny contains {
	"rule": "require-sbom",
	"message": "the artifact carries no software bill of materials (SPDX or CycloneDX) attestation",
} if {
	enabled("require-sbom")
	not has_sbom
}

has_sbom if {
	some entry in evidence
	object.get(entry, "predicateType", "") in sbom_types
}

# --- require-gate -----------------------------------------------------------
# Demands: every named quality gate has run over the artifact, evidenced by a
# quality-gate attestation whose predicate names the gate. Tuned by:
# parameters["requiredGates"] — a comma-separated list (or an array) of gate
# names; naming none disables the rule.
deny contains {"rule": "require-gate", "message": message} if {
	some gate in required_gates
	not gate_ran(gate)
	message := sprintf("required quality gate %q has not run over this artifact", [gate])
}

required_gates := gates if {
	raw := object.get(parameters, "requiredGates", "")
	is_string(raw)
	gates := {gate | some part in split(raw, ","); gate := trim_space(part); gate != ""}
}

required_gates := gates if {
	raw := object.get(parameters, "requiredGates", [])
	is_array(raw)
	gates := {gate | some part in raw; is_string(part); gate := trim_space(part); gate != ""}
}

gate_ran(name) if {
	some entry in evidence
	object.get(entry, "predicateType", "") == quality_gate_type
	object.get(object.get(entry, "predicate", {}), "gate", "") == name
}

# --- require-independent-review ---------------------------------------------
# Demands: the change carries a pull-request-approval attestation asserting it
# was approved by somebody other than its author. Tuned by:
# parameters["require-independent-review"] = "true".
deny contains {
	"rule": "require-independent-review",
	"message": "no attestation asserts this change was independently reviewed",
} if {
	enabled("require-independent-review")
	not anything_vendored
	not independently_reviewed
}

# The vendored half of the same rule. It is a separate clause with a separate
# message because the two findings are different facts: "nobody reviewed this
# change" is a gap somebody can close, and "this artifact has no change to
# review" is a property of what it is. Nothing here is ever satisfied by a
# substitute claim — see the note above deny for why an artifact with no
# commit stays refused rather than being waved through on the adoption record.
deny contains {"rule": "require-independent-review", "message": message} if {
	enabled("require-independent-review")
	anything_vendored
	message := sprintf(
		"this environment requires an independent review, and %s was published by somebody else: a review is a claim about a commit under review, and a vendored artifact has none. Nothing substitutes for it — the questions a vendored digest can answer are upstream-signature-verified and digest-approved-by-someone-else",
		[vendored_images],
	)
}

independently_reviewed if {
	some entry in evidence
	object.get(entry, "predicateType", "") == pull_request_type
	object.get(object.get(entry, "predicate", {}), "independentlyApproved", false) == true
}

# --- no-self-approval -------------------------------------------------------
# Demands: the recorded review must not show the author approving their own
# change. Distinct from require-independent-review: that one wants a reviewer,
# this one objects to a specific reviewer. Tuned by:
# parameters["no-self-approval"] = "true".
deny contains {
	"rule": "no-self-approval",
	"message": "the recorded review shows the author approving their own change",
} if {
	enabled("no-self-approval")
	not anything_vendored
	some entry in evidence
	object.get(entry, "predicateType", "") == pull_request_type
	object.get(object.get(entry, "predicate", {}), "selfApproved", false) == true
}

deny contains {"rule": "no-self-approval", "message": message} if {
	enabled("no-self-approval")
	anything_vendored
	message := sprintf(
		"this environment forbids self-approval, and %s was published by somebody else: there is no review record to read an approver out of. The four-eyes question a vendored digest can answer is digest-approved-by-someone-else",
		[vendored_images],
	)
}

# --- require-pull-request ---------------------------------------------------
# The third commit-shaped rule, and the only one that fires **for a vendored
# artifact alone**.
#
# Its id belongs to a control that runs before a build: a project that requires
# review has its direct pushes refused by the reconciler, long before the
# engine sees anything (internal/controller/sourceprovenance.go, and
# docs/COMPLIANCE.md §8.8). There is deliberately no engine-side rule for a
# built artifact, because a second implementation of one requirement is how two
# answers to one question come about.
#
# But that control never runs for an acquisition — there is no commit to ask a
# provider about — so an environment that requires it would silently require
# nothing of exactly the artifacts it was written to keep out. This clause is
# where it is said instead, at the one place a vendored artifact can be
# refused. Tuned by: parameters["require-pull-request"] = "true".
deny contains {"rule": "require-pull-request", "message": message} if {
	enabled("require-pull-request")
	anything_vendored
	message := sprintf(
		"this environment requires changes to arrive through a reviewed pull request, and %s was published by somebody else: there is no commit and no request, and the platform will not invent one",
		[vendored_images],
	)
}

# --- upstream-signature-verified --------------------------------------------
# Demands: the vendor's own signature on every vendored image of this release
# verified against the key this installation configured. Tuned by:
# parameters["upstream-signature-verified"] = "true", and optionally
# parameters["upstreamSignatureIdentity"] — the identity the signature had to
# name, matched against the one the platform actually checked against.
#
# Three facts can come back and each fires differently, because "the vendor
# publishes no signature" is not the same finding as "the signature did not
# check out" and an operator sent to look at the wrong one wastes an afternoon.
#
# It is inert for a release of images the platform built: a built artifact has
# no upstream, and the questions asked of one are require-provenance and the
# review rules above.
upstream_identity := lower(trim_space(object.get(parameters, "upstreamSignatureIdentity", "")))

deny contains {"rule": "upstream-signature-verified", "message": message} if {
	enabled("upstream-signature-verified")
	some artifact in vendored
	object.get(artifact, "signature", "") == "none"
	message := sprintf(
		"%s is published without a signature, and this environment requires the upstream signature to verify",
		[vendored_name(artifact)],
	)
}

deny contains {"rule": "upstream-signature-verified", "message": message} if {
	enabled("upstream-signature-verified")
	some artifact in vendored
	not object.get(artifact, "signature", "") in {"none", "verified"}
	message := sprintf(
		"the upstream signature on %s did not verify: the platform recorded it as %q",
		[vendored_name(artifact), object.get(artifact, "signature", "unrecorded")],
	)
}

# A signature that verified against the wrong signer is not this environment's
# signature. The identity is compared against the one the *platform* checked
# against — the configured expectation — and never against a subject read out
# of the certificate, which is a claim by whoever wrote the certificate.
deny contains {"rule": "upstream-signature-verified", "message": message} if {
	enabled("upstream-signature-verified")
	upstream_identity != ""
	some artifact in vendored
	object.get(artifact, "signature", "") == "verified"
	lower(trim_space(object.get(artifact, "signatureIdentity", ""))) != upstream_identity
	message := sprintf(
		"the upstream signature on %s verified against %q, and this environment requires %q",
		[
			vendored_name(artifact),
			object.get(artifact, "signatureIdentity", "no named identity"),
			upstream_identity,
		],
	)
}

# --- digest-approved-by-someone-else ----------------------------------------
# Demands: whoever is moving a vendored digest into this environment is not
# whoever admitted it onto the platform. Tuned by:
# parameters["digest-approved-by-someone-else"] = "true".
#
# This is the four-eyes control for software nobody here wrote, and it is a
# **different question** from the three above rather than a substitute for
# them. It does not claim anybody reviewed the code; it claims two people were
# involved in it arriving here, which is the control an auditor actually asks
# a vendored estate for.
#
# It asks nothing on a rescan. `requestedBy` is empty when nobody is asking —
# a scheduled re-evaluation of what is already deployed is not a request by
# anyone — and a four-eyes rule fired against nobody would report every
# vendored environment as drifting every hour, for a question that was asked
# and answered at promotion.
requester := lower(trim_space(object.get(input, "requestedBy", "")))

deny contains {"rule": "digest-approved-by-someone-else", "message": message} if {
	enabled("digest-approved-by-someone-else")
	requester != ""
	some artifact in vendored
	object.get(artifact, "admittedBy", "") == ""
	message := sprintf(
		"the platform has no record of who admitted %s, so it cannot establish that somebody other than %s approved it",
		[vendored_name(artifact), object.get(input, "requestedBy", "the requester")],
	)
}

deny contains {"rule": "digest-approved-by-someone-else", "message": message} if {
	enabled("digest-approved-by-someone-else")
	requester != ""
	some artifact in vendored
	lower(trim_space(object.get(artifact, "admittedBy", ""))) == requester
	message := sprintf(
		"%s was admitted onto the platform by %s, who is also requesting this move: this environment requires the digest to be approved by somebody else",
		[vendored_name(artifact), object.get(artifact, "admittedBy", "")],
	)
}

# --- max-severity -----------------------------------------------------------
# Demands: no vulnerability-scan finding above the named severity, unless a VEX
# statement the environment trusts says this artifact is not affected. Tuned by:
# parameters["maxSeverity"] — one of none, low, medium, high, critical — and,
# for the suppression half, by the four vex* parameters below.
# Matching is deliberately simple: severity strings compared through one rank
# map, and suppression by vulnerability id against input.vex.
severity_rank := {
	"none": 0,
	"negligible": 0,
	"unknown": 0,
	"low": 1,
	"medium": 2,
	"high": 3,
	"critical": 4,
}

max_severity_parameter := lower(trim_space(object.get(parameters, "maxSeverity", "")))

deny contains {"rule": "max-severity", "message": message} if {
	max_severity_parameter != ""
	object.get(severity_rank, max_severity_parameter, -1) == -1
	message := sprintf(
		"parameters.maxSeverity %q is not a severity: use none, low, medium, high or critical",
		[max_severity_parameter],
	)
}

deny contains {"rule": "max-severity", "message": message} if {
	ceiling := object.get(severity_rank, max_severity_parameter, -1)
	ceiling >= 0
	some finding in scan_findings
	severity := lower(trim_space(object.get(finding, "severity", "")))
	object.get(severity_rank, severity, 0) > ceiling
	identifier := vulnerability_of(finding)
	not vex_not_affected(identifier)
	message := sprintf(
		"finding %s is %s, above the allowed maximum severity %q",
		[identifier, severity, max_severity_parameter],
	)
}

# The findings the newest scan reports. It reads every entry carrying the type
# because that is what a comprehension does, but the input carries at most one:
# a scan is a restatement of the same claim and policy.EvidenceFrom collapses
# them to the newest before the rules ever see them. That is deliberately not
# done here — a bundle-side rule would be this bundle's alone, and a custom
# bundle would inherit the accumulation without knowing it had.
scan_findings contains finding if {
	some entry in evidence
	object.get(entry, "predicateType", "") == vulnerability_scan_type
	some finding in object.get(object.get(entry, "predicate", {}), "findings", [])
}

vulnerability_of(finding) := identifier if {
	identifier := object.get(finding, "vulnerability", "")
	identifier != ""
}

vulnerability_of(finding) := identifier if {
	object.get(finding, "vulnerability", "") == ""
	identifier := object.get(finding, "id", "")
	identifier != ""
}

vulnerability_of(finding) := "an unnamed finding" if {
	object.get(finding, "vulnerability", "") == ""
	object.get(finding, "id", "") == ""
}

# --- vex suppression --------------------------------------------------------
# Not a rule of its own — no `deny` here — but the whole of what max-severity
# consults before it fires, and the place an environment says whose word it
# takes about exploitability.
#
# Four things must hold before a finding is suppressed, and each is one line
# below: the statement says `not_affected`, it gives one of OpenVEX's five
# justifications, its signature is acceptable, and it is current. Free text is
# not a justification — a suppression whose reason cannot be counted cannot be
# reviewed in aggregate, which is the whole of what an exception register is
# for. The platform refuses an unjustified `not_affected` at ingest as well;
# this is the half that also covers a document some other tool attached.
#
# Tuned by:
#   parameters["vexRequireVerified"] — "false" honours a statement whose
#       envelope the platform's key did not verify. Defaults to true, so a VEX
#       document pushed by something else under a key nobody here holds is
#       listed and never believed: that is "reject VEX from untrusted signers",
#       on by default rather than opt-in.
#   parameters["vexTrustedAuthors"] — comma-separated authors this environment
#       takes the word of. Empty means every author the platform admitted at
#       ingest, which is the operator's own narrower list. Matching is exact
#       and **case-insensitive**, which is not a nicety: it is the same rule
#       VEXSpec.AdmitsAuthor applies to the platform's own list, and §10.5
#       presents the two as one idea at two levels. An operator who copied
#       `Security@Shop.Example` off the singleton into an environment's
#       parameters and got a case-sensitive comparison would have every
#       statement silently refused, with no message anywhere saying why.
#   parameters["vexMaxAgeDays"] — how old a statement may be, judged against
#       input.at so a replay suppresses exactly what the original suppressed.
#       Empty means no bound. Under a bound, a statement carrying no timestamp
#       is not current, and a parameter that is not a whole number of days
#       suppresses nothing at all — a bound nobody can read bounds everything
#       out, which is the fail-safe direction.
vex_statements := object.get(input, "vex", [])

vex_justifications := {
	"component_not_present",
	"vulnerable_code_not_present",
	"vulnerable_code_not_in_execute_path",
	"vulnerable_code_cannot_be_controlled_by_adversary",
	"inline_mitigations_already_exist",
}

vex_require_verified if lower(trim_space(object.get(parameters, "vexRequireVerified", ""))) != "false"

vex_trusted_authors := {author |
	some part in split(object.get(parameters, "vexTrustedAuthors", ""), ",")
	author := lower(trim_space(part))
	author != ""
}

vex_age_parameter := trim_space(object.get(parameters, "vexMaxAgeDays", ""))

vex_not_affected(identifier) if {
	some statement in vex_statements
	object.get(statement, "vulnerability", "") == identifier
	object.get(statement, "status", "") == "not_affected"
	object.get(statement, "justification", "") in vex_justifications
	vex_signature_acceptable(statement)
	vex_author_acceptable(statement)
	vex_within_age(statement)
}

vex_signature_acceptable(_) if not vex_require_verified

vex_signature_acceptable(statement) if object.get(statement, "verified", false) == true

vex_author_acceptable(_) if count(vex_trusted_authors) == 0

vex_author_acceptable(statement) if lower(trim_space(object.get(statement, "author", ""))) in vex_trusted_authors

vex_within_age(_) if vex_age_parameter == ""

vex_within_age(statement) if {
	regex.match(`^[0-9]+$`, vex_age_parameter)
	stamp := object.get(statement, "timestamp", "")
	stamp != ""
	age_ns := time.parse_rfc3339_ns(object.get(input, "at", "")) - time.parse_rfc3339_ns(stamp)
	age_ns <= (to_number(vex_age_parameter) * 24) * 3600000000000
}

# --- dataclass-le-environment -----------------------------------------------
# Demands: the project's data class does not exceed the environment's — data
# never flows somewhere rated below it. A classified project landing on an
# environment nobody has rated fires too: classified data has no business in
# a container without a rating. Inert only while the project is unclassified
# — an unclassified project asserts nothing to exceed with. No parameter
# tunes it, because the classes themselves are the configuration.
class_rank := {
	"public": 0,
	"internal": 1,
	"confidential": 2,
	"strictlyConfidential": 3,
}

deny contains {"rule": "dataclass-le-environment", "message": message} if {
	project_class := object.get(object.get(input, "project", {}), "dataClass", "")
	environment_class := object.get(object.get(input, "environment", {}), "dataClass", "")
	project_rank := object.get(class_rank, project_class, -1)
	environment_rank := object.get(class_rank, environment_class, -1)
	project_rank >= 0
	environment_rank >= 0
	project_rank > environment_rank
	message := sprintf(
		"the project's data class %q exceeds the environment's %q",
		[project_class, environment_class],
	)
}

deny contains {"rule": "dataclass-le-environment", "message": message} if {
	project_class := object.get(object.get(input, "project", {}), "dataClass", "")
	environment_class := object.get(object.get(input, "environment", {}), "dataClass", "")
	object.get(class_rank, project_class, -1) >= 0
	object.get(class_rank, environment_class, -1) == -1
	message := sprintf(
		"the project is classified %q but the environment is unclassified: rate the environment before promoting into it",
		[project_class],
	)
}

# --- data-provenance-preview ------------------------------------------------
# Demands: a preview environment is provisioned only with masked or synthetic
# data — a claim that declares production-derived data may not back a preview,
# and a claim whose provider declared *nothing* is treated as the worst case
# rather than as clean: undeclared fires unless the policy would accept
# production anyway (or names "undeclared" as acceptable outright). That is
# what makes "a provisioner that cannot declare cannot be used where a class
# is required" a property of the system instead of a convention. Tuned by:
# parameters["preview-data-provenance"] — a comma-separated list of acceptable
# provenances, defaulting to "masked,synthetic".
allowed_preview_provenance := provenances if {
	raw := trim_space(object.get(parameters, "preview-data-provenance", ""))
	raw != ""
	provenances := {p | some part in split(raw, ","); p := trim_space(part); p != ""}
}

allowed_preview_provenance := {"masked", "synthetic"} if {
	trim_space(object.get(parameters, "preview-data-provenance", "")) == ""
}

# An acceptable provenance: listed, or — for the undeclared case alone — the
# policy already accepts production, because a policy that tolerates the known
# worst has nothing left to protect from the unknown.
preview_provenance_acceptable(provenance) if provenance in allowed_preview_provenance

preview_provenance_acceptable("undeclared") if "production" in allowed_preview_provenance

deny contains {"rule": "data-provenance-preview", "message": message} if {
	object.get(object.get(input, "environment", {}), "type", "") == "preview"
	some claim in object.get(input, "claims", [])
	object.get(claim, "provenance", "") != ""
	provenance := object.get(claim, "provenance", "")
	not preview_provenance_acceptable(provenance)
	message := sprintf(
		"claim %q holds %s-derived data, and a preview environment accepts only: %s",
		[object.get(claim, "name", "unnamed"), provenance, concat(", ", sort(allowed_preview_provenance))],
	)
}

deny contains {"rule": "data-provenance-preview", "message": message} if {
	object.get(object.get(input, "environment", {}), "type", "") == "preview"
	some claim in object.get(input, "claims", [])
	object.get(claim, "provenance", "") == ""
	not preview_provenance_acceptable("undeclared")
	message := sprintf(
		"claim %q declares no data provenance, and an undeclared claim is treated as production-derived: a preview accepts only %s, or a policy that permits production",
		[object.get(claim, "name", "unnamed"), concat(", ", sort(allowed_preview_provenance))],
	)
}
