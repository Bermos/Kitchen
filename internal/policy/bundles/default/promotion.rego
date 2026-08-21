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
# Demands: no vulnerability-scan finding above the named severity, unless a
# VEX statement says the product is not affected. Tuned by:
# parameters["maxSeverity"] — one of none, low, medium, high, critical.
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

vex_not_affected(identifier) if {
	some statement in object.get(input, "vex", [])
	object.get(statement, "status", "") == "not_affected"
	object.get(statement, "vulnerability", "") == identifier
}

# --- dataclass-le-environment -----------------------------------------------
# Demands: the project's data class does not exceed the environment's — data
# never flows somewhere rated below it. Inert until both sides are classified
# (issue #137 fills the fields); no parameter tunes it, because the classes
# themselves are the configuration.
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

# --- data-provenance-preview ------------------------------------------------
# Demands: a preview environment is provisioned only with masked or synthetic
# data — a claim that declares production-derived data may not back a preview.
# Inert until claims declare a provenance (issue #138 populates it). Tuned by:
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

deny contains {"rule": "data-provenance-preview", "message": message} if {
	object.get(object.get(input, "environment", {}), "type", "") == "preview"
	some claim in object.get(input, "claims", [])
	provenance := object.get(claim, "provenance", "")
	provenance != ""
	not provenance in allowed_preview_provenance
	message := sprintf(
		"claim %q holds %s-derived data, and a preview environment accepts only: %s",
		[object.get(claim, "name", "unnamed"), provenance, concat(", ", sort(allowed_preview_provenance))],
	)
}
