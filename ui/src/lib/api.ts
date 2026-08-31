import { loadConfig } from "./config";
import { renew, signOut, token } from "./auth";

// Typed client for the operator REST API (docs/API.md). The types mirror the
// API's view shapes — the platform's own vocabulary, nothing Kubernetes.

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime: string;
}

/** One key of a Secret or a ResourceClaim binding backing an env var. */
export interface KeyRef {
  name: string;
  key: string;
}

/** One of a project's environment variables, as read. Values never come back
 * out of the API — `set` and `previewSet` say only that there is one — and
 * secret- and claim-backed variables carry the reference they were written
 * as. */
export interface EnvVar {
  name: string;
  set: boolean;
  previewSet: boolean;
  fromSecret?: KeyRef;
  fromClaim?: KeyRef;
}

/**
 * One of a project's own secrets: a credential the platform did not mint.
 *
 * There is no value here and there never will be one — the API answers a name
 * and the reference an environment variable reads it by, and nothing else
 * exists to answer. `reference` is served rather than derived so that nothing
 * in this dashboard has to know the name of the object the platform keeps
 * secrets in.
 */
export interface ProjectSecret {
  name: string;
  reference: KeyRef;
}

/** One environment variable as written. Nothing reads a value back, so an
 * absent `value` keeps the stored one and an empty one clears it. */
export interface EnvVarWrite {
  name: string;
  value?: string;
  previewValue?: string;
  fromSecret?: KeyRef;
  fromClaim?: KeyRef;
}

/**
 * What the platform asks a workload before it sends anyone to it, with every
 * timing resolved.
 *
 * It is always present on a project: every environment is probed, and a
 * project that declared nothing is reported with the default check — a TCP
 * connect to the container's port — rather than with nothing, which would
 * read as "not checked". On a worker it is absent unless the worker asked
 * for one.
 */
export interface Health {
  /** The HTTP path the probe asks for. Empty means a TCP connect, which is
   * deliberately not `GET /`: plenty of applications answer that before they
   * are ready, and one that 404s there would never become Ready at all. */
  path?: string;
  /** The port probed. Empty on a project means the container's own. */
  port?: number;
  periodSeconds: number;
  timeoutSeconds: number;
  failureThreshold: number;
  /** The generous one. A container has this many checks x the period to
   * come up before the platform gives up on it, and it is separate from
   * failureThreshold precisely so that slow startup does not have to loosen
   * the threshold that catches a wedge afterwards. */
  startupFailureThreshold: number;
}

/** A health check as a settings PATCH carries it. Every number is optional
 * and 0 takes the platform's default; sending `{}` restores the default
 * check. */
export interface HealthSettings {
  path?: string;
  port?: number;
  periodSeconds?: number;
  timeoutSeconds?: number;
  failureThreshold?: number;
  startupFailureThreshold?: number;
}

export interface Project {
  name: string;
  /** The calling account's role on this project: "admin", "developer" or
   * "viewer". It arrives with every project rather than as a list to join
   * against, because the overview renders a list of them — and it is the role
   * itself rather than a set of capability booleans, so what the dashboard
   * offers is derived from the same table the API enforces. An operator reads
   * "admin" on every project, including ones they are not listed on. */
  role: string;
  repo: string;
  connection: string;
  registry: string;
  productionBranch: string;
  /** Refuse to build a production-branch commit the git provider cannot say
   *  arrived through a reviewed pull request. */
  requirePullRequest: boolean;
  previews: boolean;
  previewsProtected: boolean;
  buildStrategy?: string;
  dockerfilePath?: string;
  rootDirectory?: string;
  env?: EnvVar[];
  port?: number;
  replicas?: number;
  cpu?: string;
  memory?: string;
  /** What the platform checks the application with. Always present. */
  health?: Health;
  /** What the application is started with, in exec form — a list of words,
   * never a shell line. Absent means the image's own entrypoint. */
  command?: string[];
  args?: string[];
  /** What a preview runs instead of `args`: the sibling of an environment
   * variable's preview value. Absent or empty means a preview runs
   * production's, which is how an override is taken away. */
  previewArgs?: string[];
  /** Two of this workload must never run at once: it is deployed by stopping
   * the old copy before starting the new one, and it cannot be given more
   * than one replica. */
  singleton?: boolean;
  /** This workload does work nobody asked for, so no environment of this
   * project idles down to no pods — not even a preview, which would
   * otherwise idle by default. */
  notRequestDriven?: boolean;
  productionEnvironment?: string;
  latestBuild?: string;
  createdAt: string;
  conditions?: Condition[];
  /** The project's staged pipeline, in promotion order. Absent for the
   * default build-straight-to-production flow. Stages are topology — what
   * each environment demands lives on the Environment's requirements. */
  promotionStages?: PromotionStage[];
  /** The sensitivity classification of the data this project handles:
   * public, internal, confidential or strictlyConfidential. Absent means
   * unclassified — shown as such, never defaulted. */
  dataClass?: string;
  /** How much it matters that this project's function keeps working, and the
   * tolerances that come with it. Absent means undesignated: the institution
   * has not said, and Kitchen does not decide. */
  criticality?: string;
  rto?: string;
  rpo?: string;
}

/** The classification vocabulary, in ascending sensitivity — the order the
 * platform compares classes in. */
export const DATA_CLASSES = ["public", "internal", "confidential", "strictlyConfidential"] as const;

/** One rung of a project's promotion ladder. */
export interface PromotionStage {
  name: string;
  environment: string;
  /** Whether the platform creates the next promotion into this stage by
   * itself when the stage before it applies. */
  autoPromote: boolean;
}

/** One request to move a release into an environment, with what the policy
 * decided about it. The spec is immutable: retrying a blocked promotion is a
 * new one, and an old one stays as the record of what was refused and why. */
export interface Promotion {
  name: string;
  project: string;
  environment: string;
  release: string;
  requestedBy: string;
  trigger: string;
  reason?: string;
  /** Pending, Evaluating, Allowed, AllowedWithException, Blocked, Applied or
   * Failed. Blocked and Failed are terminal, like Applied. */
  phase: string;
  verdict?: string;
  /** The stored decision behind the verdict — the decision register holds
   * its fired rules and the replayable input. */
  decisionID?: string;
  /** The rules that fired unwaived, by their stable ids. */
  unmetRules?: string[];
  message?: string;
  evaluatedAt?: string;
  appliedAt?: string;
  createdAt: string;
  conditions?: Condition[];
}

/**
 * What PATCH /projects/{name} accepts: absent fields keep their value.
 *
 * **Environment variables are deliberately not here.** They moved to
 * `PATCH /projects/{name}/env`, which wants `developer` where this route
 * wants `admin` — a whole route is the unit of authorization, so the two are
 * two routes. A body carrying `env` is now refused with a `400` naming the
 * other one, and leaving the field off this type is what stops the dashboard
 * sending it by accident.
 */
export interface ProjectSettings {
  productionBranch?: string;
  requirePullRequest?: boolean;
  previews?: boolean;
  previewsProtected?: boolean;
  buildStrategy?: string;
  dockerfilePath?: string;
  rootDirectory?: string;
  port?: number;
  replicas?: number;
  cpu?: string;
  memory?: string;
  /** Replace the health check the platform probes with. `{}` restores the
   * default one. */
  health?: HealthSettings;
  /** Replace what the application is started with. Each replaces its whole
   * list and `[]` clears it — an application started with no arguments,
   * where leaving the field out keeps whatever it had. */
  command?: string[];
  args?: string[];
  previewArgs?: string[];
  /** Declare the workload a singleton. Sending it with more than one replica
   * is refused rather than clamped: a value quietly lowered would read back
   * as a setting that did not take. */
  singleton?: boolean;
  /** Declare that the workload does work nobody asked for, which turns
   * idling off for every environment of the project. */
  notRequestDriven?: boolean;
  /** Reclassify the project's data; "" removes the classification. Always
   * allowed — environments rated below the new class read as non-compliant
   * in the inventory and at promotion, rather than the correction being
   * refused. Audit-logged with the previous value. */
  dataClass?: string;
  /** Designate the project's function and its tolerances; "" removes each.
   * Always allowed, and never a gate — criticality is an input to alerting
   * and to policy. Audit-logged with the previous values. */
  criticality?: string;
  rto?: string;
  rpo?: string;
}

export interface NewProject {
  name: string;
  repo: string;
  connection: string;
  registry: string;
  productionBranch?: string;
  previews?: boolean;
  /** The build context, when the preflight showed it was wrong and somebody
   * corrected it on the form rather than after a failed build. */
  rootDirectory?: string;
  dockerfilePath?: string;
}

export interface Revision {
  sha: string;
  branch: string;
  message?: string;
  author?: string;
  pullRequest?: number;
}

export interface Build {
  name: string;
  project: string;
  phase?: string;
  git: Revision;
  detectedFramework?: string;
  /** The kitchen.json this commit carried, when it carried one. */
  config?: RepoConfig;
  image?: string;
  artifact?: Artifact;
  cache?: BuildCache;
  gates?: QualityGate[];
  source?: SourceProvenance;
  /** Why the build failed, when it did. Absent on every build that did not. */
  failure?: BuildFailure;
  startedAt?: string;
  completedAt?: string;
  createdAt: string;
  conditions?: Condition[];
}

/** The commit's own kitchen.json: where it was read from, and which settings
 *  it took over from the project.
 *
 *  It answers what was declared rather than what it was declared as. The
 *  values are already in the release the build produced and on the
 *  environment running it, so repeating them here would be a second copy to
 *  disagree with the first. What nothing else answers is which settings
 *  stopped being editable in the dashboard, which is the thing somebody about
 *  to edit one needs to know. */
export interface RepoConfig {
  /** The file, relative to the repository root. */
  path: string;
  /** Every setting it declares, dotted: "build.strategy", "runtime.port",
   *  "env.NODE_ENV", "processes". */
  declares: string[];
}

/** A failed build's own account of itself.
 *
 *  The Job behind a build says only "Job has reached the specified backoff
 *  limit", which is true of every failed build there has ever been. This is
 *  the answer to the question that sentence leaves: which container stopped
 *  the build, how it exited, and the last of what it printed.
 *
 *  It is on the build rather than only on the pod, which means a developer can
 *  read it — a pod is the operator's screen, and a build that failed is not
 *  the operator's problem. */
export interface BuildFailure {
  /** The container that ended the build — the clone as readily as the builder. */
  container?: string;
  /** What it exited with. Absent when nothing exited: a pod evicted before it
   *  ran, or an image that would not pull. */
  exitCode?: number;
  /** Kubernetes' own word for the ending, kept unchanged so that a search for
   *  it finds this build. */
  reason?: string;
  /** The failure in one line. */
  message?: string;
  /** The tail of that container's output, oldest line first. A copy taken when
   *  the failure was seen, for the case the log store cannot serve. */
  log?: string[];
}

/** What the layer cache did for a build. A cold build had nothing to reuse,
 *  which is the difference between "this was slow" and "this is a regression". */
export interface BuildCache {
  enabled: boolean;
  warm: boolean;
  /** Where the cache is kept in the registry. */
  ref?: string;
  /** How much of the build was cached: max or min. Empty for buildpacks,
   *  whose lifecycle has one cache image and no such choice. */
  mode?: string;
  /** Why there was no cache, when there was none. */
  message?: string;
}

/** What a build produced, by content, and whether the platform attested it.
 *  The evidence itself is not here — it lives in the registry against the
 *  digest, and `attestations()` is what reads it back. */
export interface Artifact {
  repository?: string;
  digest?: string;
  attested: boolean;
  attestedAt?: string;
  keyID?: string;
  /** What is attached to the digest, by predicate type. Enough to say that
   *  provenance and an SBOM are there without asking the registry; the
   *  evidence itself still comes from `attestations()`. */
  evidence?: ArtifactEvidence[];
  /** Why an artifact is unattested, when it is. */
  message?: string;
}

/** How the commit reached the branch: through review, or not.
 *
 *  Every field is a third party's claim, which is why `provider` travels with
 *  them — the platform did not witness the review, it asked and was answered.
 *  `required` says whether the project demanded review for this commit, so a
 *  build carrying none reads as "not asked for" rather than "asked for and
 *  missing". */
export interface SourceProvenance {
  provider?: string;
  pullRequest?: number;
  title?: string;
  author?: string;
  mergedBy?: string;
  approvers?: string[];
  selfApproved: boolean;
  independent: boolean;
  /** The allowlisted machine identity this commit was exempted under, if any. */
  machineIdentity?: string;
  required: boolean;
  checkedAt?: string;
  /** Why nothing could be established — an outage, or a provider that cannot
   *  answer. Not a finding about the commit. */
  message?: string;
}

/** One quality gate's run over a build's artifact.
 *
 *  `Completed` means the gate ran, whatever it found — a scanner reporting a
 *  hundred critical vulnerabilities has completed, because it did its job.
 *  `Failed` means it did not run at all and nothing is known either way.
 *  Nothing here says whether the findings were acceptable: gates record facts,
 *  and whether a fact is disqualifying is a property of the environment being
 *  deployed to. */
export interface QualityGate {
  name: string;
  phase?: "Pending" | "Running" | "Completed" | "Failed" | "Skipped";
  /** `platform` for a gate Kitchen ran, `external` for a result submitted by
   *  something that had already run it. */
  source?: "platform" | "external";
  /** Who submitted an external result. Empty for one the platform ran. */
  reportedBy?: string;
  predicateType?: string;
  attested: boolean;
  finishedAt?: string;
  message?: string;
}

/** One attestation attached to an artifact, as the Build reports it.
 *
 *  `kind` is a label the API derives from the predicate type, so that this
 *  copy of the vocabulary does not have to be kept in step by hand. `source`
 *  says who made the claim — the platform signs both, so the signature cannot
 *  tell them apart, and a claim about what a build did is worth more when it
 *  comes from the process that did the building. */
export interface ArtifactEvidence {
  predicateType: string;
  kind: "provenance" | "sbom" | "buildRecord" | "deployment" | "other";
  source?: "builder" | "platform";
  manifest?: string;
}

/** One entry in the tamper-evident log. The chain fields come back with every
 *  record on purpose: a view that hid them would be asking to be believed. */
export interface AuditRecord {
  sequence: number;
  timestamp: string;
  actor: string;
  actorKind: string;
  correlation?: string;
  operation: string;
  kind: string;
  name: string;
  project?: string;
  fromState?: string;
  toState?: string;
  reason?: string;
  details?: string;
  /** A transition that moved a control rather than a workload — a waiver, a
   *  requirement, a credential, a grant, or a write the platform did not
   *  make. Read out of the details, which is what the chain hashes: the
   *  marking cannot be added or removed without verification saying so. */
  privileged?: boolean;
  privilegeClass?: string;
  prevHash: string;
  hash: string;
}

export interface AuditQuery {
  kind?: string;
  name?: string;
  project?: string;
  actor?: string;
  privileged?: boolean;
  privilegeClass?: string;
  since?: string;
  until?: string;
  limit?: number;
}

/** A break in the chain: a record edited, removed, or slipped in. */
export interface AuditFinding {
  sequence: number;
  break: "mutated" | "missing" | "unlinked";
  detail: string;
}

export interface AuditVerification {
  from: number;
  to: number;
  checked: number;
  intact: boolean;
  findings: AuditFinding[];
  /** Where the platform believes the chain ends, held outside the table. A
   *  run that is intact but ends below this is a log cut short from the end. */
  anchor: number;
  truncated: boolean;
}

/** What the platform is producing about itself. */
export interface Compliance {
  audit: {
    enabled: boolean;
    recording: boolean;
    retentionDays: number;
    sequence: number;
    message?: string;
  };
  attestation: {
    enabled: boolean;
    signing: boolean;
    keyID?: string;
    /** The verification key, PEM. Public by construction — evidence signed
     *  under a key nobody can obtain is evidence nobody can check. */
    publicKey?: string;
    message?: string;
  };
  /** Whether policy decisions are being stored. The engine always evaluates;
   *  without a store the decisions stand but cannot be replayed later, and
   *  this is where the platform says so. */
  policy: {
    storing: boolean;
    message?: string;
  };
}

/** One row of the classification inventory: an environment or a claim with
 *  its data class, its data's provenance and its location. The absences are
 *  words, not blanks — "unclassified", "undeclared", "unknown" — so an export
 *  cannot leave an empty cell open to a generous reading. */
export interface InventoryItem {
  kind: "environment" | "claim";
  project: string;
  name: string;
  type: string;
  dataClass: string;
  /** Claims only: what the provisioned data derives from — production,
   *  masked, synthetic, or "undeclared" when the provider said nothing. */
  provenance?: string;
  residency: string;
}

/** The whole classification inventory in one request, exportable as it is.
 *  Filtered to the caller's projects; an operator reads the whole install. */
export interface ComplianceInventory {
  generatedAt: string;
  /** The platform's declared default residency — declared, not observed. */
  defaultResidency?: string;
  items: InventoryItem[];
}

/** The criticality vocabulary, ascending. Kitchen carries the designation;
 *  it never decides it. Absent is answered as the word "undesignated". */
export const CRITICALITIES = ["nonCritical", "important", "critical"] as const;

/** One environment under a designated function, with the designation that
 *  actually applies to it. `inherited` names the fields that came from the
 *  project rather than from the environment, so nothing on a screen reads as
 *  a declaration nobody made. */
export interface CriticalityEnvironment {
  name: string;
  type: string;
  criticality: string;
  rto?: string;
  rpo?: string;
  inherited?: string[];
  url?: string;
  release?: string;
  image?: string;
  domains?: string[];
}

/** One provisioned resource under a function, with the third party behind it. */
export interface CriticalityClaim {
  name: string;
  type: string;
  connection?: string;
  provider?: string;
  phase?: string;
  dataClass: string;
  residency: string;
}

/** One third-party relationship a function depends on, and what for. */
export interface CriticalityConnection {
  name: string;
  provider: string;
  usedFor: string[];
}

/** One designated function and everything the platform can see behind it. */
export interface CriticalityFunction {
  project: string;
  criticality: string;
  rto?: string;
  rpo?: string;
  environments: CriticalityEnvironment[];
  claims: CriticalityClaim[];
  connections: CriticalityConnection[];
  thirdParties: string[];
}

/** The function-to-resource mapping in one request. */
export interface CriticalityMap {
  generatedAt: string;
  minimum?: string;
  functions: CriticalityFunction[];
  /** How many visible projects carry no designation anywhere — the number
   *  that says whether a short map is a small estate or an unfinished
   *  designation exercise. */
  undesignated: number;
  /** How far the traversal follows, in the answer's own words. */
  depth: string;
}

/** One environment that would be affected, and how it reaches the subject. */
export interface CriticalityDependent {
  project: string;
  environment: string;
  type: string;
  criticality: string;
  rto?: string;
  rpo?: string;
  inherited?: string[];
  through: string[];
}

/** What the reverse query was asked about. */
export interface CriticalitySubject {
  kind: "connection" | "provider";
  name: string;
  provider?: string;
  connections?: string[];
}

/** What breaks if one connection, or one third party, is unavailable. */
export interface CriticalityDependents {
  generatedAt: string;
  subject: CriticalitySubject;
  affected: CriticalityDependent[];
  counts: Record<string, number>;
  /** The smallest recovery objective among the affected environments: how
   *  long this third party may be gone before the first declared tolerance
   *  is breached. */
  tightestRTO?: string;
  depth: string;
}

/** One rule standing in the way of a release that is already deployed, and
 *  whether it was standing there when the release was promoted. */
export interface DriftRule {
  rule: string;
  message?: string;
  /** "rescan" for a rule that started failing after promotion; "promotion"
   *  for one that fired then too and was waived by an exception which has
   *  since run out. The distinction is the whole point of the view. */
  since: "rescan" | "promotion";
  /** The grant waiving this rule in the evaluation this row reports — the one
   *  currently holding the release up. Empty on a rule firing unwaived. */
  exception?: string;
  /** The grant that waived this rule when the release was promoted, where
   *  there was one. On a blocked row that is the grant which has since run
   *  out, and it is the reader's next stop. */
  waivedAtPromotion?: string;
}

/** One deployed (environment, release) pair measured against its bar today. */
export interface DriftItem {
  project: string;
  environment: string;
  release: string;
  artifact?: string;
  status: "compliant" | "waived" | "newly-failing" | "waived-at-promotion" | "not-evaluated";
  verdict?: string;
  scannedAt?: string;
  /** The vulnerability database the finding was produced against. An
   *  "unpinned:" prefix means the scanner could not name its own database and
   *  the scan is dated rather than reproducible. */
  dataSnapshot?: string;
  findings?: number;
  decisionID?: string;
  /** Why the most recent scan attempt did not run, where it did not. A row
   *  carrying it is answering with something older than the failure, whatever
   *  its verdict says — which is why such a row is never `compliant`. */
  scanFailed?: string;
  promotedVerdict?: string;
  promotedAt?: string;
  rules: DriftRule[];
  message?: string;
}

/** What is running right now that no longer meets its environment's bar.
 *  `rescanning` false means nothing is checking, which is a different answer
 *  from nothing being wrong — and the screen says which. */
export interface ComplianceDrift {
  generatedAt: string;
  rescanning: boolean;
  message?: string;
  drifting: number;
  items: DriftItem[];
  counts?: Record<string, number>;
}

/** One rule a policy decision fired. A waived rule fired all the same — the
 *  exception changed the verdict, never the facts. */
export interface FiredRule {
  rule: string;
  message?: string;
  waived?: boolean;
  exception?: string;
}

/** One stored policy decision: what was asked, what was answered, and the
 *  digests it can be reproduced from. `input` is present only on the
 *  single-decision read. */
export interface Decision {
  id: string;
  timestamp: string;
  kind: string;
  project?: string;
  environment?: string;
  release?: string;
  artifact?: string;
  bundleDigest: string;
  inputDigest: string;
  dataSnapshot?: string;
  verdict: string;
  rulesFired?: FiredRule[];
  input?: unknown;
  decidedBy?: string;
}

export interface DecisionQuery {
  project?: string;
  environment?: string;
  release?: string;
  verdict?: string;
  kind?: string;
  since?: string;
  until?: string;
  limit?: number;
}

/** A stored decision re-evaluated from its stored inputs: both verdicts, and
 *  whether they match — the bit the endpoint exists for. */
export interface DecisionReplay {
  original: { verdict: string };
  replay: { verdict: string; fired?: FiredRule[] };
  match: boolean;
  decision: string;
}

/** One break-glass exception: a bounded, two-person, per-rule waiver. Phase
 *  is the effective phase — the server judges it against the clock, so a
 *  grant past its expiry never reads Active. */
export interface Exception {
  name: string;
  project: string;
  environment: string;
  release?: string;
  ruleIDs: string[];
  reason: string;
  requestedBy: string;
  approvedBy: string;
  incidentRef?: string;
  expiresAt: string;
  autoRollback: boolean;
  phase: "Active" | "Expired" | "Resolved";
  usedBy?: string[];
  resolvedBy?: string;
  resolvedAt?: string;
  createdAt: string;
  conditions?: Condition[];
}

/** One grant in the access survey: one account's one role in one place, with
 *  what the platform knows about whether anybody is still behind it.
 *
 *  `orphaned` is `inactive` AND `unknown` together, never either alone —
 *  either on its own has an innocent reading (a quiet quarter, an issuer that
 *  serves no directory) and the pair does not. */
export interface Identity {
  subject: string;
  email?: string;
  grant: string;
  role: string;
  lastActive?: string;
  inactive?: boolean;
  unknown?: boolean;
  orphaned?: boolean;
}

/** Who holds what on the platform, whole. `directoryConsulted` is
 *  load-bearing: false means nothing at all is claimed about ownership,
 *  because the identity provider could not be asked. */
export interface IdentitySurvey {
  generatedAt: string;
  inactivityDays: number;
  directoryConsulted: boolean;
  identities: Identity[];
  orphans: number;
  message?: string;
}

/** One grant inside a recertification cycle, with what was decided about it. */
export interface AccessReviewEntry {
  subject: string;
  email?: string;
  grant: string;
  role: string;
  lastActive?: string;
  inactive?: boolean;
  unknown?: boolean;
  orphaned?: boolean;
  decision?: "confirm" | "revoke";
  decidedBy?: string;
  decidedAt?: string;
  note?: string;
  selfReview?: boolean;
  applied?: boolean;
  applyMessage?: string;
}

/** The retained artefact a closed cycle produced: a pointer to the signed
 *  record in the store, never a copy of it. `message` is what to read when a
 *  cycle closed without one. */
export interface AccessReviewArtifact {
  recordID?: string;
  subject?: string;
  predicateType?: string;
  signedAt?: string;
  message?: string;
}

/** One recertification cycle. `phase` is judged against the clock server-side,
 *  so Overdue means overdue now rather than "the reconciler got round to it".
 *  Overdue is a report and never a consequence. */
export interface AccessReview {
  name: string;
  scope: "platform" | "project" | "all";
  project?: string;
  reviewers: string[];
  openedBy: string;
  reason?: string;
  dueBy: string;
  openedAt?: string;
  snapshotAt?: string;
  closedBy?: string;
  closedAt?: string;
  phase: "Open" | "Overdue" | "Closed";
  pending: number;
  confirmed: number;
  revoked: number;
  selfReviewed: number;
  orphaned: number;
  entries: AccessReviewEntry[];
  artifact?: AccessReviewArtifact;
  createdAt: string;
  conditions?: Condition[];
}

/** One decision a reviewer records about one grant. The (subject, grant) pair
 *  identifies the entry: an account holding a role on four projects is four
 *  decisions. */
export interface AccessDecision {
  subject: string;
  grant: string;
  decision: "confirm" | "revoke";
  note?: string;
}

/** One policy bundle available to require: what an environment owner pins. */
export interface PolicyBundle {
  digest: string;
  source: string;
  rules: string[];
}

/** One attestation attached to an artifact's digest. */
export interface Evidence {
  predicateType: string;
  statement: {
    _type: string;
    subject: { name?: string; digest?: Record<string, string> }[];
    predicateType: string;
    predicate: unknown;
  };
  envelope: { payloadType: string; payload: string; signatures: { keyid?: string; sig: string }[] };
  verified: boolean;
  keyIDs?: string[];
  digest: string;
}

/** One OpenVEX statement about an artifact, as the API joins it to the
 *  findings.
 *
 *  `justified`, `expired` and `verified` are facts about the statement, never
 *  a verdict: whether it actually suppresses anything is the target
 *  environment's policy's question, and the same statement can be honoured in
 *  staging and refused in production. */
export interface VEXStatement {
  vulnerability: string;
  status: string;
  justification?: string;
  products?: string[];
  justified: boolean;
  author?: string;
  /** Who handed the document to the platform, which is a different fact from
   *  who the document says wrote it. */
  submittedBy?: string;
  documentID?: string;
  timestamp?: string;
  expiresAt?: string;
  expired: boolean;
  verified: boolean;
  statusNotes?: string;
  impactStatement?: string;
  actionStatement?: string;
}

/** One finding from the artifact's newest vulnerability scan, beside the
 *  statement covering it. A suppressed finding is still a finding. */
export interface VEXFinding {
  vulnerability: string;
  severity?: string;
  package?: string;
  version?: string;
  fixedIn?: string;
  vex?: VEXStatement;
}

export interface VEXAnswer {
  subject: string;
  /** "verified" when signatures were checked, "listed" when there was no key
   *  to check them with — a reader that cannot tell the two apart will
   *  eventually treat one as the other. */
  verification: string;
  statements: VEXStatement[];
  findings: VEXFinding[];
  caveat?: string;
}

export interface EvidenceSet {
  subject: string;
  /** Whether signatures were checked at all. A listing and a verification are
   *  different things, and a reader that cannot tell them apart will
   *  eventually treat one as the other. */
  verified: boolean;
  attestations: Evidence[];
}

export interface Release {
  name: string;
  project: string;
  build: string;
  image: string;
  environments?: string[];
  createdAt: string;
}

/** How one entry of a release's configuration snapshot compares with
 * another's. `change` is the platform's own verdict over two literals it holds
 * and the dashboard does not: the API never reads a value back, so the
 * comparison is made on the server and only the verdict crosses the wire.
 *
 * The direction is the write's — the release named in the path is where the
 * environment is going, so a variable the live release sets and the target
 * does not reads `removed`. */
export type ConfigChange = "added" | "removed" | "changed" | "unchanged";

/** Where a variable's value comes from. A reference is not a secret and
 * travels with the diff; a literal is only ever `"value"`. */
export type ConfigSource = "value" | "secret" | "claim";

export interface VariableChange {
  name: string;
  change: ConfigChange;
  /** The source in the target release; absent when the target does not carry
   * the variable at all. */
  source?: ConfigSource;
  /** The source in the release running now; absent when that one does not. */
  againstSource?: ConfigSource;
  ref?: KeyRef;
  againstRef?: KeyRef;
  /** The change is confined to the preview override: the two releases agree
   * about what every environment but a preview runs with. Without it a
   * preview-only edit would read, on a production environment, as a change to
   * production — which it is not. */
  previewOnly?: boolean;
}

/** One runtime field across the two snapshots. Unlike a variable these carry
 * their values: a port, a replica count and a compute request are project
 * settings a viewer already reads. */
export interface FieldChange {
  field: string;
  from?: string;
  to?: string;
  changed: boolean;
}

export interface ProcessChange {
  name: string;
  change: ConfigChange;
  type?: string;
  schedule?: string;
}

/** GET /releases/{name}/config-diff?against= — what a move to `release` would
 * change about the configuration `against` is running with. Every list is
 * complete, unchanged entries included: the count of what did not move is
 * part of the reassurance. */
export interface ConfigDiff {
  release: string;
  against: string;
  project: string;
  variables: VariableChange[];
  runtime: FieldChange[];
  processes: ProcessChange[];
}

export interface Preview {
  pullRequest: number;
  branch: string;
}

/** One completed stint of a release being current on an environment: when it
 * held the environment, and how and by whom it stopped being current. */
export interface ReleaseHistoryEntry {
  release: string;
  from: string;
  to: string;
  reason: "promoted" | "rolledBack" | "superseded";
  by?: string;
}

/** The bar an environment declares: a policy bundle pinned by digest, and the
 * parameters its owners tuned it with. Neither is a secret — the point of a
 * declared requirement is that the deploying team can read what it will be
 * judged against. */
export interface EnvironmentRequirements {
  bundleDigest: string;
  parameters?: Record<string, string>;
}

export interface Environment {
  name: string;
  project: string;
  type: string;
  release: string;
  observedRelease?: string;
  phase?: string;
  url?: string;
  preview?: Preview;
  /** Who may change this environment's requirements — subjects or verified
   * email addresses, the access-entry vocabulary. Platform operators always
   * may; an empty or absent list leaves the bar to the operators alone. */
  owners?: string[];
  requirements?: EnvironmentRequirements;
  /** The highest sensitivity class this environment is rated to hold,
   * declared by its owners; absent means unrated. */
  dataClass?: string;
  /** Where this environment's data is declared to be — declared, not
   * observed. Absent falls back to the platform's declared residency. */
  residency?: string;
  /** What this environment itself declares. Absent means it declares
   * nothing, which is not the same as nothing applying: a production
   * environment reads its project's designation, and a preview reads none.
   * GET /compliance/criticality answers with that resolved. */
  criticality?: string;
  rto?: string;
  rpo?: string;
  history?: ReleaseHistoryEntry[];
  createdAt: string;
  conditions?: Condition[];
}

/** One attestation as an eligibility answer counts it: what kind of claim,
 * whose it is, and whether it verified against the platform's key. */
export interface EligibilityEvidence {
  predicateType: string;
  /** "platform" or "builder" for evidence the platform indexed; absent for
   * evidence found in the registry that nothing here attached. */
  source?: string;
  verified: boolean;
}

/** How a release measures up against an environment's requirements
 * (GET /environments/{name}/eligibility) — a pure function of stored
 * evidence. `eligible` is three-valued on purpose: null means nothing has
 * judged the pair yet, which is not the same claim as passed. */
export interface EnvironmentEligibility {
  environment: string;
  project: string;
  release: string;
  requirements: EnvironmentRequirements | null;
  evidence: EligibilityEvidence[];
  eligible: boolean | null;
  evaluated: boolean;
  /** The rules that fired, as stable rule ids — never a generic failure.
   * Empty until the policy engine evaluates. */
  unmetRules: string[];
  message?: string;
}

/** One pod behind an environment (GET /environments/{name}/workload). */
export interface WorkloadPod {
  name: string;
  phase: string;
  ready: boolean;
  restarts: number;
  node?: string;
  startedAt?: string;
  /** Why the pod is not serving — a crash loop, a pull failure. */
  message?: string;
}

/** One firing of a scheduled job. `name` is the Job that was the run, and it
 * is also what the log store keys the run's output by — which is why a run
 * that the cluster has long since collected still has readable logs. */
export interface ProcessRun {
  name: string;
  phase: string;
  startedAt?: string;
  finishedAt?: string;
  durationSeconds?: number;
  message?: string;
}

/** One of a project's processes besides its web process, as one environment
 * runs it: a worker (continuous, never addressed) or a scheduled job.
 *
 * `healthy` is the platform's own verdict rather than something the dashboard
 * derives, so that this screen and the CLI cannot disagree about what a red
 * dot means. `suspended` is a process the environment declares and does not
 * run — a preview whose process was not opted in — which is listed with its
 * reason rather than left out. */
export interface Process {
  name: string;
  type: string;
  command?: string[];
  args?: string[];
  schedule?: string;
  concurrencyPolicy?: string;
  timeout?: string;
  replicas?: number;
  readyReplicas?: number;
  /** A worker two of which must never run at once: deploying it stops the old
   * pod before starting the new one, where a rolling update would overlap
   * them. Never set on a scheduled job, whose answer to the same question is
   * its concurrency policy. */
  singleton?: boolean;
  cpu?: string;
  memory?: string;
  /** The worker's health check, timings resolved. Absent for a worker that
   * declared none: unlike the web process, a worker is probed only where it
   * asked to be. */
  health?: Health;
  workload?: string;
  suspended?: boolean;
  reason?: string;
  active?: number;
  lastRun?: ProcessRun;
  lastFailure?: ProcessRun;
  healthy: boolean;
}

/** What an environment is actually running, as opposed to what it was asked
 * to run: the Deployment's replica counts, its pods and their restarts. */
export interface Workload {
  environment: string;
  namespace: string;
  /** Empty when nothing has been materialized yet; `message` says why. */
  deployment?: string;
  image?: string;
  replicas: { desired: number; ready: number; available: number; updated: number };
  restarts: number;
  startedAt?: string;
  resources?: { cpuRequest?: string; cpuLimit?: string; memoryRequest?: string; memoryLimit?: string };
  pods?: WorkloadPod[];
  message?: string;
}

/** One Kubernetes object the operator materialized for an environment
 * (GET /environments/{name}/objects) — operator mode's inspect surface. */
export interface MaterializedObject {
  kind: string;
  apiVersion: string;
  name: string;
  namespace: string;
  present: boolean;
  manifest?: Record<string, unknown>;
  message?: string;
}

export interface EnvironmentObjects {
  environment: string;
  namespace: string;
  objects: MaterializedObject[];
}

/** One platform workload out of the operator's component survey. */
export interface ComponentStatus {
  name: string;
  kind: string;
  healthy: boolean;
  available: number;
  desired: number;
  message?: string;
}

/** The platform as it is running (GET /status) — the status bar's request.
 *
 * It is the one payload that varies by role. The cluster's name and the build
 * queue are everybody's ("why is my build waiting" is a developer's question);
 * the tunnel, the gateway, the component survey and the node counts are the
 * operator's, and they are **absent** for a member rather than zeroed — so
 * `tunnel === undefined` means "you are not allowed to know" and
 * `tunnel.enabled === false` means "no tunnel is configured". */
export interface PlatformStatus {
  cluster: { name?: string; nodes?: number; readyNodes?: number; message?: string };
  tunnel?: { enabled: boolean; connected: boolean; message?: string };
  builds: {
    running: number;
    capacity: number;
    queued: number;
    /** How long the build waiting longest has been waiting. Absent when none is. */
    oldestWaitSeconds?: number;
    /** The queued builds themselves, longest wait first — narrowed to the
     * caller's own projects, while the counts above are the whole gate's. An
     * operator holds every project, so theirs is the whole queue. */
    waiting?: { name: string; project: string; queuedAt: string; waitSeconds: number }[];
  };
  gateway?: { address?: string; programmed: boolean; message?: string };
  components?: ComponentStatus[];
}

/** Who the caller is, as the API describes them to themselves (GET /me). It
 * says nothing about anyone else, so any valid token may ask for it. */
export interface Me {
  subject: string;
  email?: string;
  name?: string;
  /** "operator" or "member". */
  platformRole: string;
}

/**
 * A connection, in either of the two shapes `GET /connections` answers with.
 *
 * An operator gets the connection: what it is, when it was made and what its
 * conditions say. Everybody else gets the picker's shape — a name, what it
 * can back, and whether the platform has it working — because a project needs
 * a git source and a registry to exist at all, and a member who cannot see
 * that any connection exists cannot create a project.
 *
 * The two are one type here because one picker renders both. Everything the
 * operator's shape adds is therefore optional, and `ui/src/lib/connections`
 * is where the difference is read rather than in the screens.
 */
export interface Connection {
  name: string;
  /** Operator's shape only. */
  provider?: string;
  capabilities?: string[];
  /** Picker's shape only: the platform reached the provider and the provider
   * accepted the credential. The operator's shape carries the same verdict as
   * the `CredentialsValid` condition instead. */
  ready?: boolean;
  /** Operator's shape only. */
  createdAt?: string;
  /** Operator's shape only — a condition's message is the provider's own
   * words, and those are the operator's business. */
  conditions?: Condition[];
}

/** One account's role on a project, as `GET /projects/{name}/members` lists
 * them. The subject is the issuer's `sub` and the one thing a write addresses
 * a member by; the address is informational, exactly as it is on the object —
 * it is what makes a list of opaque strings render as people. */
export interface Member {
  subject: string;
  email?: string;
  role: string;
  /**
   * "account" or "key" — what kind of member this grant is about.
   *
   * A CI key is a member of exactly one project (docs/AUTH.md, "Machine
   * accounts"), so its grant is listed here with everybody else's. The API
   * derives this from the machine account's address and calls it a display
   * rule: no access decision anywhere reads it, and a role is resolved from
   * the subject alone. The dashboard uses it for exactly that — rendering a
   * key as a key rather than as a stranger with an odd address.
   */
  kind?: string;
  /** The key's own name, for a member whose `kind` is "key". */
  name?: string;
}

/** What `POST /projects/{name}/members` takes. Exactly one of the two ways of
 * naming somebody: an address the platform resolves at the identity provider,
 * or a subject taken as given — a machine account, or an installation whose
 * issuer serves no directory. */
export interface NewMember {
  email?: string;
  subject?: string;
  role: string;
}

/**
 * One CI key on a project, as `GET /projects/{name}/keys` lists it.
 *
 * There is no key value here and there never is one again: a key is stored
 * hashed at the identity provider, so a listing carries only the `prefix` —
 * enough to tell two keys apart and useless as a credential. `subject` is the
 * machine account created to own the key, which is what the project's grant
 * actually names.
 *
 * `role` is read from the project's grant rather than from anything stored on
 * the key, so an **absent** role is a key whose grant has been removed: it can
 * still authenticate and can do nothing, and the listing says so rather than
 * hiding it.
 */
export interface ProjectKey {
  name: string;
  project: string;
  subject: string;
  email?: string;
  prefix: string;
  created: string;
  lastUsed?: string;
  role?: string;
}

/**
 * What `POST /projects/{name}/keys` answers — the listing's shape plus the key
 * itself, which appears in this response and in no other.
 */
export interface IssuedKey extends ProjectKey {
  key: string;
}

/** What `POST /projects/{name}/keys` takes. `role` defaults to `developer`
 * and may also be `viewer`; `admin` is refused, because a credential in a
 * build pipeline that can mint its own successors is one nobody can account
 * for. */
export interface NewKey {
  name: string;
  role?: string;
}

/** A credential as the API accepts one — a token, or a username and password,
 * depending on the provider. Write-only: the API never reads it back. */
export interface ConnectionCredential {
  token?: string;
  username?: string;
  password?: string;
}

export interface NewConnection {
  name: string;
  provider: string;
  config?: Record<string, unknown>;
  /** Every provider but cnpg requires one. CloudNativePG provisions with the
   * operator's own identity, so there is nothing to store — and a credential
   * sent for it is refused rather than kept and never read. */
  credential?: ConnectionCredential;
}

/** What PATCH /connections/{name} accepts: a new config, a rotated
 * credential, or both. */
export interface ConnectionChanges {
  config?: Record<string, unknown>;
  credential?: ConnectionCredential;
}

/** What POST /connections/test takes: a credential to try before it is
 * stored, or the name of a connection whose stored credential should be
 * re-checked. Nothing is written either way. */
export interface ConnectionTestRequest {
  name?: string;
  provider?: string;
  config?: Record<string, unknown>;
  credential?: ConnectionCredential;
}

/** The probe's verdict, in the same parts the Connected and CredentialsValid
 * conditions are written from — a provider that is down and a credential that
 * is wrong are different answers. */
export interface ConnectionTestResult {
  reachable: boolean;
  credentialChecked: boolean;
  credentialValid: boolean;
  message: string;
  /** What an accepted credential still cannot do — a token that registers
   * webhooks but could not post a commit status. The connection works;
   * something the platform wants would not. */
  warnings?: string[];
}

/**
 * One repository a connection's credential can see, as
 * `GET /connections/{name}/repositories` lists it.
 *
 * `fullName` is owner/name — the only field a project is actually created
 * with. `defaultBranch` is what the production branch should start as, and
 * the other two are there so two similarly-named repositories can be told
 * apart in a list.
 */
export interface Repository {
  fullName: string;
  defaultBranch?: string;
  private?: boolean;
  description?: string;
}

/**
 * What a connection can see, and whether it could be asked at all.
 *
 * `supported` is the field that matters: a provider the platform cannot
 * enumerate is not a failure, it is a field that has to be typed into, and
 * the API says so with a 200 rather than an error. `truncated` says the
 * listing was cut short at the provider — a repository missing from a picker
 * is otherwise indistinguishable from one that does not exist.
 */
export interface ConnectionRepositories {
  provider: string;
  supported: boolean;
  items: Repository[];
  truncated?: boolean;
  /** Why there is no listing, in words a form can show. */
  message?: string;
}

/**
 * What the build context currently is, asked about before the project exists.
 * Every field is the value the form holds, so changing the root directory and
 * asking again is the whole of correcting it.
 */
export interface DetectRequest {
  repo: string;
  ref?: string;
  rootDirectory?: string;
  dockerfilePath?: string;
}

/**
 * What the repository looks like to the platform.
 *
 * `detected` false is not an error: it is the answer, and the answer the form
 * exists to deliver before a build spends five minutes reaching it. `message`
 * says why in words a form can show, whether or not anything was detected,
 * and `files` is what the verdict was reached from.
 */
export interface Detection {
  detected: boolean;
  framework?: string;
  strategy?: string;
  port?: number;
  ref?: string;
  rootDirectory?: string;
  dockerfile: boolean;
  files?: string[];
  /** The repository itself could not be read: it is not there, or the
   * connection's credential cannot see it. The one `detected: false` that
   * correcting the build context will not change — every provider answers the
   * same 404 for a repository a token may not know about as for a path that is
   * not in one, so this used to arrive headed as a missing directory. */
  unreadable?: boolean;
  message?: string;
}

/** The DNS change that proves ownership of a custom domain, exactly as the
 * user has to type it into their zone. Either record satisfies the check;
 * the CNAME also routes the hostname at the platform. */
export interface DomainVerification {
  txtRecord: string;
  txtValue: string;
  cnameTarget?: string;
}

export interface Domain {
  name: string;
  hostname: string;
  environment: string;
  /** The spec's own TLS mode; empty inherits the platform's. */
  tls?: string;
  /** The mode actually in effect, as the operator resolved it. */
  effectiveTLS?: string;
  verified: boolean;
  verification?: DomainVerification;
  createdAt: string;
  conditions?: Condition[];
}

/** What POST /domains takes: the name is derived from the hostname when
 * absent, and tls empty inherits the platform's mode. */
export interface NewDomain {
  name?: string;
  hostname: string;
  environment: string;
  tls?: string;
}

/** The database a postgres claim asked for: which Postgres, what it has to be
 * able to do, and how much room it gets. All four are set when the database is
 * created and are not changed under a running one — asking for a different
 * database means asking for a different database. */
export interface ClaimPostgres {
  version?: string;
  extensions?: string[];
  storageSize?: string;
  storageClass?: string;
}

export interface Claim {
  name: string;
  project: string;
  /** The claim's declared sensitivity class — never above its project's,
   * which the create refuses. Absent means unclassified. */
  dataClass?: string;
  /** The provider's declaration of what the provisioned data derives from:
   * "production", "masked" or "synthetic". Absent means the provider
   * declared nothing — shown as undeclared, treated by policy as the worst
   * case. A branch of a production database is production. */
  dataProvenance?: string;
  /** Where the provider reported the resource actually is (a Neon region
   * id). Reported, not declared; absent means it reported nothing. */
  residency?: string;
  /** Empty for an oidcClient claim: the platform's own identity provider
   * registers the client, and there is no Connection in front of it. */
  connection: string;
  type: string;
  phase?: string;
  secret?: string;
  /** Retain (default) keeps the provisioned database when the claim is
   * deleted; Delete destroys it and its data. An oidcClient claim has none:
   * its client is always deregistered. */
  deletionPolicy?: string;
  previewBranching: boolean;
  /** What a postgres claim asked the database itself to be. Absent when it
   * asked for nothing in particular, which is most of them. Whether it was
   * granted is the phase and the conditions: a claim asking for an extension
   * no image can supply is Failed, with the refusal as the message. */
  postgres?: ClaimPostgres;
  createdAt: string;
  conditions?: Condition[];
  /** What an oidcClient claim's client currently accepts as a callback. The
   * operator keeps it in step with the project's environment URLs, so this is
   * where a preview's callback shows up after it is deployed. */
  redirectURIs?: string[];
  callbackPaths?: string[];
  scopes?: string[];
}

export interface NewClaim {
  name: string;
  project: string;
  connection: string;
  type: string;
  previewBranching?: boolean;
  deletionPolicy?: string;
  /** postgres only: the major version, the extensions the application needs,
   * and the volume behind the database. */
  postgres?: ClaimPostgres;
  /** Classify the data the resource will hold. May not exceed the project's
   * class; refused in an unclassified project (classify the project first). */
  dataClass?: string;
  /** oidcClient only: appended to every environment URL of the project. */
  callbackPaths?: string[];
  /** oidcClient only: registered verbatim, for addresses the platform does
   * not own — a developer's localhost, typically. */
  redirectURIs?: string[];
  /** oidcClient only: what the client may ask the issuer for. */
  scopes?: string[];
}

export interface Settings {
  baseDomain: string;
  apiExternalURL?: string;
  gatewayClassName?: string;
  authEnabled: boolean;
  authHost?: string;
  buildStrategy?: string;
  buildConcurrency?: number;
  /** Releases a project keeps; 0 keeps every one. Always sent, since 0 is a
   * setting rather than an absent value. */
  releaseRetention: number;
  logRetentionDays?: number;
  gatewayAddress?: string;
  /** `spec.ingress.publicAddresses`: where the internet reaches the platform,
   * when a router forwards to the Gateway from an address the cluster never
   * sees. Empty in the ordinary case where the Gateway's address is public. */
  publicAddresses?: string[];
  /**
   * `spec.access.operators`: the list every `operator` requirement in the
   * policy table is resolved against.
   *
   * **Three states, and they are three.** `null` is nobody having ever said
   * who the operators are, which the reconciler will seed from the accounts
   * that exist; `[]` is somebody having narrowed it to nobody; a list is what
   * it says. The API carries the field with no `omitempty` for exactly that
   * reason, so `null` and `[]` arrive as themselves — and `undefined` is only
   * an API too old to serve the list at all, which is a fourth thing and not
   * one of the three. `operatorsState` in `./operators` is where that is read.
   */
  operators?: Operator[] | null;
  conditions?: Condition[];
}

/** One of the platform's operators, as `GET /settings` lists them: the
 * issuer's `sub`, and the address that makes a list of opaque strings read. */
export interface Operator {
  subject: string;
  email?: string;
}

/** How `PATCH /settings` names an operator — exactly one of the two ways, the
 * same two a membership write takes: an `email` the platform resolves at the
 * identity provider before anything is written, or a `subject` taken as
 * given. */
export interface OperatorWrite {
  email?: string;
  subject?: string;
}

/** One attempt to upgrade the platform itself. */
export interface PlatformUpdate {
  name: string;
  version: string;
  phase?: string;
  fromVersion?: string;
  message?: string;
  requestedBy?: string;
  startedAt?: string;
  completedAt?: string;
  conditions?: Condition[];
}

/** The platform's own version, what it can move to, and what it has tried. */
export interface PlatformUpdates {
  /** Whether the chart was installed with `selfUpdate.enabled`. */
  enabled: boolean;
  /** Why not, when it was not — including how to turn it on. */
  reason?: string;
  currentVersion: string;
  latestVersion?: string;
  available: boolean;
  /**
   * What this installation would actually accept, newest first. It is not
   * simply everything newer than `currentVersion`: pre-1.0 a minor crossing
   * carries the breaking changes, so those are left out unless `allowMinor`.
   */
  upgradableTo?: string[];
  allowMinor: boolean;
  /**
   * When the published versions were last read from the registry. The listing
   * is cached for an hour, so a re-check that turns up nothing new is only
   * legible next to the time it was taken.
   */
  checkedAt?: string;
  /** Why the published versions could not be listed — usually no egress. */
  discoveryError?: string;
  items: PlatformUpdate[];
}

export interface LogLine {
  timestamp: string;
  source: string;
  project: string;
  environment: string;
  build: string;
  pod: string;
  container: string;
  stream: string;
  /** Best-effort severity the collector parsed out of the line; "" when unknown. */
  level?: string;
  message: string;
  /** The line's own trace and span, lifted out of its structured fields by the
   * collector. A line that carries one can offer the whole request. */
  traceId?: string;
  spanId?: string;
  /** The line's own structured fields, when it was JSON the collector could flatten. */
  fields?: Record<string, string>;
}

/**
 * What an observability question is asked over. `q` is Kitchen's log query
 * language and the front door; `where` is a raw ClickHouse expression, the
 * escape hatch. Given both, they compose with AND — which is how the view
 * scopes the cluster's own pods out of an operator's hand-written SQL.
 */
export interface LogSelection {
  q?: string;
  where?: string;
  since?: string;
  until?: string;
}

/** One bar of the log histogram. */
export interface LogBucket {
  start: string;
  count: number;
  errors: number;
  warnings: number;
}

/** The shape of a window (GET /logs/histogram), empty buckets included. */
export interface LogHistogram {
  start: string;
  end: string;
  bucketSeconds: number;
  buckets: LogBucket[];
  total: number;
}

/** One value a field takes in the current selection, and how often. */
export interface LogFacetValue {
  value: string;
  count: number;
}

/** One field's distinct values over the window (GET /logs/facets). */
export interface LogFacet {
  field: string;
  values: LogFacetValue[];
  distinct: number;
}

/** One message template the selection's lines collapse into. */
export interface LogPattern {
  pattern: string;
  count: number;
  level?: string;
  sample: string;
  firstSeen: string;
  lastSeen: string;
}

/** A question about the logs that was worth keeping (GET /logs/saved). It is
 * the observability view's own URL state, named — so it can be found by
 * someone who was never sent the link. */
export interface SavedQuery {
  name: string;
  title: string;
  description?: string;
  query?: string;
  where?: string;
  /** The window it is asked over, relative to whenever it is opened. 0 means
   * everything retained. */
  rangeMinutes: number;
  limit?: number;
  view?: "lines" | "patterns";
  includeCluster?: boolean;
  savedBy?: string;
  createdAt: string;
}

/** What POST /logs/saved accepts. */
export interface NewSavedQuery {
  title: string;
  description?: string;
  query?: string;
  where?: string;
  rangeMinutes?: number;
  limit?: number;
  view?: "lines" | "patterns";
  includeCluster?: boolean;
}

/** One bucket of an environment's resource history. CPU and memory are summed
 * across the environment's containers; `replicas` is how many distinct pods
 * reported in the bucket, which is the only way to see one idle to zero and
 * come back. */
export interface ResourcePoint {
  start: string;
  cpuCores: number;
  cpuPeakCores: number;
  memoryBytes: number;
  memoryPeakBytes: number;
  replicas: number;
  restarts: number;
  oomKills: number;
}

/** What an environment has been using (GET /environments/{name}/metrics), as
 * opposed to what it is using — which is the workload endpoint. */
export interface ResourceSeries {
  start: string;
  end: string;
  bucketSeconds: number;
  points: ResourcePoint[];
  cpuLimitCores: number;
  memoryLimitBytes: number;
  restarts: number;
  oomKills: number;
  /** Whether the five-minute rollup answered — why a wide window is coarser
   * than the resolution that was asked for. */
  rollup: boolean;
}

/**
 * Whether the platform's edge publishes this environment, which is what tells
 * "nothing reaches this environment" from "nothing was asked of it in this
 * window" — both of which are zeroes. `routed` is false only where the platform
 * is *sure*; a route it could not read leaves it true with a `message` saying
 * the check did not happen.
 */
export interface EdgeStatus {
  routed: boolean;
  message?: string;
}

/** The golden-signal header (GET /environments/{name}/requests/summary).
 * `since` and `until` are the window that was *answered* — these numbers come
 * off indivisible buckets, so the start is snapped to `rollup`'s resolution. */
export interface RequestSummary {
  since: string;
  until: string;
  requests: number;
  requestsPerSecond: number;
  /** Answers of 500 and above. A 4xx is the caller's fault and is not counted. */
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
  /** Which rollup answered — `1m` or `1h`. */
  rollup: string;
  environment: string;
  edge: EdgeStatus;
}

/** One bucket of the request charts. Every bucket in the window is present,
 * empty ones included: a gap is an environment that served nothing. */
export interface RequestPoint {
  start: string;
  requests: number;
  requestsPerSecond: number;
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
}

/** The same signals over time (GET /environments/{name}/requests/series). */
export interface RequestSeries {
  start: string;
  end: string;
  bucketSeconds: number;
  points: RequestPoint[];
  rollup: string;
  environment: string;
  edge: EdgeStatus;
}

/** One row of the route table: a route template's share of the window. The
 * set of templates is bounded at ingest, which is what makes this finite. */
export interface RequestRoute {
  route: string;
  requests: number;
  requestsPerSecond: number;
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
}

/** How the route table is ordered. The sort is a query rather than a
 * presentation detail: it decides which rows survive the limit. */
export type RouteSort = "requests" | "errors" | "errorRate" | "p95";

/**
 * One request the edge served. It carries project and environment and never a
 * build or a release — the edge routes to a Service, not to a pod — and `path`
 * has already lost its query string, which is never stored.
 */
export interface RequestRow {
  timestamp: string;
  project?: string;
  environment?: string;
  host?: string;
  method: string;
  path: string;
  /** What the path was templated to; the raw path is kept beside it so a
   * mis-templated route stays diagnosable. */
  route?: string;
  status: number;
  durationMs: number;
  protocol?: string;
  /** Which vantage point observed it — `gateway` today. */
  source?: string;
  /** Reserved: filled only when the edge can carry trace context. */
  traceId?: string;
}

/** The window and route filter every request read shares. */
export interface RequestWindow {
  since?: string;
  until?: string;
  /** One route template, spelled as the route table spells it — what clicking
   * a row filters the rest by. */
  route?: string;
}

/** What the raw listing is filtered by on top of the window. */
export interface RequestListQuery extends RequestWindow {
  method?: string;
  /** A *class* of answer — `5xx`. One exact code is not offered. */
  status?: string;
  /** Keep only what the signals count as an error (500 and above). */
  errors?: boolean;
  limit?: number;
}

/** One Warning event the cluster raised (inside the crash report). */
export interface K8sEvent {
  timestamp: string;
  project?: string;
  environment?: string;
  namespace?: string;
  kind?: string;
  name?: string;
  reason: string;
  message: string;
  count: number;
  node?: string;
}

/** The termination itself, in the words the kubelet reported it in. */
export interface CrashDetail {
  pod: string;
  container: string;
  /** The kubelet's reason — OOMKilled, Error, ContainerStatusUnknown. */
  reason?: string;
  /** Its own field beside `reason`: the kernel killing a container for using
   * too much memory and a container crashing are different problems with
   * different fixes and the same exit code. */
  oomKilled: boolean;
  exitCode: number;
  signal?: number;
  message?: string;
  startedAt?: string;
  finishedAt: string;
  restarts: number;
  /** Why the container is not running now — the CrashLoopBackOff and its
   * message. Empty for one that restarted and is serving again. */
  waiting?: string;
  /** The termination ended the run *before* the current one, which is the
   * ordinary shape of a crash loop. */
  previous: boolean;
}

/** Everything the platform knows about the crash, assembled: the sections do
 * not all use the whole span, and each is what it is for. */
export interface CrashReport {
  crash: CrashDetail;
  since: string;
  until: string;
  /** The dead container's own last lines, oldest first, up to the instant. */
  logs: LogLine[];
  /** Usage leading up to the termination, against the limit the release set. */
  resources: ResourceSeries;
  /** The cluster's Warnings around the crash — they run past it, because a
   * crash loop keeps announcing itself. */
  events: K8sEvent[];
  /** What the edge served in the ±30 seconds around the instant. */
  requests: RequestRow[];
}

/** GET /environments/{name}/diagnostics. Nothing having crashed is an answer,
 * not an empty report: `crashed` false comes with the sentence that says so. */
export interface Diagnostics {
  environment: string;
  namespace: string;
  crashed: boolean;
  message?: string;
  /** Every restart the environment's pods carry right now, off the API server. */
  restarts: number;
  report?: CrashReport;
}

/** One trace as a list entry (GET /traces): what it was, how long it took end
 * to end, and whether anything in it failed. */
export interface Trace {
  traceId: string;
  timestamp: string;
  name: string;
  service: string;
  project?: string;
  environment?: string;
  durationMs: number;
  spans: number;
  errors: number;
  /** How many distinct services the trace touched — one process, or a
   * conversation. */
  services: number;
  httpStatus?: number;
}

/** One operation inside a trace (GET /traces/{traceId}). */
export interface Span {
  timestamp: string;
  traceId: string;
  spanId: string;
  parentSpanId?: string;
  name: string;
  kind?: string;
  service: string;
  project?: string;
  environment?: string;
  durationMs: number;
  statusCode?: string;
  statusMessage?: string;
  httpStatus?: number;
  attributes?: Record<string, string>;
  resource?: Record<string, string>;
}

/** A whole trace: its spans in start order, which is how a waterfall is drawn. */
export interface TraceDetail {
  traceId: string;
  spans: Span[];
}

/** One entry of the platform's activity feed (GET /events). */
export interface PlatformEvent {
  timestamp: string;
  type: string;
  project?: string;
  environment?: string;
  build?: string;
  release?: string;
  claim?: string;
  /** One of a project's workers or scheduled jobs, and — for a scheduled
   * one — the Job that was the run. `run` is what the log store keys that
   * firing's output by. */
  process?: string;
  run?: string;
  message: string;
  actor?: string;
  value?: number;
}

/** Per-project 24h traffic inside the metrics overview. */
export interface ProjectTraffic {
  project: string;
  requests24h: number;
  errors5xx24h: number;
  p95Ms: number;
  requestsPerHour: number[];
}

/** The dashboard's numbers, pre-aggregated (GET /metrics/overview). */
export interface MetricsOverview {
  deploys7d: number;
  deploysPerDay: number[];
  medianBuildSeconds: number;
  requests24h: number;
  errorRate24h: number;
  p95Ms24h: number;
  requestsPerHour: number[];
  errorsPerHour: number[];
  p95MsPerHour: number[];
  logLines24h: number;
  logLinesPerHour: number[];
  storeBytes: number;
  storeRowsPerSecond: number;
  projects?: ProjectTraffic[];
}

/** One aggregated edge of the service map (GET /traffic). */
export interface TrafficEdge {
  source: string;
  sourceNamespace?: string;
  destination: string;
  destinationNamespace?: string;
  protocol: string;
  flows: number;
  rps: number;
  errors: number;
  drops: number;
  p95Ms: number;
}

/**
 * How much of a hurry the reader is in. `unknown` is a rule that could not be
 * evaluated because an input was unreadable — deliberately neither `info` nor
 * `critical`, and never to be rendered as health.
 */
export type Severity = "critical" | "warning" | "unknown" | "info";

/** What sort of thing a finding is about, which decides which of `FindingScope`'s
 * fields carry anything. */
export type ScopeKind =
  | "platform"
  | "project"
  | "environment"
  | "workload"
  | "node"
  | "volume"
  | "domain"
  | "build";

/** The subject of a finding. A scope sets the fields that identify it and no
 * more — the joined non-empty ones are the tail of its fingerprint. */
export interface FindingScope {
  kind: ScopeKind;
  project?: string;
  environment?: string;
  /** Set only where it is not derivable from `project` — the platform's own. */
  namespace?: string;
  node?: string;
  name?: string;
}

/** One firing condition: what is wrong, where, since when, and where to look. */
export interface Finding {
  /** The rule that produced it — `workload.crashloop`. */
  signal: string;
  severity: Severity;
  scope: FindingScope;
  /** Stable for the same underlying condition across evaluations, which is what
   * will let a later release diff rounds instead of re-announcing. */
  fingerprint: string;
  /** The short human sentence: "crash-looping". */
  title: string;
  /** The numbers, and the suspect where a rule names one. Its first clause is
   * the headline number, so a strip can render `title (first clause)`. */
  detail: string;
  since: string;
  /** A dashboard path to the screen that shows the numbers behind it. */
  evidence: string;
}

/** One input the evaluation could not read, named once with the reason. */
export interface InputFailure {
  input: string;
  reason: string;
}

/** A round by severity. `unknown` is deliberately absent: a rule that could not
 * be evaluated is in `unreadable`, not in the count of problems. */
export interface SignalCounts {
  critical: number;
  warning: number;
  info: number;
}

/**
 * One evaluated round (GET /platform/signals, GET /environments/{name}/signals).
 *
 * `unreadable` is the field that keeps an empty `items` honest: no findings
 * because nothing is wrong, and no findings because nothing could be read, are
 * different answers, and the API never conflates them.
 */
export interface SignalsAnswer {
  items: Finding[];
  counts: SignalCounts;
  unreadable?: InputFailure[];
  /** When the snapshot was taken — findings are ephemeral, so this is how fresh
   * the answer is. */
  evaluatedAt: string;
  project?: string;
  environment?: string;
}

/** One bucket of a platform series. `value` is null for a bucket nothing was
 * observed in, which is deliberately not zero: a scrape that did not happen is
 * not a machine that was idle. */
export interface UsagePoint {
  start: string;
  value: number | null;
}

/** One mounted filesystem's fill, as fractions in 0..1. */
export interface NodeFilesystem {
  mountPoint: string;
  device?: string;
  capacityBytes?: number;
  used?: UsagePoint[];
  /** The newest bucket that was actually measured. */
  latest?: number;
}

/** One node's saturation over the recent window. */
export interface NodeUsage {
  bucketSeconds: number;
  cpu?: UsagePoint[];
  memory?: UsagePoint[];
  filesystems?: NodeFilesystem[];
}

export interface NodeCondition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  since: string;
}

/** What the node says it has to give, in the units it said it in. */
export interface NodeCapacity {
  cpu?: string;
  memory?: string;
  pods?: string;
}

/**
 * When the store last received anything from this node's collector.
 *
 * `lastSeen` absent with `silent` true is a node that said nothing inside the
 * lookback. Both absent is the freshness read not having happened at all —
 * which is neither fresh nor silent, and must not render as either.
 */
export interface NodeTelemetry {
  lastSeen?: string;
  silent: boolean;
  ageSeconds?: number;
}

export interface PlatformNode {
  name: string;
  ready: boolean;
  /** The cordon, which is a decision somebody took rather than a fault. */
  schedulable: boolean;
  roles?: string[];
  kubeletVersion?: string;
  createdAt: string;
  conditions?: NodeCondition[];
  pods: number;
  allocatable: NodeCapacity;
  telemetry: NodeTelemetry;
  usage?: NodeUsage;
}

export interface PlatformNodes {
  items: PlatformNode[];
  nodes: number;
  readyNodes: number;
  /** A measured zero only when `telemetryMessage` is empty. */
  silentNodes: number;
  /** Why freshness is missing, so a store nobody could reach does not make the
   * whole cluster look silent. */
  telemetryMessage?: string;
  /** The same, about the saturation series: an unmeasured node and an idle one
   * must not draw the same chart. */
  usageMessage?: string;
}

/** One FailedCreate, as the cluster worded it — why a workload has no pods. */
export interface AdmissionRefusal {
  reason: string;
  message: string;
  count: number;
  at: string;
  /** What the message betrays where it betrays anything — Pod Security is the
   * one this screen exists for. */
  suspect?: string;
}

export interface PlatformWorkload {
  kind: string;
  namespace: string;
  name: string;
  project?: string;
  environment?: string;
  /** Names a platform workload. A workload is this or a project's, never both. */
  component?: string;
  desired: number;
  ready: number;
  available: number;
  /** How many pods exist, which the replica counts cannot tell you: zero
   * available is pods that are failing *or* pods that were never created. */
  pods: number;
  healthy: boolean;
  admission?: AdmissionRefusal;
}

export interface PlatformPod {
  namespace: string;
  name: string;
  node?: string;
  /** The object a reader recognises: a Deployment rather than its ReplicaSet. */
  workload?: string;
  project?: string;
  environment?: string;
  phase: string;
  ready: boolean;
  restarts: number;
  oomKilled: boolean;
  startedAt?: string;
  message?: string;
}

export interface PodTotals {
  pods: number;
  running: number;
  pending: number;
  failed: number;
  notReady: number;
  restarts: number;
  oomKills: number;
}

export interface PlatformWorkloads {
  items: PlatformWorkload[];
  pods: PlatformPod[];
  workloads: number;
  unhealthy: number;
  /** How many want pods and have none — the one number a pod listing can never
   * contain. */
  withoutPods: number;
  totals: PodTotals;
  /** The pod listing was cut at the limit. It is sorted worst first, so the cut
   * never hides a problem. */
  truncated: boolean;
  /** Why the admission column is empty, when it is. */
  eventsMessage?: string;
}

/** The edge's headline: everything that entered the platform in the window. */
export interface PlatformRequests {
  since: string;
  until: string;
  requests: number;
  requestsPerSecond: number;
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
  /** The part of `requests` that asked for a host the platform never published. */
  unrouted: number;
  rollup: string;
}

/** One row of an edge ranking — a route, or a host. */
export interface EdgeEntry {
  key: string;
  project?: string;
  environment?: string;
  requests: number;
  requestsPerSecond: number;
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
}

/** A host that reached the edge which the platform never published.
 * `firstSeen`/`lastSeen` are what separate a scanner from a stale DNS record. */
export interface UnroutedHost {
  host: string;
  requests: number;
  requestsPerSecond: number;
  firstSeen: string;
  lastSeen: string;
}

export interface EdgeListener {
  name: string;
  port: number;
  protocol: string;
  attachedRoutes: number;
  programmed: boolean;
  message?: string;
}

export interface EdgeGateway {
  namespace: string;
  name: string;
  class?: string;
  addresses?: string[];
  programmed: boolean;
  accepted: boolean;
  message?: string;
  listeners?: EdgeListener[];
}

export interface EdgeTunnel {
  name: string;
  namespace: string;
  desired: number;
  ready: number;
  available: number;
  restarts: number;
  healthy: boolean;
  message?: string;
}

export interface Certificate {
  namespace: string;
  name: string;
  dnsNames?: string[];
  ready: boolean;
  notAfter?: string;
  daysToExpiry?: number;
  renewalTime?: string;
  /** For a stuck ACME order, the error the CA returned, verbatim — the one
   * string on the screen that says what to fix. A healthy certificate has none. */
  message?: string;
  /** Set only while a renewal is in progress, which is the only place a renewal
   * that keeps failing reports itself: `ready` stays true on the old one. */
  issuing?: string;
}

export interface CertificateTable {
  items: Certificate[];
  /** cert-manager not being installed is a supported configuration, and answers
   * an empty table with a message rather than an error. */
  message?: string;
}

export interface PlatformEdge {
  requests: PlatformRequests;
  topRoutes: EdgeEntry[];
  worstRoutes: EdgeEntry[];
  topHosts: EdgeEntry[];
  worstHosts: EdgeEntry[];
  latencyLeaders: EdgeEntry[];
  unrouted: UnroutedHost[];
  gateways: EdgeGateway[];
  tunnel?: EdgeTunnel;
  certificates: CertificateTable;
  /** Why the Gateway list is empty, absent when the platform genuinely has no
   * Gateway. The two readings of an empty list are not the same answer: "no
   * Gateway" means nothing this platform publishes is reachable, and a list
   * that could not be read proves none of it. */
  gatewayMessage?: string;
  /** The traffic half needs the store; the edge's own objects do not. */
  trafficMessage?: string;
}

/** How full one volume is, as the kubelet measured it. */
export interface VolumeUsage {
  usedBytes: number;
  capacityBytes: number;
  usedFraction: number;
}

/** One PersistentVolumeClaim and what mounts it. Called a volume throughout,
 * because `/claims` already means a `ResourceClaim` in this API. */
export interface PlatformVolume {
  namespace: string;
  name: string;
  project?: string;
  phase: string;
  bound: boolean;
  storageClass?: string;
  volume?: string;
  requested?: string;
  capacity?: string;
  pods?: string[];
  usage?: VolumeUsage;
  /** Why an unbound claim is unbound — including the missing-default-
   * StorageClass install the prerequisites warn about. */
  message?: string;
}

/** The telemetry store's own state. */
export interface StoreHealth {
  bytesOnDisk: number;
  /** Zero for an external store: the platform does not own that disk. */
  capacityBytes?: number;
  usedFraction?: number;
  claim?: string;
  rowsPerSecond: number;
  /** The longest telemetry class's retention: the horizon past which the store
   * holds nothing at all. The whole model, class by class, is
   * `api.platformRetention()`. */
  retentionDays?: number;
  message?: string;
}

/** What the flow follower counted losing, over its trailing window. */
export interface FlowLoss {
  events: number;
  notices: number;
  reconnects: number;
  windowSeconds: number;
  latest?: string;
  /** Stated rather than left to be inferred from three zeroes. */
  lossless: boolean;
}

export interface PlatformStorage {
  items: PlatformVolume[];
  volumes: number;
  unbound: number;
  /** A measured zero only when `usageMessage` is empty. */
  filling: number;
  store: StoreHealth;
  flows?: FlowLoss;
  usageMessage?: string;
}

/** One class of what the platform keeps: the rule in force, where the number
 * came from, and what the last retention sweep measured. `oldest` is the claim
 * retention actually makes — nothing of this class is older than this. */
export interface RetentionClass {
  class: string;
  label: string;
  description: string;
  days: number;
  /** `retention` when somebody set this class, otherwise the legacy field it
   * inherits from. */
  source: string;
  enforced: boolean;
  rows?: number;
  oldest?: string;
  expired?: number;
  removed?: number;
  message?: string;
}

/** The written decision behind an audit retention under the documented floor.
 * It is not a credential and is read back whole: the whole value of the field
 * is that somebody can see who signed off on keeping less evidence. */
export interface RetentionOverride {
  reason: string;
  approvedBy: string;
}

export interface PlatformRetention {
  classes: RetentionClass[];
  /** The documented minimum for the audit class, served rather than hard-coded
   * here — the API is the source of that number. */
  auditFloorDays: number;
  auditFloorOverridden: boolean;
  auditFloorOverride?: RetentionOverride;
  lastSweep?: string;
  message?: string;
}

/** A retention change. Every field is optional: an absent class is left alone,
 * which is what lets the form send only what moved. */
export interface PlatformRetentionPatch {
  containerLogs?: number;
  buildLogs?: number;
  flows?: number;
  metrics?: number;
  traces?: number;
  requests?: number;
  clusterEvents?: number;
  activity?: number;
  audit?: number;
  auditFloorOverride?: RetentionOverride;
  clearAuditFloorOverride?: boolean;
}

export interface EventFacetValue {
  value: string;
  count: number;
}

/** One field's distinct values, counted over the rows that came back — which is
 * what `truncated` is there to say. */
export interface EventFacet {
  field: string;
  values: EventFacetValue[];
}

export interface PlatformEvents {
  items: K8sEvent[];
  facets: EventFacet[];
  truncated: boolean;
}

/** What the events explorer is asked over. Every field is also the deep link
 * from another screen — "show me this pod's events". */
export interface PlatformEventQuery {
  since?: string;
  until?: string;
  project?: string;
  environment?: string;
  namespace?: string;
  kind?: string;
  name?: string;
  reason?: string;
  node?: string;
  /** Full text over the message, case-insensitively. */
  search?: string;
  limit?: number;
}

/** The collector DaemonSet's own counts, which catch the one that never
 * started: desired with nothing available and no pods anywhere is admission
 * refusing them, and that leaves nothing for a pod listing to show. */
export interface CollectorStatus {
  present: boolean;
  name?: string;
  namespace?: string;
  desired: number;
  ready: number;
  available: number;
  message?: string;
}

export interface IngestNode {
  node: string;
  /** Why this node's collector pod is not serving, where it is not. */
  collector?: string;
  telemetry: NodeTelemetry;
}

export interface PlatformIngest {
  items: IngestNode[];
  silentNodes: number;
  nodesWithoutCollector: number;
  collector: CollectorStatus;
  flows?: FlowLoss;
  telemetryMessage?: string;
}

export class APIError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/**
 * Send a request with the session's bearer token, and give a 401 exactly one
 * more chance: the token may have been refused a moment before the scheduled
 * renewal, or the issuer may have rotated its keys under us. Renewing and
 * retrying keeps the page — and whatever was half-typed on it — where it is,
 * rather than sending the browser back through the identity provider.
 *
 * A 401 that survives the retry is a session that is over, and the caller
 * routes back to the login.
 */
async function authorized(send: (bearer: string) => Promise<Response>): Promise<Response> {
  const bearer = await token();
  if (!bearer) {
    void signOut();
    throw new APIError(401, "not signed in");
  }
  const res = await send(bearer);
  if (res.status !== 401) return res;

  const renewed = await renew();
  // The same token back means the renewal had nothing new to offer, so the
  // retry would be the request that just failed.
  if (!renewed || renewed === bearer) {
    void signOut();
    throw new APIError(401, "the session expired");
  }
  const retry = await send(renewed);
  if (retry.status === 401) {
    void signOut();
    throw new APIError(401, "the session expired");
  }
  return retry;
}

/** Whether this cluster could snapshot a volume. Both halves have to be there:
 * a snapshot controller with no CRDs registered — which is what issue #64
 * found on a real cluster — accepts nothing and reports nothing. */
export interface SnapshotSupport {
  supported: boolean;
  classes?: string[];
  message?: string;
}

/** The identity provider's database, as a backup sees it. */
export interface BackupAccounts {
  available: boolean;
  database?: string;
  message?: string;
}

/** What an export would carry, and what it deliberately would not. */
export interface Backup {
  platformVersion: string;
  clusterName?: string;
  baseDomain?: string;
  /** How many objects of each kind, keyed by plural name. */
  resources: Record<string, number>;
  secrets: number;
  accounts: BackupAccounts;
  /** Served rather than written into the dashboard, so this screen and the
   * archive's own manifest cannot come to disagree about what is missing. */
  excluded: string[];
  snapshots: SnapshotSupport;
  filename: string;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const config = await loadConfig();
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await authorized((bearer) =>
    fetch(`${base}/api/v1${path}`, {
      method,
      headers: {
        authorization: `Bearer ${bearer}`,
        ...(body !== undefined ? { "content-type": "application/json" } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    }),
  );
  if (!res.ok) {
    let message = `${res.status}`;
    try {
      message = ((await res.json()) as { error: string }).error;
    } catch {
      // keep the status
    }
    throw new APIError(res.status, message);
  }
  // A delete that answers 204 has nothing to parse.
  if (res.status === 204) return undefined as T;
  return res.json();
}

/**
 * Take a platform backup and hand back the archive.
 *
 * It does not go through `request`, because the answer is a gzip stream and
 * not JSON — and it cannot be a plain link either: every call to this API
 * carries a bearer token, which an <a download> has no way to send. So the
 * archive is fetched here and handed to the caller as a Blob to save.
 *
 * A POST, matching the API: the body is every credential the platform holds,
 * and this is the request the audit log records as "somebody took a copy of
 * everything".
 */
export async function downloadBackup(): Promise<{ blob: Blob; filename: string }> {
  const config = await loadConfig();
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await authorized((bearer) =>
    fetch(`${base}/api/v1/platform/backup`, {
      method: "POST",
      headers: { authorization: `Bearer ${bearer}` },
    }),
  );
  if (!res.ok) {
    let message = `${res.status}`;
    try {
      message = ((await res.json()) as { error: string }).error;
    } catch {
      // keep the status
    }
    throw new APIError(res.status, message);
  }
  const disposition = res.headers.get("content-disposition") ?? "";
  const named = /filename="?([^";]+)"?/.exec(disposition);
  return { blob: await res.blob(), filename: named?.[1] ?? "kitchen-backup.tar.gz" };
}

/** The half of an audit pack this dashboard reads.
 *
 * It is deliberately narrow. The screen's job is to take a pack and hand it
 * over, not to interpret it — a type mirroring the whole document would be a
 * second copy of the API's shape for somebody to keep in step, and the pack's
 * own page (docs/api/audit-pack.md) is where the fields are described. What is
 * here is what the screen shows before somebody saves the file: whether it is
 * signed, whether the window is fully covered, and how much is in it. */
export interface AuditPack {
  schema: string;
  project: string;
  range: { from: string; to: string; halfOpen: string };
  verification: {
    signed: boolean;
    message?: string;
    keyID?: string;
    procedure: string[];
    warning: string;
  };
  retention: { truncated: boolean; message: string; auditDays: number; coveredFrom?: string };
  platform: { auditRecording: boolean; rescanning: boolean; rescanMessage?: string };
  inventory: { environments: unknown[]; releases: unknown[]; claims: unknown[] };
  changeLog: unknown[];
  promotions: unknown[];
  decisions: { items: unknown[]; truncated: boolean; message?: string };
  attestations: unknown[];
  exceptions: unknown[];
  drift: { current: unknown[]; history: unknown[] };
  auditLog: { items: unknown[]; truncated: boolean; message?: string; privileged: number };
  signedRecords: { items: unknown[] };
}

/** One rendering of one project's audit pack, as a file to save.
 *
 * It does not go through `request` for the reason `downloadBackup` does not:
 * the answer is a document rather than an API payload, it can be HTML, and it
 * cannot be a plain link either — every call to this API carries a bearer
 * token, which an `<a download>` has no way to send.
 *
 * `digest` is the sha256 of the *pack's* bytes, whichever rendering was asked
 * for: it identifies the document, not the response, which is what lets a
 * printed page be tied back to the bytes that were signed. */
export async function downloadAuditPack(
  project: string,
  range: { from: string; to: string },
  format: "json" | "dsse" | "html" = "json",
): Promise<{ blob: Blob; filename: string; digest: string }> {
  const config = await loadConfig();
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const params = new URLSearchParams({ from: range.from, to: range.to });
  if (format !== "json") params.set("format", format);
  const res = await authorized((bearer) =>
    fetch(`${base}/api/v1/projects/${encodeURIComponent(project)}/audit-pack?${params}`, {
      headers: { authorization: `Bearer ${bearer}` },
    }),
  );
  if (!res.ok) {
    let message = `${res.status}`;
    try {
      message = ((await res.json()) as { error: string }).error;
    } catch {
      // keep the status
    }
    throw new APIError(res.status, message);
  }
  const disposition = res.headers.get("content-disposition") ?? "";
  const named = /filename="?([^";]+)"?/.exec(disposition);
  return {
    blob: await res.blob(),
    filename: named?.[1] ?? `kitchen-audit-pack-${project}.${format === "html" ? "html" : "json"}`,
    digest: res.headers.get("x-kitchen-pack-digest") ?? "",
  };
}


const list =
  <T>(path: string) =>
  async (query?: Record<string, string>): Promise<T[]> => {
    const qs = query && Object.keys(query).length ? `?${new URLSearchParams(query)}` : "";
    const body = await request<{ items: T[] }>("GET", `${path}${qs}`);
    return body.items;
  };

/**
 * Follow an endpoint as Server-Sent Events. The server sends the current page
 * first and then every row that arrives, until `signal` aborts. Uses fetch
 * rather than EventSource because the API wants a bearer token, which
 * EventSource cannot carry. Throws when the stream cannot be established or
 * drops — the caller's cue to fall back to polling.
 *
 * It is generic over the row because the API's two live tails — log lines and
 * the edge's requests — are the same loop over different rows, on the server
 * as much as here.
 */
async function streamRows<T>(path: string, onRow: (row: T) => void, signal: AbortSignal): Promise<void> {
  const config = await loadConfig();
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await authorized((bearer) =>
    fetch(`${base}/api/v1${path}`, {
      headers: { authorization: `Bearer ${bearer}`, accept: "text/event-stream" },
      signal,
    }),
  );
  if (!res.ok || !res.body || !(res.headers.get("content-type") ?? "").includes("text/event-stream")) {
    throw new APIError(res.status, `streaming unavailable (${res.status})`);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) throw new APIError(0, "the stream ended");
    buffer += decoder.decode(value, { stream: true });
    let boundary;
    while ((boundary = buffer.indexOf("\n\n")) >= 0) {
      const chunk = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      let event = "message";
      let data = "";
      for (const line of chunk.split("\n")) {
        if (line.startsWith("event:")) event = line.slice(6).trim();
        if (line.startsWith("data:")) data += line.slice(5).trim();
      }
      if (!data) continue;
      if (event === "error") {
        let message = data;
        try {
          message = (JSON.parse(data) as { error: string }).error;
        } catch {
          // keep the raw payload
        }
        throw new APIError(0, message);
      }
      try {
        onRow(JSON.parse(data) as T);
      } catch {
        // an unreadable event is dropped, the stream lives on
      }
    }
  }
}

export interface LogQuery {
  limit?: number;
  since?: string;
  until?: string;
  search?: string;
  container?: string;
}

/** The selection as query parameters, leaving out what was not asked. */
function selectionParams(selection: LogSelection): URLSearchParams {
  const params = new URLSearchParams();
  if (selection.q) params.set("q", selection.q);
  if (selection.where) params.set("where", selection.where);
  if (selection.since) params.set("since", selection.since);
  if (selection.until) params.set("until", selection.until);
  return params;
}

/** The window and route filter the four request reads share. */
function requestParams(window: RequestWindow): URLSearchParams {
  const params = new URLSearchParams();
  if (window.since) params.set("since", window.since);
  if (window.until) params.set("until", window.until);
  if (window.route) params.set("route", window.route);
  return params;
}

/** The listing's own filters on top of that window. */
function requestListParams(query: RequestListQuery): URLSearchParams {
  const params = requestParams(query);
  if (query.method) params.set("method", query.method);
  if (query.status) params.set("status", query.status);
  if (query.errors) params.set("errors", "1");
  if (query.limit) params.set("limit", String(query.limit));
  return params;
}

function logQuery(query: LogQuery): string {
  const params = new URLSearchParams();
  if (query.limit) params.set("limit", String(query.limit));
  if (query.since) params.set("since", query.since);
  if (query.until) params.set("until", query.until);
  if (query.search) params.set("search", query.search);
  if (query.container) params.set("container", query.container);
  const qs = params.toString();
  return qs ? `?${qs}` : "";
}

export const api = {
  /** The caller, to themselves: the account behind the token and its platform
   * role. What they may do with a *project* travels on the project. */
  me: () => request<Me>("GET", "/me"),

  projects: list<Project>("/projects"),
  createProject: (project: NewProject) => request<Project>("POST", "/projects", project),
  project: (name: string) => request<Project>("GET", `/projects/${name}`),
  updateProject: (name: string, changes: ProjectSettings) =>
    request<Project>("PATCH", `/projects/${name}`, changes),
  // Environment variables are their own route and their own role: the day job
  // is a developer's where the project's settings are an admin's. The list
  // replaces the stored one wholesale — which is why every variable has to be
  // in it — and a body with no `env` at all is refused rather than read as an
  // empty list. The answer is the project, so the new list renders without a
  // second read.
  updateProjectEnv: (name: string, env: EnvVarWrite[]) =>
    request<Project>("PATCH", `/projects/${name}/env`, { env }),
  deleteProject: (name: string) => request<Project>("DELETE", `/projects/${name}`),

  // A project's own secrets: the credentials Kitchen did not mint. The write
  // is the same request whether it is a new secret or a rotation, and the
  // value travels one way — no response on this API carries one, which is why
  // there is no `getProjectSecret` here to call.
  projectSecrets: (project: string) => list<ProjectSecret>(`/projects/${project}/secrets`)(),
  setProjectSecret: (project: string, name: string, value: string) =>
    request<ProjectSecret>("PUT", `/projects/${project}/secrets/${encodeURIComponent(name)}`, { value }),
  // Answers 204, or 409 naming the environment variables that still read it —
  // which is the sentence the screen shows rather than swallows.
  deleteProjectSecret: (project: string, name: string) =>
    request<void>("DELETE", `/projects/${project}/secrets/${encodeURIComponent(name)}`),
  // A project's people. Reading the list is a viewer's — knowing who else is
  // on a project is part of knowing what the project is — and the three
  // writes are an admin's. They name the member in the body rather than in
  // the path: an issuer's subject is opaque and may carry characters a path
  // segment cannot.
  members: (project: string) => list<Member>(`/projects/${project}/members`)(),
  addMember: (project: string, member: NewMember) =>
    request<Member>("POST", `/projects/${project}/members`, member),
  changeMemberRole: (project: string, subject: string, role: string) =>
    request<Member>("PATCH", `/projects/${project}/members`, { subject, role }),
  // Answers 204. The API refuses to remove the last admin and says so in a
  // 409, which is the sentence the screen shows rather than swallows.
  removeMember: (project: string, subject: string) =>
    request<void>("DELETE", `/projects/${project}/members`, { subject }),

  // A project's CI keys — the same membership list with its non-human half
  // shown. Listing carries the prefix and never a key value; issuing answers
  // the key **once**, which is the one response in this whole client whose
  // body must not be kept.
  projectKeys: (project: string) => list<ProjectKey>(`/projects/${project}/keys`)(),
  createKey: (project: string, key: NewKey) => request<IssuedKey>("POST", `/projects/${project}/keys`, key),
  // Answers 204. Revokes the credential first and takes the grant off after:
  // a grant naming an account that no longer exists is a line to tidy up, and
  // a key that still works is not.
  deleteKey: (project: string, name: string) =>
    request<void>("DELETE", `/projects/${project}/keys/${encodeURIComponent(name)}`),

  projectBuilds: (name: string) => list<Build>(`/projects/${name}/builds`)(),
  projectReleases: (name: string) => list<Release>(`/projects/${name}/releases`)(),
  projectEnvironments: (name: string) => list<Environment>(`/projects/${name}/environments`)(),
  rebuild: (project: string, revision?: { sha: string; branch?: string }) =>
    request<Build>("POST", `/projects/${project}/builds`, revision ?? {}),

  // The platform upgrading itself. Creating an update takes a version and
  // nothing else; every other decision is the operator's.
  // `refresh` skips the hour-long cache in front of the chart registry and
  // asks it again, which is what the settings page's re-check does.
  updates: (refresh = false) => request<PlatformUpdates>("GET", `/updates${refresh ? "?refresh=true" : ""}`),
  startUpdate: (version: string) => request<PlatformUpdate>("POST", "/updates", { version }),
  // What helm said while it upgraded the platform — the same LogLine rows and
  // the same bounded-then-followed pair as a build's output, over the
  // self-update job's pod. The job is reaped an hour after it finishes and the
  // lines outlive it in the store, so a finished update answers as readily as
  // the one running now; an update that never started a job answers with an
  // empty page rather than an error.
  updateLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/updates/${name}/logs${logQuery(query)}`).then((b) => b.items),
  streamUpdateLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamRows<LogLine>(`/updates/${name}/logs${logQuery(query)}`, onLine, signal),

  builds: list<Build>("/builds"),
  build: (name: string) => request<Build>("GET", `/builds/${name}`),
  cancelBuild: (name: string) => request<Build>("POST", `/builds/${name}/cancel`),
  buildLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/builds/${name}/logs${logQuery(query)}`).then((b) => b.items),

  releases: list<Release>("/releases"),
  // What a move between two releases would change: `name` is where the
  // environment is going, `against` where it is now. The comparison is made
  // on the server precisely so that the values do not have to travel — see
  // ConfigDiff.
  releaseConfigDiff: (name: string, against: string) =>
    request<ConfigDiff>("GET", `/releases/${name}/config-diff?against=${encodeURIComponent(against)}`),

  // Promotions: asking for a release to land on an environment, and reading
  // what became of the asking. The POST answers 201 with the promotion,
  // phase Pending — the policy engine decides from there.
  projectPromotions: (name: string, query: { environment?: string; release?: string; phase?: string } = {}) => {
    const params = new URLSearchParams();
    if (query.environment) params.set("environment", query.environment);
    if (query.release) params.set("release", query.release);
    if (query.phase) params.set("phase", query.phase);
    const suffix = params.size ? `?${params.toString()}` : "";
    return request<{ items: Promotion[] }>("GET", `/projects/${name}/promotions${suffix}`).then((b) => b.items);
  },
  promote: (project: string, body: { environment: string; release: string; reason?: string }) =>
    request<Promotion>("POST", `/projects/${project}/promotions`, body),
  promotion: (name: string) => request<Promotion>("GET", `/promotions/${name}`),

  environments: list<Environment>("/environments"),
  environment: (name: string) => request<Environment>("GET", `/environments/${name}`),
  // The answer is the environment after the move — or, when the environment
  // declares requirements, the promotion the move became (202): the policy
  // engine decides, and the promotions list is where the verdict lands.
  moveEnvironment: (name: string, release: string) =>
    request<Environment | Promotion>("PATCH", `/environments/${name}`, { release }),
  deleteEnvironment: (name: string) => request<Environment>("DELETE", `/environments/${name}`),
  // The requirements write is the environment's owners' (or an operator's):
  // the API enforces it in the handler, so `may()` alone cannot decide this
  // control — the screen also checks the owners list against the caller.
  patchEnvironmentRequirements: (
    name: string,
    body: {
      bundleDigest?: string;
      parameters?: Record<string, string>;
      owners?: string[];
      dataClass?: string;
      residency?: string;
      criticality?: string;
      rto?: string;
      rpo?: string;
    },
  ) => request<Environment>("PATCH", `/environments/${name}/requirements`, body),
  environmentEligibility: (name: string, release?: string) =>
    request<EnvironmentEligibility>(
      "GET",
      `/environments/${name}/eligibility${release ? `?release=${encodeURIComponent(release)}` : ""}`,
    ),
  environmentWorkload: (name: string) => request<Workload>("GET", `/environments/${name}/workload`),
  // The workers and scheduled jobs (#78). It is per environment, not per
  // project, because what runs is the *release's* process list: an environment
  // that has been rolled back runs what that release declared.
  environmentProcesses: (name: string) =>
    request<{ items: Process[] }>("GET", `/environments/${name}/processes`).then((body) => body.items),
  processRuns: (environment: string, process: string) =>
    request<{ items: ProcessRun[] }>(
      "GET",
      `/environments/${environment}/processes/${encodeURIComponent(process)}/runs`,
    ).then((body) => body.items),
  // No body: nothing about a manual run is the caller's to choose. It is a
  // copy of what the schedule would have run.
  startProcessRun: (environment: string, process: string) =>
    request<ProcessRun>("POST", `/environments/${environment}/processes/${encodeURIComponent(process)}/runs`),
  // What the workload endpoint cannot be: the same environment over time.
  environmentMetrics: (name: string, query: { since?: string; until?: string; points?: number } = {}) => {
    const params = new URLSearchParams();
    if (query.since) params.set("since", query.since);
    if (query.until) params.set("until", query.until);
    if (query.points) params.set("points", String(query.points));
    return request<ResourceSeries>("GET", `/environments/${name}/metrics?${params}`);
  },
  environmentObjects: (name: string) => request<EnvironmentObjects>("GET", `/environments/${name}/objects`),
  environmentLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/environments/${name}/logs${logQuery(query)}`).then((b) => b.items),

  // The observability surface. An empty selection is a legitimate question —
  // everything in the window — so nothing has to be typed to ask it.
  logs: (selection: LogSelection, limit?: number) => {
    const params = selectionParams(selection);
    if (limit) params.set("limit", String(limit));
    return request<{ items: LogLine[] }>("GET", `/logs?${params}`).then((b) => b.items);
  },

  // The same selection, asked three other ways: when, what else is in it, and
  // what it is actually saying.
  logHistogram: (selection: LogSelection, buckets?: number) => {
    const params = selectionParams(selection);
    if (buckets) params.set("buckets", String(buckets));
    return request<LogHistogram>("GET", `/logs/histogram?${params}`);
  },
  logFacets: (selection: LogSelection, fields?: string[]) => {
    const params = selectionParams(selection);
    if (fields?.length) params.set("fields", fields.join(","));
    // A facet no line in the window holds has no values. An operator running
    // an older API against this dashboard gets `null` there rather than an
    // empty list, so it is normalised here and the type stays honest.
    return request<{ items: LogFacet[] }>("GET", `/logs/facets?${params}`).then((b) =>
      b.items.map((facet) => ({ ...facet, values: facet.values ?? [] })),
    );
  },
  logPatterns: (selection: LogSelection, limit?: number) => {
    const params = selectionParams(selection);
    if (limit) params.set("limit", String(limit));
    return request<{ items: LogPattern[] }>("GET", `/logs/patterns?${params}`).then((b) => b.items);
  },

  // A question worth keeping. The URL already makes any selection a link;
  // this is what makes one findable by whoever did not get the link.
  savedQueries: list<SavedQuery>("/logs/saved"),
  saveQuery: (query: NewSavedQuery) => request<SavedQuery>("POST", "/logs/saved", query),
  deleteSavedQuery: (name: string) => request<SavedQuery>("DELETE", `/logs/saved/${encodeURIComponent(name)}`),

  // Live tails of the same log endpoints, as Server-Sent Events.
  streamBuildLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamRows<LogLine>(`/builds/${name}/logs${logQuery(query)}`, onLine, signal),
  streamEnvironmentLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamRows<LogLine>(`/environments/${name}/logs${logQuery(query)}`, onLine, signal),
  streamLogs: (selection: LogSelection, limit: number, onLine: (line: LogLine) => void, signal: AbortSignal) => {
    const params = selectionParams(selection);
    if (limit) params.set("limit", String(limit));
    return streamRows<LogLine>(`/logs?${params}`, onLine, signal);
  },

  // What the internet asked of an environment. The three aggregate reads come
  // off the rollups — a year-wide summary costs what an hour's does — and the
  // listing off the raw rows, which are kept for the shorter of a week and the
  // platform's retention.
  requestSummary: (name: string, window: RequestWindow = {}) =>
    request<RequestSummary>("GET", `/environments/${name}/requests/summary?${requestParams(window)}`),
  requestSeries: (name: string, window: RequestWindow & { buckets?: number } = {}) => {
    const params = requestParams(window);
    if (window.buckets) params.set("buckets", String(window.buckets));
    return request<RequestSeries>("GET", `/environments/${name}/requests/series?${params}`);
  },
  // The sort travels to the server because it decides which rows survive the
  // limit: the ten busiest routes and the ten slowest are not the same ten.
  requestRoutes: (name: string, window: RequestWindow & { sort?: RouteSort; limit?: number } = {}) => {
    const params = requestParams(window);
    if (window.sort) params.set("sort", window.sort);
    if (window.limit) params.set("limit", String(window.limit));
    return request<{ items: RequestRoute[]; environment: string; edge: EdgeStatus }>(
      "GET",
      `/environments/${name}/requests/routes?${params}`,
    );
  },
  // The rows themselves, newest first. The body is an object rather than a
  // bare collection because the edge's answer belongs beside them: an empty
  // list means one thing for an environment on the edge and another for one
  // that is not.
  requests: (name: string, query: RequestListQuery = {}) =>
    request<{ items: RequestRow[]; environment: string; edge: EdgeStatus }>(
      "GET",
      `/environments/${name}/requests?${requestListParams(query)}`,
    ),
  // The same listing followed live, over the same loop the log tails use. The
  // server sends its page oldest first and then every request as it lands.
  streamRequests: (name: string, query: RequestListQuery, onRow: (row: RequestRow) => void, signal: AbortSignal) =>
    streamRows<RequestRow>(`/environments/${name}/requests?${requestListParams(query)}`, onRow, signal),

  // The crash report: exit code and reason, the last lines, the memory series
  // leading up to it, the cluster's warnings and the edge's requests, joined.
  environmentDiagnostics: (name: string, sizes: { logs?: number; requests?: number } = {}) => {
    const params = new URLSearchParams();
    if (sizes.logs) params.set("logs", String(sizes.logs));
    if (sizes.requests) params.set("requests", String(sizes.requests));
    return request<Diagnostics>("GET", `/environments/${name}/diagnostics?${params}`);
  },

  // The platform's recent activity, newest first.
  events: (query: { project?: string; limit?: number } = {}) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.limit) params.limit = String(query.limit);
    return list<PlatformEvent>("/events")(params);
  },

  // The dashboard's numbers, pre-aggregated server-side.
  metricsOverview: (project?: string) =>
    request<MetricsOverview>("GET", `/metrics/overview${project ? `?project=${encodeURIComponent(project)}` : ""}`),

  // Traces, from applications that instrument themselves. The two filters are
  // the two reasons anyone opens a trace list: something failed, or it was
  // slow.
  traces: (
    query: {
      project?: string;
      environment?: string;
      service?: string;
      since?: string;
      until?: string;
      errors?: boolean;
      minDuration?: number;
      limit?: number;
    } = {},
  ) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.environment) params.environment = query.environment;
    if (query.service) params.service = query.service;
    if (query.since) params.since = query.since;
    if (query.until) params.until = query.until;
    if (query.errors) params.errors = "1";
    if (query.minDuration) params.minDuration = String(query.minDuration);
    if (query.limit) params.limit = String(query.limit);
    return list<Trace>("/traces")(params);
  },
  // No window: a trace id arrives from a log line or from the list, and
  // needing to know when it happened would break that link.
  trace: (traceId: string) => request<TraceDetail>("GET", `/traces/${encodeURIComponent(traceId)}`),

  // The service map's aggregated edges for a window.
  traffic: (query: { project?: string; since?: string; until?: string } = {}) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.since) params.since = query.since;
    if (query.until) params.until = query.until;
    return list<TrafficEdge>("/traffic")(params);
  },

  connections: list<Connection>("/connections"),
  createConnection: (connection: NewConnection) =>
    request<Connection>("POST", "/connections", connection),
  updateConnection: (name: string, changes: ConnectionChanges) =>
    request<Connection>("PATCH", `/connections/${name}`, changes),
  testConnection: (test: ConnectionTestRequest) =>
    request<ConnectionTestResult>("POST", "/connections/test", test),
  deleteConnection: (name: string) => request<void>("DELETE", `/connections/${name}`),
  // What this connection's credential can see, for the repository field of
  // the create-a-project form. Any account may ask: creating a project is
  // self-service, and this is the field after the connection.
  connectionRepositories: (name: string) =>
    request<ConnectionRepositories>("GET", `/connections/${encodeURIComponent(name)}/repositories`),
  // The field after the repository: read it the way a build would and say what
  // the platform makes of it, while the build context is still a form field.
  // It writes nothing, which is why a form may ask it on every keystroke's
  // worth of settling.
  detectRepository: (name: string, target: DetectRequest) =>
    request<Detection>("POST", `/connections/${encodeURIComponent(name)}/detect`, target),
  domains: list<Domain>("/domains"),
  domain: (name: string) => request<Domain>("GET", `/domains/${name}`),
  createDomain: (domain: NewDomain) => request<Domain>("POST", "/domains", domain),
  deleteDomain: (name: string) => request<Domain>("DELETE", `/domains/${name}`),
  claims: list<Claim>("/claims"),
  createClaim: (claim: NewClaim) => request<Claim>("POST", "/claims", claim),
  // Answers 202: the operator's finalizer finishes the teardown — branches,
  // binding secrets and, under deletionPolicy Delete, the database itself.
  deleteClaim: (name: string) => request<Claim>("DELETE", `/claims/${name}`),

  // The platform as it is running: cluster, tunnel, build queue, components.
  status: () => request<PlatformStatus>("GET", "/status"),

  // What is wrong with one environment right now — the diagnostics strip. The
  // same catalogue and the same shape the problems list answers in, narrowed to
  // this environment and its project.
  environmentSignals: (name: string) => request<SignalsAnswer>("GET", `/environments/${name}/signals`),

  // The operator's screens. Everything platform-scoped lives under this one
  // prefix and nothing project-scoped does, which is what makes the
  // authorization it is designed for a middleware rather than an audit.
  platformSignals: () => request<SignalsAnswer>("GET", "/platform/signals"),
  // `node` narrows to one, which is where the findings' evidence links point.
  platformNodes: (query: { node?: string } = {}) =>
    request<PlatformNodes>("GET", `/platform/nodes${query.node ? `?node=${encodeURIComponent(query.node)}` : ""}`),
  platformWorkloads: (query: { namespace?: string; limit?: number } = {}) => {
    const params = new URLSearchParams();
    if (query.namespace) params.set("namespace", query.namespace);
    if (query.limit) params.set("limit", String(query.limit));
    return request<PlatformWorkloads>("GET", `/platform/workloads?${params}`);
  },
  // The window bounds the traffic tables; the Gateway, the tunnel and the
  // certificates are read as they are, whatever it says.
  platformEdge: (query: { since?: string; until?: string; limit?: number } = {}) => {
    const params = new URLSearchParams();
    if (query.since) params.set("since", query.since);
    if (query.until) params.set("until", query.until);
    if (query.limit) params.set("limit", String(query.limit));
    return request<PlatformEdge>("GET", `/platform/edge?${params}`);
  },
  platformStorage: () => request<PlatformStorage>("GET", "/platform/storage"),
  platformEvents: (query: PlatformEventQuery = {}) => {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== "") params.set(key, String(value));
    }
    return request<PlatformEvents>("GET", `/platform/events?${params}`);
  },
  platformIngest: () => request<PlatformIngest>("GET", "/platform/ingest"),
  // How long each class is kept, and how far back each one goes. The PATCH
  // refuses an audit retention under the floor unless the body carries the
  // override, and says so itself rather than leaving it to admission.
  platformRetention: () => request<PlatformRetention>("GET", "/platform/retention"),
  updatePlatformRetention: (body: PlatformRetentionPatch) =>
    request<PlatformRetention>("PATCH", "/platform/retention", body),
  // What an export would carry. Taking one is downloadBackup, which answers a
  // gzip stream rather than JSON and so cannot live in this table.
  backup: () => request<Backup>("GET", "/platform/backup"),

  compliance: () => request<Compliance>("GET", "/compliance"),
  complianceInventory: () => request<ComplianceInventory>("GET", "/compliance/inventory"),
  // Drift: the deployed releases that no longer clear their bar. Compliant
  // pairs are left out unless `all` asks for them, because the question the
  // view exists for is what is not.
  complianceDrift: (query: { project?: string; environment?: string; all?: boolean } = {}) => {
    const params = new URLSearchParams();
    if (query.project) params.set("project", query.project);
    if (query.environment) params.set("environment", query.environment);
    if (query.all) params.set("all", "true");
    const search = params.toString();
    return request<ComplianceDrift>("GET", `/compliance/drift${search ? `?${search}` : ""}`);
  },
  // The criticality mapping (#141). Both are traversals of the reconciled
  // graph made on the request, so neither is cached here either.
  complianceCriticality: (query: { criticality?: string; project?: string } = {}) => {
    const params = new URLSearchParams();
    if (query.criticality) params.set("criticality", query.criticality);
    if (query.project) params.set("project", query.project);
    const search = params.toString();
    return request<CriticalityMap>("GET", `/compliance/criticality${search ? `?${search}` : ""}`);
  },
  complianceDependents: (subject: { connection?: string; provider?: string }) => {
    const params = new URLSearchParams();
    if (subject.connection) params.set("connection", subject.connection);
    if (subject.provider) params.set("provider", subject.provider);
    return request<CriticalityDependents>("GET", `/compliance/dependents?${params}`);
  },
  audit: (query: AuditQuery = {}) => {
    const params: Record<string, string> = {};
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== "") params[key] = String(value);
    }
    return list<AuditRecord>("/audit")(params);
  },
  verifyAudit: (from = 1) => request<AuditVerification>("GET", `/audit/verify?from=${from}`),
  decisions: (query: DecisionQuery = {}) => {
    const params: Record<string, string> = {};
    for (const [key, value] of Object.entries(query)) {
      if (value !== undefined && value !== "") params[key] = String(value);
    }
    return list<Decision>("/decisions")(params);
  },
  decision: (id: string) => request<Decision>("GET", `/decisions/${encodeURIComponent(id)}`),
  replayDecision: (id: string) => request<DecisionReplay>("POST", `/decisions/${encodeURIComponent(id)}/replay`),
  policyBundles: () => list<PolicyBundle>("/policy/bundles")(),
  // The exception register: active grants by default, the whole history with
  // historical. Resolving is the one write — an auditable act with a reason.
  exceptions: (query: { project?: string; environment?: string; historical?: boolean } = {}) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.environment) params.environment = query.environment;
    if (query.historical) params.historical = "true";
    return list<Exception>("/exceptions")(params);
  },
  resolveException: (name: string, reason: string) =>
    request<Exception>("PATCH", `/exceptions/${encodeURIComponent(name)}`, { resolved: true, reason }),
  // Access recertification. Every one of these is the operator's: the answer
  // is the whole installation's access in one document.
  identities: () => request<IdentitySurvey>("GET", "/access/identities"),
  accessReviews: (historical = false) =>
    list<AccessReview>("/access/reviews")(historical ? { historical: "true" } : {}),
  accessReview: (name: string) => request<AccessReview>("GET", `/access/reviews/${encodeURIComponent(name)}`),
  openAccessReview: (body: { scope?: string; project?: string; reason?: string } = {}) =>
    request<AccessReview>("POST", "/access/reviews", body),
  // Decisions and the close go in one request on purpose: a close that raced
  // the last decision would mint an artefact missing it.
  reviewAccess: (name: string, body: { decisions?: AccessDecision[]; close?: boolean }) =>
    request<AccessReview>("PATCH", `/access/reviews/${encodeURIComponent(name)}`, body),
  attestations: (build: string) => request<EvidenceSet>("GET", `/builds/${encodeURIComponent(build)}/attestations`),
  // Exploitability assertions, joined to the findings they modify. The read is
  // what keeps a suppression from being silent: it is the one place a person
  // can see that a critical finding is not blocking, who said it does not
  // apply here, and on what grounds.
  vex: (build: string) => request<VEXAnswer>("GET", `/builds/${encodeURIComponent(build)}/vex`),
  submitVEX: (build: string, document: unknown) =>
    request<{ documentID?: string; author: string; submittedBy: string; vulnerabilities: string[] }>(
      "POST",
      `/builds/${encodeURIComponent(build)}/vex`,
      { document },
    ),

  settings: () => request<Settings>("GET", "/settings"),
  // Fields left out stay as they are, `operators` included — a settings patch
  // that does not mention the list cannot disturb it. When it does, the list
  // replaces the old one wholesale, so every operator who is to stay has to
  // be in it.
  updateSettings: (
    changes: Partial<
      Pick<Settings, "buildStrategy" | "buildConcurrency" | "releaseRetention" | "logRetentionDays">
    > & { operators?: OperatorWrite[] },
  ) =>
    request<Settings>("PATCH", "/settings", changes),
};
