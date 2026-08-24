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
	not independently_reviewed
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
	some entry in evidence
	object.get(entry, "predicateType", "") == pull_request_type
	object.get(object.get(entry, "predicate", {}), "selfApproved", false) == true
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
#       ingest, which is the operator's own narrower list.
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
	author := trim_space(part)
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

vex_author_acceptable(statement) if object.get(statement, "author", "") in vex_trusted_authors

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
