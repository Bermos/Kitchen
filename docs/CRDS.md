# Kitchen — CRD Schema (draft)

API group: `kitchen.bermos.dev/v1alpha1`.

All Kitchen CRs live in the platform namespace (`kitchen-system`). The operator materializes
workloads (apps/v1 Deployments, Services, HTTPRoutes, Secrets) into per-project namespaces
(`kitchen-<project>`). CRs are the desired state; ClickHouse never holds state, only telemetry.

## Resource overview

```mermaid
graph LR
    K[Kitchen<br/><i>cluster config, singleton</i>]
    C[Connection<br/><i>git / registry / database</i>]
    P[Project] -->|gitSource / registry| C
    P --> B[Build<br/><i>one per commit build</i>]
    B --> R[Release<br/><i>immutable: image + config</i>]
    E[Environment<br/><i>production + previews</i>] -->|releaseRef| R
    E -->|belongs to| P
    D[Domain] -->|target| E
    RC[ResourceClaim<br/><i>e.g. Postgres via Neon</i>] -->|via| C
    RC -->|bound to| P
    PU[PlatformUpdate<br/><i>the platform's own upgrades</i>] -.->|upgrades| K
    SQ[SavedQuery<br/><i>a log question worth keeping</i>]
    NS[NotificationSubscription<br/><i>where to send an account of this</i>]
    ND[NotificationDelivery<br/><i>one event, on its way</i>] -->|owned by| NS
    SQ -.->|alert fires| ND
    E -.->|deploy, health| ND
```

The chain is the product: **webhook → Build → Release → Environment → running pods + URL.**
Rollback is just pointing an Environment at an older Release. A preview deployment is just
an Environment with `type: preview` that the operator creates and deletes on PR events.

---

## `Kitchen` (cluster-scoped, singleton)

Platform-wide configuration, editable from the UI (which is why it's a CR and not just Helm
values — Helm installs the operator; the operator owns runtime config).

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Kitchen
metadata:
  name: default
spec:
  baseDomain: apps.example.com          # generated URLs: <slug>.apps.example.com
  clusterName: chef                     # what the dashboard's status bar calls this cluster;
                                        # defaults to the first label of baseDomain
  residency: CH                         # where this installation's data is — declared, not observed;
                                        # the compliance inventory's default for environments
  api:
    externalURL: https://kitchen.apps.example.com   # operator API + webhook receiver; defaults to kitchen.<baseDomain>
  ingress:
    gatewayClassName: cilium
    publicAddresses: [85.195.238.240]  # where the internet reaches this platform, when a router
                                       # forwards :80/:443 to a private Gateway address; the only
                                       # thing that reads it is the dns.mismatch signal
    cloudflared:
      enabled: true
      tunnelSecretRef: { name: cloudflared-creds }   # tunnel fronts the Gateway service
  tls:
    mode: cloudflared                   # cloudflared | acme | none
    # mode: acme requires the acme block; the API server refuses the combination
    # without it, rather than admitting a platform whose HTTPS listener has no
    # certificate to terminate with.
    # acme:
    #   email: platform@example.com     # the CA's contact address for this account
    #   server: https://acme-v02.api.letsencrypt.org/directory
    #   dns01:                          # DNS-01 only: the platform needs a wildcard
    #     cloudflare:
    #       apiTokenSecretRef: { name: cloudflare-api-token, key: api-token }
  auth:
    enabled: true                       # the platform's identity provider
    host: auth.apps.example.com         # also the OIDC issuer; defaults to auth.<baseDomain>
    secretRef: { name: kitchen-auth }   # written by the chart; issuer + the operator's registration credential
    databaseSecretRef: { name: kitchen-postgres }  # the accounts database; read by backups alone
    previewGate:                        # forward-auth for protected previews
      enabled: true
      host: previews.apps.example.com   # where logins come back to; defaults to previews.<baseDomain>
      replicas: 2
      sessionTTL: 8h
  access:                               # who may do what with the platform itself
    operators:                          # everything, everywhere, plus admin on every project
      - subject: user_01H8X…            # the issuer's `sub`, or an address — see below
        email: anna@example.com         # informational, so the YAML reads
  builds:
    defaultStrategy: auto               # dockerfile | buildpacks | auto (what a project on "auto" takes)
    concurrency: 2                      # builds running at once; read with resources below — the two
                                        # together are what bound the platform's own build footprint
    resources:                          # the ceiling one build runs under, written onto every container
      cpu: "2"                          # of the build pod as its request and its limit at once, so a node
      memory: 4Gi                       # with no room queues the build instead of starting it on top of
                                        # what is already running. A build that reaches the memory ceiling
                                        # is killed and fails saying so. Either may be empty for no
                                        # ceiling. It is the operator's, not a project's: one that could
                                        # raise its own could evict its neighbours.
    timeoutMinutes: 60                  # the ceiling in time the resources above are in capacity: how long
                                        # one build may run before the job controller ends it and the build
                                        # fails with DeadlineExceeded. An hour is far past anything the
                                        # platform is meant to build — raise it where a cold-cache monorepo
                                        # or a small node legitimately takes longer, 0 for no deadline. A
                                        # change reaches builds started after it: a Job's deadline is
                                        # immutable once it exists.
    cache:
      enabled: true                     # reuse layers between builds, in the registry the project pushes to
      mode: max                         # max | min — how much of a BuildKit build is kept
      scope: project                    # project | branch — what two builds share to reuse each other's layers
    imagePollInterval: 10m              # how often the platform asks whether a watched tag has moved — the
                                        # event that corresponds to a push, for a project whose software this
                                        # platform did not build. One registry manifest HEAD per watched
                                        # reference, never a pull. A reference pinned to a digest is never
                                        # asked about; pinning is how a project opts out of moving. 0 is off,
                                        # and anything else has a floor of one minute
  previews:
    maxPerProject: 5                    # preview environments one project may have live at once. It is beside
                                        # the build ceiling because it is the same statement about a different
                                        # thing: how much of this cluster a project may take by pushing. A pull
                                        # request past it gets a commit status and a comment naming the setting
                                        # rather than an environment, and its preview on the next push once a
                                        # slot frees — nothing is queued. Production and anything promoted are
                                        # never counted; a project may set its own (previews.max below); 0 is
                                        # no ceiling at all
  appNamespaces:
    podSecurity: privileged             # privileged | baseline | restricted — the Pod Security level the
                                        # operator labels every kitchen-<project> namespace with. Set rather
                                        # than inherited: rootless BuildKit needs seccomp and AppArmor
                                        # unconfined, which Pod Security admits at privileged alone, and a
                                        # Job whose pods it refuses creates no pod at all — the build sits
                                        # in Running forever. Lower it only where every project builds with
                                        # buildpacks, which ask for neither.
  registry:
    enabled: true                       # the registry the platform runs for itself
    host: registry.apps.example.com     # where it is published; defaults to registry.<baseDomain>
    service: kitchen-registry           # what the route points at, written by the chart
    port: 5000
    secretRef: { name: kitchen-registry }   # written by the chart; the registry's own username and password
  objectStore:
    enabled: false                      # the MinIO the platform can run for itself; off by default
    service: kitchen-objectstore        # the Service every bucket is reached at, written by the chart
    port: 9000
    region: us-east-1                   # what every bucket reports; a formality S3 clients insist on
    secretRef: { name: kitchen-objectstore }   # written by the chart; the store's root access key pair
  scaleToZero:
    enabled: true                       # off by default; needs KEDA + the HTTP add-on
    interceptor:                        # what an idling environment's URL points at
      service: keda-add-ons-http-interceptor-proxy
      namespace: kitchen-system
      port: 8080
  databases:
    namespace: kitchen-databases        # where provisioned databases live — never a project's namespace
  compliance:
    audit:
      enabled: true                     # append-only, hash-chained record of every state transition
      retentionDays: 365                # its own retention, minimum 90 — evidence outlives telemetry
    attestation:
      enabled: true                     # sign a build record for every artifact, attached to its digest
      signingKeyRef: { name: kitchen-attestation-key }   # unset: the operator generates one, once
      build:
        provenance: true                # ask the builder how it built it — SLSA, from BuildKit
        sbom: true                      # ask the builder what is in it; pulls a scanner every build
        sbomGenerator: ""               # unset: a pinned syft scanner, emitting SPDX 2.3
      vendored:                         # evidence about artifacts the platform did not build
        sbom: true                      # generate one where the vendor published none, and attest
                                        # it as the platform's own observation. Off means a
                                        # vendored image no rescan can ever look inside
        sbomGenerator: ""               # unset: a pinned syft, run over the digest. Not the
                                        # builder-side generator above — that one speaks
                                        # BuildKit's scanner protocol and cannot run standalone
        timeoutSeconds: 1800            # one generation; a vendored image can be large
    machineIdentities:                  # exempt from a project's pull request requirement;
      - renovate[bot]                   # every use of the exemption is an audit record
      - release-please[bot]
    gates:                              # run over every artifact; they record findings, never verdicts
      - name: trivy
        image: aquasec/trivy:0.58.0
        version: "0.58.0"               # what a finding is reproducible against
        format: trivy-json
        args: [image, --format=json, --output=$(KITCHEN_FINDINGS), $(KITCHEN_ARTIFACT)]
    rescan:                             # re-evaluate what is deployed, against today's database
      enabled: false                    # off: it costs a scanner pod per environment per interval
      interval: 24h                     # per (environment, release) pair, from its last finished scan
      concurrency: 4                    # scans in flight across the whole platform
      scanner:                          # matched against the SBOM, never against the image
        name: grype
        image: anchore/grype:v0.87.0
        version: "0.87.0"
        format: grype-json              # grype-json, trivy-json or osv-json
        args: [-o, json, --file, $(KITCHEN_FINDINGS), "sbom:$(KITCHEN_SBOM)"]
        timeoutSeconds: 900
  retention:                            # how long each class is kept; absent = inherit
    containerLogs: 14                   # empty inherits observability.clickhouse.retentionDays,
    buildLogs: 180                      # as flows, metrics, traces, requests, clusterEvents
    audit: 365                          # and activity do; audit inherits compliance.audit.retentionDays
    auditFloorOverride:                 # the only way under the 90-day floor, and an audit record
      reason: demonstration cluster; holds no production data at all
      approvedBy: cto@example.com
  backup:                               # the platform's own scheduled backup
    schedule: "0 3 * * *"               # five fields, UTC. Empty = no scheduled backup, which is what
                                        # an installation predating the field keeps having. A schedule
                                        # with no destination is refused at admission: an archive on a
                                        # volume on this cluster does not survive the loss of this
                                        # cluster, so there is deliberately no local destination
    suspend: false                      # pause the schedule without losing it
    timeout: 30m                        # bounds one run: export, upload, read-back and prune together
    destination:
      type: s3                          # any S3-compatible store; the endpoint override is what makes
      s3:                               # MinIO, R2, Backblaze, Wasabi, Ceph and Garage one code path
        bucket: kitchen-backups         # give it its own bucket: it becomes this cluster's root
        prefix: prod                    # credential store — see docs/BACKUP.md
        region: eu-central-1
        endpoint: ""                    # empty is AWS
        forcePathStyle: false           # <endpoint>/<bucket>; every store reached by IP needs it
        serverSideEncryption: AES256    # AES256 | aws:kms — the archive is every secret, in the clear
        credentialsSecretRef:           # accessKeyId + secretAccessKey; written by
          name: kitchen-backup-destination   # PUT /platform/backup/destination and never read back.
                                        # Absent = the ambient chain (IRSA, Pod Identity, an instance
                                        # role), which is the better answer where it is available
    retention:                          # both bounds apply where both are set; empty keeps everything,
      keepLast: 30                      # which is the safe default. It is not a safety property:
      keepDays: 90                      # retention deletes, and Object Lock is the store's answer
  observability:
    clickhouse:
      retentionDays: 30                 # the retention every telemetry class inherits when
                                        # spec.retention does not set it
      secretRef: { name: kitchen-clickhouse }   # written by the chart; the store's connection details
    hubble:
      relayAddress: hubble-relay.kube-system.svc.cluster.local:80   # flow collection; empty disables it
    metrics:
      enabled: true                     # the operator samples restarts, OOM kills, limits and
      intervalSeconds: 30               # replicas; CPU and memory are the collector's kubelet scrape
    traces:
      enabled: true                     # tell applications where to send their spans
      endpoint: http://kitchen-otlp.kitchen-system.svc.cluster.local:4318   # the collector, and where
                                        # the operator exports its own samples
    clockSync:
      enabled: true                     # do the clocks that stamp all of this agree with each other?
      maxDriftSeconds: 5                # measured off the kubelet node leases; drift beyond this is
                                        # an unhealthy `clock-sync` entry in status.components
status:
  conditions: [...]                     # Ready, GatewayProgrammed, TunnelConnected,
                                        # TelemetrySchemaReady, PreviewGateReady, RegistryReady,
                                        # ObjectStoreReady, ScaleToZeroReady, DatabasesReady,
                                        # ComplianceReady, BackupReady, InternalCAReady
  gatewayAddress: 203.0.113.7
  registry:
    host: registry.apps.example.com
    connection: kitchen-registry        # the Connection the operator seeded, once
  objectStore:
    endpoint: http://kitchen-objectstore.kitchen-system.svc.cluster.local:9000
    connection: kitchen-objectstore     # the Connection the operator seeded, once
  scaleToZero:
    managed: true                       # false or absent: KEDA was already there and is nobody's to touch
    namespace: keda
    version: 2.20.2                     # what the operator installed, and may upgrade
    addOnVersion: 0.15.0
  databases:
    managed: true                       # false: CloudNativePG was already there and is nobody's to touch
    namespace: cnpg-system
    version: 0.29.0                     # the chart the operator installed, and may upgrade
  compliance:
    audit:
      recording: true                   # false with a message when there is nowhere to append to
      sequence: 1428                    # where the chain ends, published outside the table
    attestation:
      signing: true
      keyID: 9f2c...                    # SHA-256 of the public key's DER encoding
      secretName: kitchen-attestation-key
    rescan:
      running: true                     # false with a message: off, no scanner, or nothing to sign with
      lastSweep: 2026-08-24T03:14:00Z
      environments: 42                  # deployed pairs the last pass considered
      scanning: 4                       # how many had a scan in flight when it finished
  retention:
    lastSweep: 2026-08-24T03:00:00Z
    auditFloorOverridden: false
    classes:
      - class: containerLogs
        days: 14
        source: retention               # or the legacy field this class inherits from
        enforced: true                  # false until a sweep has measured it
        rows: 41203311
        oldest: 2026-08-10T04:11:02Z    # the claim the rule makes: nothing older than this
        expired: 0                      # rows still past the horizon; a small number is normal
  backup:                               # what the schedule has actually been doing — the half of this
    schedule: 0 3 * * *                 # feature that matters most, because a backup system's
    suspended: false                    # characteristic failure is six weeks of no archive that
    destination: s3://kitchen-backups/prod   # nobody noticed. Also a BackupReady condition and a
    lastRun: 2026-08-24T03:00:00Z       # `backup` row in status.components, which is the list an
    lastSuccess: 2026-08-24T03:01:44Z   # operator already reads
    lastSuccessArchive: prod/kitchen-backup-prod-2026-08-24T030102Z.tar.gz
    lastSuccessBytes: 4718592
    lastFailure: null
    archives: 30                        # what the last run left at the destination, after its prune
    message: the last archive was written to s3://kitchen-backups/prod 6 hours ago
  clockSync:
    checked: 2026-08-24T09:00:00Z
    method: kubelet node lease renewTime, compared with the operator's own clock
    nodes: 3
    drifted: 0
    maxDriftSeconds: 5
    worstNode: node-b
    worstDriftMillis: 42
```

`InternalCAReady` is the platform's report on its own encryption, across every
store it bundles: the telemetry store and the identity provider's accounts
database. One condition rather than one each — the question is whether this
namespace is readable, and the answer is as good as its weakest store.

There is no field for it on this object: whether a bundled store serves TLS is
a chart value (`clickhouse.tls.enabled`, `postgres.tls.enabled`), and what
reaches the operator is the connection secret the chart writes — which either
names a Secret for that store's certificate or does not. The condition says
which, and it is written by reading cert-manager's own `Certificate` objects
rather than by remembering what the last reconcile did:

- **True** — the platform's internal CA is issued and every bundled store
  serves a certificate signed by it, and there is an `internal-ca` row in
  `status.components` beside the workloads.
- **False, `Issuing` or `CertManagerUnavailable`** — the CA or one store's
  certificate is not there yet, named, with cert-manager's own message. That
  store's pod is waiting for it, so this is also the answer to "why is
  ClickHouse — or Postgres — in ContainerCreating".
- **False, `StoreInTheClear`** — a store is reached unencrypted, because
  somebody set `clickhouse.tls.enabled=false` or `postgres.tls.enabled=false`,
  or pointed the platform at an external store that offers no TLS. The message
  names which store and what is readable in it. It is a choice, so it does not
  hold the platform short of `Ready`; it is said here because a platform
  quietly shipping every log line, every session and its own passwords across
  its namespace is exactly the finding this exists to close.
- **Absent** — there are no bundled stores, or every one of them is external
  with a certificate of its own. Neither is the internal CA's business.

The dependencies the operator installs that the chart cannot are **Addons**,
one object each, and neither is a field here any more. KEDA's HTTP add-on ships
a `ScaledObject` of KEDA's own CRD, and Helm validates a release's whole
manifest before applying any of it; CloudNativePG ships CRDs and an admission
webhook of its own and is a popular thing for a cluster to already run, and
Helm will not adopt a release another one owns. A platform that owns its
cluster should not answer "install this yourself first" to "give me a
database", and an operator is under none of Helm's constraints — so it installs
one release, waits, and installs the next, as a job under an account the chart
creates only when asked (`scaleToZero.install.enabled`,
`databases.install.enabled`), because it is bound to cluster-admin.

What is left on the singleton is `scaleToZero.enabled` — whether environments
idle at all, which stays meaningful in a cluster somebody else installed KEDA
into — and `databases.namespace`, where provisioned databases go. The two
`Ready` conditions stay too, as roll-ups of the Addons' own, because "can this
cluster idle an environment" and "can it provision a database" are facts about
the cluster everything downstream already asks.

`status.managed` on an Addon is what keeps an install a seed rather than a
takeover: it is written only when the operator's own install job succeeded, and
a cluster already serving the entry's API is recorded with it false and never
written to again. What it decides for CloudNativePG is narrower than for KEDA:
who may *upgrade* it, not who may use it. A `cnpg` connection provisions into
whichever CloudNativePG the cluster runs, whoever installed it.

The `managed` flag is *re-derived* on every reconcile rather than remembered
from the one that installed. The install job is what says the platform
installed a dependency — it is the operator's own, it carries the chart
versions and namespace it installed as labels, and it outlives the reconcile
that read it — so a status write that does not land costs a pass rather than
the ownership of a release. It used to cost the ownership: the record was
minted once, and a platform that had lost it read the cluster as somebody
else's, said so emphatically, and never upgraded the release it had made
itself.

`registry` is why a fresh installation can build something without anyone
having a registry account first. The chart runs zot and its volume; the
operator publishes it on the shared Gateway and seeds a `dockerRegistry`
Connection pointing at it, so a new project's registry picker has a working
default.

Publishing it on the base domain rather than reaching it in-cluster is the
whole design. The node's container runtime is what pulls an image, and it
trusts neither an in-cluster CA nor a plain-HTTP address unless the node is
configured to — which is what every other in-cluster registry needs, and
Kitchen is a chart installed into someone else's cluster. The platform's
wildcard certificate is publicly trusted, so a route on it asks the node for
nothing. The costs are stated rather than hidden: pulls leave the cluster and
come back through the Gateway, the registry is reachable from outside (so it
admits nobody anonymously), and in `tls.mode: none` there is no trusted
certificate at all — `RegistryReady` is then False with `TLSModeNone` and
nothing is published.

`status.registry.connection` is written once and read forever after as "this
has been seeded". A Connection someone deletes stays deleted: the seed is a
good default, not a fixture the platform keeps reinstating. While it is still
there and still labelled `app.kubernetes.io/managed-by: kitchen`, its URL and
credential are kept in step with the registry.

`objectStore` is the same shape with the route left out. The chart runs a
single MinIO with a volume when its `objectStore.enabled` is set, and the
operator seeds an `s3` Connection (`kitchen-objectstore`) pointing at its
Service, with the root credential the chart generated once. There is no
route because nothing outside the cluster needs one: an application runs in
the cluster and reaches the store at its Service address, and the node's
container runtime — the reason the registry needs a publicly trusted
certificate — is not in the path. The corollary is that a bucket in it cannot
be publicly readable, and a claim asking for that is refused with the reason.
The seeded Connection is written with `forcePathStyle: true` (MinIO needs
it), `inCluster: true` (what refuses the public bucket), and the default
`scopedCredentials`, so every claim's bucket gets a user and a policy of its
own minted from the root credential; no application is ever handed the root.
`status.objectStore.connection` is a seed on the registry's terms: deleted,
it stays deleted.

`access.operators` is the platform role, and the only one there is: an account
on the list is an operator — everything, everywhere, and project `admin` on
every project, present and future — and every other account is a member, which
has no platform surface at all, sees what project membership grants it, and
may create projects. It lives on this object because this object is already
the platform's configuration and is already edited through `PATCH /settings`,
so granting somebody the operator hat needs no new store and no kubectl.

It is the one field here with no `{}` default, deliberately. The others have
one so that an installation predating them still gets the feature; here the
absence carries information. **No `access` block at all means nobody has ever
said who the operators are** — an installation upgrading into enforcement,
where every account can call every route today and so every account read
honestly *is* an operator, and the list is seeded from the accounts that
exist. **An empty list means somebody narrowed it to nobody on purpose**, and
is left exactly as it is. A default would collapse the two on the first write
and lock an upgraded installation out of its own platform. See
[AUTH.md](AUTH.md#bootstrap-and-what-happens-on-upgrade).

The entries name accounts the same way a Project's grants do, minus the role —
`subject` plus an informational `email` — and the rule for what `subject` may
hold is the same one, described under `Project` below.

Seeding reads the account directory the bundled identity provider serves, and
**an installation federated to an issuer of its own has none**: OpenID Connect
defines no way to enumerate accounts, so on a Keycloak or an Auth0 nothing is
ever seeded, nobody holds the operator role, and every operator-only route
refuses everybody — including the `PATCH /settings` that would name an
operator. Such an installation names its operators at install time, through
the chart value `kitchen.access.operators`, which renders into this field; a
list the chart wrote is a list somebody wrote, and is never re-seeded over.
The `OperatorsConfigured` condition distinguishes the three: `OperatorsSeeded`
and `OperatorsNamed` say who holds the role, `NoAccountDirectory` says this
issuer serves none and names the value to set, and `DirectoryUnavailable` says
one that should have answered did not, and is retried.

The chart writes this field only on install, and on an upgrade with
`kitchen.applyOnUpgrade=true`, where it carries the live list across rather
than dropping it — the singleton is a Helm hook that is deleted and recreated,
and a recreated object with the field absent would be re-seeded from every
account that exists.

`compliance` is the platform's own evidence, and it is on the singleton rather
than on a Project deliberately: a team that could turn its own audit log off,
or sign its own evidence with a key it chose, would be attesting to nothing.
The audit log is a hash-chained table in the telemetry store with a retention
of its own — telemetry ages out in weeks and the evidence an incident is
reconstructed from must not go with it. Attestation signs a build record for
every artifact and attaches it to the artifact's digest as a DSSE envelope
over an in-toto statement, through OCI referrers, so the evidence is readable
by anything that speaks those and by nothing that has to speak Kitchen. Both
report on `status.compliance`, because the failure mode of evidence is
silence: an installation that believes it is producing evidence and is not
should find that out from the platform rather than from an auditor.

`compliance.rescan` is the same argument extended in time. A gate scans an
artifact on the day it is built and a promotion judges that scan on the day it
is promoted; both were true when they were made, and neither is a statement
about what is running today. The rescan pass walks every currently-deployed
release on `interval`, matches the bill of materials the build already
attested against a **current** vulnerability database, signs the result onto
the artifact's digest and re-runs the environment's own bar over it — through
the same evaluator a promotion uses, so the two cannot disagree. It needs no
rebuild and no redeploy, because the image is never pulled. It is also the
only thing that judges exception expiry, and the only thing that acts on an
Exception's `autoRollback`. `status.compliance.rescan` reports whether it is
running at all, for the same reason the other two report: an installation
that believes it is being re-checked and is not should hear it from the
platform. See [COMPLIANCE.md](COMPLIANCE.md) §9.

`observability` is one retention over a store that two things write. An
OpenTelemetry collector DaemonSet fills the logs, traces and metrics tables
with the stock ClickHouse exporter; the operator fills flows and the activity
feed, and owns the DDL and the TTLs for all of them, including the tables it
never inserts a row into. The exporter runs with `create_schema: false`, so
`retentionDays` moving is still one operator reconcile away from every table,
and the collector on a fresh cluster retries until that reconcile has created
the database.

Both switches govern less than their names suggest, and in opposite
directions. `metrics.enabled` turns the operator's sampler on and off: CPU and
memory come off the kubelet through the collector either way, and what it
decides is whether restarts, OOM kills, resource limits and replica counts are
collected at all. Those are facts about API objects rather than about a
running process, which is why no receiver exposes them — and the restart
differencing has to happen where the previous sample is remembered, because a
lifetime counter bucketed for a chart loses every transition that lands on a
bucket boundary. `traces.enabled` decides whether `traces.endpoint` is
advertised to the environments the operator deploys, through the OTLP
variables every SDK reads; the collector receives either way, and the same
address is where the operator sends its own samples as an ordinary client.

## `Connection` (namespaced: kitchen-system)

A plugin instance. One CRD for all plugin families; `provider` selects the implementation,
`config` is provider-specific (validated by the plugin via webhook).

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Connection
metadata:
  name: github-main
spec:
  provider: github                      # github | gitlab | gitea | dockerRegistry | neon | cnpg | s3
                                        # | inngest | inngestSelfHosted | valkey | redis
  credentialsSecretRef: { name: github-app-creds }   # synced from Infisical
  config:
    appId: "12345"
    webhookSecretRef: { name: github-webhook-secret }
status:
  conditions: [...]                     # Connected, CredentialsValid
  capabilities: [gitSource, statusChecks]
```

A `redis` connection's status carries one more thing, because a server
somebody else runs has a finite pool of logical databases and every claim
through the connection draws from it:

```yaml
status:
  cache:
    databases:                          # which claim holds which logical database
      - { database: 1, holder: kitchen-my-shop-cache }
      - { database: 2, holder: kitchen-my-shop-cache/my-shop-pr-41 }
      - { database: 3, holder: "" }     # handed out before and given back
```

It is the allocation itself and not a report of one: a claim's database is
read back from here on every reconcile, which is what keeps it the same one,
and a claim finding every database held is refused rather than put in one
somebody else is using. Database 0 is never allocated — every binding made
before the platform allocated databases selected it, and the claims that were
bound then keep it. See
[docs/api/claims.md](api/claims.md#creating-a-claim) for what a logical
database does and does not separate.

The Connection the operator seeds for the bundled registry is an ordinary one:
`dockerRegistry`, with `config.url` naming the host it publishes and a
credential the platform wrote and never reads back. Nothing downstream treats
it as a special case — it is deletable, replaceable, and pickable exactly like
one someone created on the connections page.

First-party providers: `github`, `gitlab` and `gitea` (capabilities `gitSource` and
`statusChecks`), `dockerRegistry` (capability `imageStore`), two that
implement `database` — `neon` for a hosted Postgres and `cnpg` for one this
cluster runs itself — and `s3` (capability `objectStore`) for any
S3-compatible store: the bundled MinIO, one a team runs, AWS S3, Cloudflare
R2. The operator matches on **capabilities**, not provider names, which is
why the second database provider cost the claim nothing.

An `s3` connection's `credentialsSecretRef` names a Secret with `accessKeyId`
and `secretAccessKey`; its `config` says where the store is and how it is
talked to — `endpoint`, `region`, `forcePathStyle`, and `scopedCredentials`,
which is whether the platform mints a user and a policy per bucket through
the MinIO admin API (the default) or hands every claim the connection's own
key pair (S3 and R2, which have no such API). The seeded one for the bundled
store additionally carries `inCluster: true`, which is what refuses a
publicly readable bucket: there is no public to read it.
`inngest` (capability `backgroundJobs`) is an Inngest Cloud account an
`inngest` claim reads its keys from, and `inngestSelfHosted` is the same
capability served by an Inngest this cluster runs: one server per claim and
one per preview, which is what gives a preview an event stream of its own.
Its `config` is the operator's defaults for every claim through it — all of it
optional:

```yaml
spec:
  provider: inngestSelfHosted
  config:
    namespace: kitchen-inngest          # where the servers run
    image: inngest/inngest:v1.44.0      # what they run; the default is pinned in
                                        # internal/provider/inngest/selfhosted.go, and bumping it
                                        # means checking Inngest's release notes for the
                                        # persistence flags the provisioner sets
    storageSize: 1Gi                    # the volume behind a preview's embedded store
    storageClass: fast-ssd              # default: the cluster's own default StorageClass
```

Production's server keeps its state in a CloudNativePG `Cluster` and a Valkey
of its own, provisioned through the same providers a `postgres` and a `redis`
claim go through; a preview's keeps its own on that volume. See
[docs/api/claims.md](api/claims.md) for what a claim through each of the two
providers binds, and for what deleting one destroys.

**`cnpg`, `valkey` and `inngestSelfHosted` are the providers with no
credential**, and so the ones whose `credentialsSecretRef` may be left out:
they provision with the operator's own service account, into the cluster
Kitchen is installed in, and there is nothing for anybody to store or rotate.
A CEL rule on the spec keeps that the exception — every other provider is
still refused without one, and one of these naming a credential is refused for
naming something nothing reads. `cnpg`'s `Connected` condition is a fact about
this cluster rather than about a remote API: true when `postgresql.cnpg.io` is
served, false with the install instruction when it is not.

Its `config` is the operator's own defaults for every claim through it, and
the whole of it is optional:

```yaml
spec:
  provider: cnpg
  config:
    namespace: kitchen-databases        # where the databases live; default is the singleton's
    storageSize: 20Gi                   # what a claim that names no size gets
    storageClass: fast-ssd              # default: the cluster's own default StorageClass
    instances: 1                        # Postgres instances per database
    images:                             # replaces the platform's catalogue entirely
      - repository: registry.internal/postgres
        majors: ["16", "17"]
        extensions: [timescaledb]       # what this image promises; claims are refused against it
```

`images` is the operator's and not the claim's on purpose: a developer asking
for an extension should not be able to choose the image it arrives in.

## `Project` (namespaced: kitchen-system)

The unit a user thinks in: software that becomes a running app. Usually a
repository this platform builds; since #307, optionally an image somebody else
built.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Project
metadata:
  name: my-shop
spec:
  source:                               # a union: exactly one member is set
    git:                                # a repository this platform builds
      connectionRef: { name: github-main }
      repo: bermos/my-shop
      productionBranch: main
      requirePullRequest: false         # refuse a production commit the provider cannot say was
                                        # reviewed. Preview builds are unaffected; machine
                                        # identities on the platform's allowlist are exempt
    # image:                            # ...or the web process's image, published by somebody else
    #   repository: ghcr.io/home-assistant/home-assistant
    #   tag: "2026.9.1"                 # a tag or a digest; both means "this tag, and still this content"
    #   digest: sha256:…
    #   connectionRef: { name: ghcr }   # what it is *pulled* with; omit for a public image
    #   signature:                      # whose signature on this image is acceptable
    #     publicKeyRef: { name: ha-cosign }   # a Secret holding `public.pem`; the only thing
    #                                         # that can make the result `verified`
    #     identity: releases@home-assistant.io  # the signer the signature must name
    #     issuer: https://token.actions.githubusercontent.com
  build:
    strategy: auto                      # auto takes the Kitchen default; dockerfile | buildpacks decide here
    dockerfilePath: Dockerfile          # when strategy: dockerfile; relative to rootDirectory,
                                        # which it may not leave
    dockerfileTarget: web               # which stage of a multi-stage Dockerfile to ship.
                                        # Unset ships its last stage; a stage the file does not
                                        # declare fails the build, and so does setting this on a
                                        # build that resolves to buildpacks, which has no stages.
                                        # It is the web process's, and the stage every workload
                                        # that names none of its own is built to
    rootDirectory: .                    # the build root: the directory that is built, and what
                                        # every path the project declares is relative to. Both
                                        # strategies mean the same directory by it
  registry:                             # where builds push. Required with source.git,
    connectionRef: { name: harbor }     # and refused with source.image, which builds nothing
  previews:
    enabled: true
    protected: true                     # gate preview URLs behind platform login (default)
    max: 3                              # this project's own ceiling on live previews, overriding the
                                        # platform's previews.maxPerProject. Unset takes the platform's,
                                        # which is what almost every project should do; 0 is no ceiling
                                        # for this project
    ttlAfterClosed: 1h                  # grace period before teardown
  dataClass: confidential               # public | internal | confidential | strictlyConfidential;
                                        # absent = unclassified, shown as such and never defaulted.
                                        # Claims narrow it, environments must be rated at least it
  scaleToZero:                          # only does anything where the platform allows it
    mode: previews                      # previews (default) | always | never; overridden
                                        # by runtime.notRequestDriven below
    idleAfter: 5m                       # quiet for this long, then no pods at all
    maxReplicas: 5                      # ceiling for a cold-started environment
  access:                               # who may do what with this project
    - subject: user_01H8X…              # the issuer's `sub`, or an address — see below
      email: anna@example.com           # informational, so the YAML reads
      role: developer                   # admin | developer | viewer
  env:                                  # env vars with per-environment-type overlay
    - name: DATABASE_URL
      fromResourceClaim: { name: shop-db, key: url }   # injected by claim binding
    - name: PUBLIC_API_BASE
      value: https://api.example.com
      previewValue: https://api-staging.example.com    # previews get this instead
    - name: SESSION_SECRET
      secretRef: { name: shop-secrets, key: session }  # Infisical-synced
  runtime:
    port: 3000                          # omit to take the detected framework's
    replicas: 2                         # previews always get 1
    notRequestDriven: false             # does work nobody asked for, so nothing of
                                        # this project idles — previews included
    singleton: false                    # two of this must never run at once:
                                        # strategy Recreate, and replicas > 1 refused
    command: [./server]                 # replaces the image's entrypoint; exec
    args: [--config=prod.toml]          # form, a list of words, never a shell line
    previewArgs: [--config=fake.toml]   # used instead of args in previews, the way
                                        # an env var's previewValue replaces its value
    resources: { cpu: 500m, memory: 512Mi }
    health:                             # what the platform asks before it sends
      path: /healthz                    # anyone to a new pod. No path = a TCP
      port: 9000                        # connect to the port above, never GET /
      periodSeconds: 10                 # every environment is probed either way
      timeoutSeconds: 2
      failureThreshold: 3               # a running pod out of service
      startupFailureThreshold: 30       # x period = how long it has to come up
    security:                           # the posture every workload of this
      runAsNonRoot: true                # project runs under — web, workers,
      runAsUser: 1001                   # services and scheduled runs alike,
                                        # unless one declares its own below
      runAsGroup: 1001                  # 0 = the image's own user left
      readOnlyRootFilesystem: true      # alone, not "run as root"
      fsGroup: 1001                     # owns the volumes it mounts: a fresh
                                        # one comes up root:root, which a
                                        # non-root workload cannot write
      fsGroupChangePolicy: OnRootMismatch
                                        # unset = Always, a walk of the whole
                                        # volume on every start
      allowPrivilegeEscalation: false   # the default, and the one the platform
      dropCapabilities: [ALL]           # tightens; there is no list to add one
    init:                               # what the web process needs done inside the
      - volume: config                  # volumes it mounts, before its container
        directories:                    # starts. Names a volume claim this workload
          - path: custom_components     # mounts; every path is relative to that
          - path: secrets               # claim's mountPath, so nothing here can
            mode: "0750"                # leave the volume. Created only if absent
        seed:                           # a `files` entry copied in, and only where
          - file: configuration         # the destination does not exist — so a
            path: configuration.yaml    # second deploy never clobbers what the
                                        # application wrote. Mode is octal *as a
                                        # string*: JSON's numbers are not octal
  processes:                            # what it ships *besides* the web process
    - name: migrate                     # a batch/v1 Job, once per deploy, and
      type: task                        # nothing takes traffic until it succeeds
      command: [npm, run, migrate]
      timeout: 10m                      # how long the deploy waits for it
    - name: worker                      # a Deployment with no Service, no route
      type: worker
      command: [node, worker.js]
      replicas: 2
      singleton: false                  # two of this workload must never run at
                                        # once: Recreate, and replicas > 1 refused
      health: { port: 9000 }            # opt-in here, and it must name the port
      init:                             # a workload's own volume preparation, the
        - volume: spool                 # same declaration as runtime.init above:
          directories: [{ path: jobs }] # a claim names the one process that mounts
                                        # it, so each workload prepares its own
    - name: api                         # a Deployment and a ClusterIP Service,
      type: service                     # and still no route: never published
      port: 8080                        # required here, refused on the others
      build:                            # its own directory of the repository
        strategy: auto                  # auto | dockerfile | buildpacks. auto is
                                        # the default and reads this workload's own
                                        # rootDirectory: a Dockerfile there wins,
                                        # else what detection finds goes to
                                        # buildpacks, else the build fails naming
                                        # this workload. It never inherits the
                                        # project's strategy
        dockerfilePath: Dockerfile      # relative to rootDirectory, which is
        dockerfileTarget: api           # which stage of that file to ship. Unset
                                        # takes the project's, not the file's last
                                        # stage; a stage on a buildpacks workload
                                        # fails the build naming this workload
        rootDirectory: services/api     # this workload's build root
    - name: cache                       # a workload running an image nobody here
      type: service                     # built: the third answer to what `build` asks
      port: 6379
      security:                         # this workload's own posture, merged over
        runAsUser: 65532                # runtime.security field by field: what is
                                        # set here wins, what is left out is
                                        # inherited. It is how a unit whose images
                                        # have different bases says that three run
                                        # as 1001 and this one runs as 65532
      image:                            # excludes `build`; a workload is one or the other
        repository: docker.io/library/redis
        tag: "7.4"                      # a tag or a digest, as above
        # connectionRef: { name: ghcr } # what it is pulled with; omit for a public image
        # signature: { identity: … }    # whose signature is acceptable; see source.image above
    - name: nightly-report              # a batch/v1 CronJob; one firing is a run
      type: cron
      schedule: "0 3 * * *"             # five fields, read in UTC
      command: [node, report.js]
      timeout: 30m                      # becomes activeDeadlineSeconds
      concurrencyPolicy: Forbid         # Allow | Forbid | Replace
      previews: false                   # unset means off for a worker and a
                                        # cron, and on for a service and a task
  files:                                # configuration files, for software that
    - name: configuration               # is configured by one rather than by
      path: /config/configuration.yaml  # variables. Absolute, naming the file
      content: |                        # itself: only that path is replaced
        logger: info
      workloads: [web]                  # who reads it; omit and everything does
    - name: seed-only                   # no path: placed in no container at all.
      content: |                        # Such a file exists to be seeded into a
        logger: info                    # volume by runtime.init — a mounted config
                                        # file is read-only, and mounted where the
                                        # seed writes it would shadow the copy the
                                        # application then owns
    - name: app-ini                     # a file whose content is a credential:
      path: /data/conf/app.ini          # it is held in a Secret the API writes
      secret: true                      # and no response ever reads back, so
                                        # `content` is refused here
status:
  conditions: [...]                     # Ready, Previews, PreviewCapacity, SourceConnected,
                                        # RegistryConnected, WebhookRegistered, InitialBuild. The
                                        # last three belong to a repository and are absent without
                                        # one; PreviewCapacity is absent where there is no ceiling
  productionEnvironmentRef: { name: my-shop-production }
  latestBuildRef: { name: my-shop-bld-8f3a2c1 }
  initialBuildRef: { name: my-shop-bld-8f3a2c1 }   # the build the platform made
                                        # itself, once, when the project was new
  previews:                             # the preview ceiling, as the operator last measured it
    live: 5
    max: 5                              # the ceiling in force — this project's own, or the
                                        # platform's. 0 is none, and the condition is then absent
    refused:                            # pull requests refused a preview at the ceiling, oldest
      - pullRequest: 61                 # first. It is a record, not a queue: each gets its preview
        commit: ab12cd34ef56            # on its next push once a slot is free. Bounded at 20
        at: "2026-09-03T18:04:00Z"
  imagePoll:                            # the digest poll's own record, for a project with
    lastPolledAt: ...                   # vendored references. Absent for everything else
    message: ""                         # why the registry could not be asked, when it could not.
                                        # It is also what keeps a registry that stays down to one
                                        # failed acquisition rather than one every interval
```

`files` is what software the platform did not build is configured by (#311).
It is configuration rather than storage — small, changing with a deploy, and
frozen into every Release, so a rollback restores the file that release ran
with. Each is mounted read-only at its path into every workload it names, and
into all of them where it names none; only that one path is replaced, so the
rest of the directory stays as the image left it. Nothing is substituted into
it from the environment, deliberately —
[docs/api/files.md](api/files.md#why-they-are-not-templated-from-the-environment)
says why. A file marked `secret` carries no `content` here: the API writes it
into `kitchen-project-files-<project>`, which the reconciler mirrors into the
application namespace beside the project's secrets, and no route reads it
back.

`initialBuildRef` is what makes a new project deploy without waiting for a
push: the reconciler resolves the production branch's tip and creates one
Build of it as soon as both connections are usable, and records it here so it
is never done twice. The `InitialBuild` condition carries why it has not
happened yet when it has not.

**A project whose source is an image has no repository, and is asked nothing a
repository is asked.** No source Connection, no registry to push to, no
webhook, and no pull requests — so `SourceConnected`, `RegistryConnected` and
`WebhookRegistered` are not written at all, and the `Previews` condition is
`False` with reason `NoRepository` and a message saying why rather than the
previews quietly never appearing. Its `initialBuildRef` is an **acquisition**:
a Build with no commit, which resolves the digest the image reference names,
freezes it onto a Release and runs no builder. Nothing fakes a commit — the
Build names no SHA and no branch, so the commit-shaped policy rules stay inert
rather than being satisfied by a substitute, and an environment that requires
one refuses the artifact saying which rule and why
([COMPLIANCE.md §18.6](COMPLIANCE.md)).

What such a Build *does* carry is `status.artifact.upstream`: the reference the
image was taken from, the digest it resolved to, who admitted it onto this
platform and when, and what became of the vendor's own signature — `verified`
against the key `source.image.signature` names, `unverifiable` with a reason,
or `none`, which means the vendor publishes no signature and is a fact rather
than a failure. Beside it, `status.artifact.evidence` indexes what the vendor
published about the digest (`source: vendor-asserted`, restated and
countersigned) and what the platform observed about an image it only pulled
(`source: platform-observed`, which includes a bill of materials generated
where the vendor supplied none). `status.artifact.observedSBOM` says what
became of that generation. The whole model is [COMPLIANCE.md
§18](COMPLIANCE.md).

**What moves it afterwards is the digest poll.** A vendored project has no
push to react to, so the platform asks: one registry manifest HEAD per watched
reference per `Kitchen.spec.builds.imagePollInterval`, compared against what
this project's last acquisition resolved, and a new acquisition where they
differ. A reference pinned to a digest is asked nothing at all — that is how a
project says it does not want to be moved — and `POST
/projects/{name}/acquisitions` asks now, optionally naming the digest to take.
`status.imagePoll` is the pass's own record, and the whole of the state it
keeps: what each reference resolved to lives on the Build that took it.

A unit may mix the two: a project built from a repository can carry a workload
that runs an upstream image (`processes[].image` above). They ship in one
Release, which records the digest every workload resolved to, so the unit
deploys and rolls back as one — the vendored digests restored exactly as the
built ones are. The pods pull with the image's own `connectionRef`, or with
nothing where it names none; only what the platform built pulls with what the
build pushed under.

Two of those reasons look alike and are not. `NoCommit` — an empty repository,
or a production branch that is not there — is the project's own configuration
meeting the repository, and asking again changes nothing, so the project is
left alone until somebody pushes or corrects the branch; it is still a ready
project. `RepositoryUnreadable` is the opposite: what has to change is a
credential held at the provider — a token's repository access, an app
installation, a secret somebody replaced — and none of that is something the
platform can watch happen. So the project keeps asking, every 30 seconds,
until the repository answers; it also reports `Ready=False` with that reason
meanwhile, because a project whose source nothing can read is not going to
build. Both are how a credential fixed at the provider reaches the platform on
its own rather than waiting for something to write to the Project.

The project's Connections are watched for the same reason: a connection whose
capabilities or credential verdict has just changed enqueues every project
bound to it, source and registry alike. The Build and Environment watches
cannot cover that case — a project that has never built has neither.

`access` is the project's membership, and it is the whole of the answer: an
account with no entry holds no role here at all, so the project is not in
their list and not theirs to build, redeploy or read. `admin` adds membership,
the project's own settings and deleting it to what a `developer` may do;
`viewer` reads status, URLs, builds, releases and logs, and may open a
protected preview. The one account that needs no entry is a platform operator,
who holds `admin` on every project — that rule lives in exactly one place in
the code, so a project's list can never lock one out. Membership is here,
rather than in the identity provider or in a token claim, because a role
carried in a token is a snapshot good for as long as the token is: removing
somebody would leave them on the project for up to an hour, and removal is the
case where that delay matters most. See
[AUTH.md](AUTH.md#where-membership-lives).

The list merges per `subject` rather than by position, so an apply that adds
one person cannot drop another. `subject` is normally the issuer's `sub`,
which is opaque — the dashboard resolves an address to one when it writes a
grant, and `email` beside it is informational, so that a list of opaque
strings still reads. Hand-written YAML may name an address in `subject`
instead, and the rule telling the two apart is blunt on purpose: **a subject
containing `@` is read as an email address**, matched case-insensitively, and
honoured only for a token whose `email_verified` claim is true. An unverified
address is something the token holder said about themselves, so an
unverified-email grant is a grant to whoever can get the identity provider to
let them type that address — it resolves to no role rather than to the one
written down.

`processes` is what the project ships besides its web process, and there is
deliberately no `web` entry: the web process is `runtime` above, singular
because the URL is — an Environment publishes one hostname, one Service and
one route, and a second process claiming to be the web one would have to be
told which of those it got. A `worker` runs continuously and is never
addressed; a `service` runs continuously and *is* addressed, from inside the
cluster and nowhere else; a `cron` runs on its schedule and each firing is a
Job. Publishing stays the exception `runtime` declares — nothing in this list
gets a route, whatever its type.

A `task` is the one entry that does not keep running (#272): it is one Job per
deploy, and the Environment's reconcile applies **nothing** of the release —
no Deployment, no Service, no CronJob, no route — until it has succeeded, so a
schema migration finishes before anything serves a request and a run that
fails leaves the release that was serving serving. It takes a `timeout` (how
long the deploy waits), no schedule, no port, no health check and no
`singleton`; several of them run in the order they are declared. Reversing a
schema change is out of scope — a rollback runs the task the release it goes
back to declared, which is the most the platform can honestly do.

A workload that declares no `build` runs the Release's image with another
command, which is why this was a field rather than a second build. One that
declares a `build` is built from its own directory of the repository: a
repository shipping four images is **one project with four entries here**
(#271), deployed and rolled back as a whole, because the deployable unit is
the project and a tier above it would double every route in the authorization
table. The Release records which image each workload was built to, beside the
process list it froze, so restoring it restores that exact set.

A `service` is reached at `<environment>-<name>` in the project's application
namespace, and every workload of the environment is handed
`KITCHEN_SERVICE_<NAME>` (plus `_HOST` and `_PORT`) pointing at it — so a
preview's web process talks to the preview's own API, which is what makes a
preview of a multi-workload unit a preview of the unit.

`previews` unset means off for a `worker` and a `cron` and on for a `service`
and a `task`, which is why the field is a pointer. A preview shares the
project's environment variables, so a preview that emails customers nightly is
a bad afternoon and a preview worker draining the production queue is a worse
one; a service, by contrast, is addressed only by its own environment's
siblings, so leaving it out protects nothing and breaks the preview — and a
preview whose own database branch nothing migrated is broken the same way. The list merges per
`name`, so two people adding two workloads do not drop each other's.

`runtime.notRequestDriven` is a workload that does work nobody asked for, and
it turns idling off for every environment of the Project — previews included,
which is where it matters, since previews idle by default. Scale to zero is
request-driven by construction: the interceptor brings an environment back on
the next request to its URL, and there is no request for a background loop.
Parked, it stops, and the hole that leaves in whatever it was collecting is
indistinguishable from the upstream having been down. It lives on the runtime
because it describes what the workload *is*, but the idling decision reads the
Project's live value rather than the Release's frozen copy, for the same
reason `scaleToZero` is not snapshotted at all: a rollback must not quietly
start parking an environment again.

**Idling is the web process's, and that is why `scaleToZero` is the project's.**
The signal behind scale to zero is request pressure at the KEDA interceptor,
and the interceptor sits on the environment's public route — where the web
process is the only thing behind it. So it is the web Deployment that parks
and the web Deployment a request wakes; a `worker`, a `service`, a `cron` and
a `task` are left running whatever the mode says. A `service` is the one that
looks as though it ought to be covered and is not: its callers are its own
siblings inside the environment and never touch the Gateway, so there is no
signal to idle it on and none to wake it on — and a `worker` has no requests
at all. A per-workload field would be a declaration with no mechanism under
it.

The consequence is that a **mixed posture is not expressible**: a project
cannot park its web process and keep one service always-on, because the policy
is the project's, all of it or none of it. A workload that must stay warm is
met two ways today — the project does not idle (`scaleToZero.mode: never`, or
`runtime.notRequestDriven` where the reason is that something of it does work
nobody asked for), or the workload moves into a project of its own, which is
the honest answer when what it runs does not in fact ship as one thing with
the rest.

If a signal for the others ever exists — an interceptor that can front an
in-cluster Service — the override is designed rather than guessed at: a
`scaleToZero` block mirroring the project's three fields, on `runtime` for the
web process and on a `processes` entry for the rest, absent meaning take the
project's, so every declaration written before it keeps meaning what it means.
Until such a signal exists only the web process's copy could be honoured,
which is what the Project's own field already says — so it is a sketch here
and not a field (#303).

`runtime.singleton` is the *web* workload two of which must never run at once.
It becomes `strategy: Recreate` on the Deployment — the old pod stops before the
new one starts — and a CEL rule on the CRD **refuses** `replicas` above one
rather than clamping it, because a clamped value reads back as a setting that
did not take. For a stateless web application none of this is needed, which is
why the default is a rolling update; for one with a poller, a scheduler or an
ingest loop in the same binary as the web server, the few seconds a rolling
update overlaps two pods are that loop running twice against a shared store.
Leader election stays the application's problem; not overlapping it during a
deploy the platform initiated is the platform's.

A worker says the same thing on its own entry, with `processes[].singleton`,
and it reads exactly the same way: `strategy: Recreate` on that worker's
Deployment, `replicas` above one refused rather than clamped. It is there
because the web process is the one that least needed it — a poller moved out
of the web binary into a worker, which is the arrangement that makes the web
process safely scalable, would otherwise take the API server's default rolling
update, and at one replica that surges to a second copy and takes none away.
One replica does not imply the declaration: a queue consumer at one replica is
usually fine overlapping, and inferring the constraint from the count would
make the count mean two things. A `cron` process is refused it — whether two
of its runs may overlap is `concurrencyPolicy`, whose default is `Forbid` —
and so is a `task`, which is one run per deploy and has no second copy to
overlap.

`runtime.command` and `runtime.args` start the application container, the same
two fields a process has and in the same exec form. `previewArgs` replaces
`args` in a preview: same commit, same artifact, different flags — which is
the point, since the artifact is built once and never rebuilt, so a preview
that needs a fake data source cannot get one by building a second image. An
empty `previewArgs` is no override, the reading an empty `previewValue` gets
too. All three are in the snapshot, so a rollback restores the arguments the
release ran with.

`runtime.health` is how the platform finds out whether an application is
*working* rather than merely started, and it is the reason `status.phase:
Live` means anything: the phase is the Deployment's availability, which is its
ready replicas, which is the readiness probe. Every environment gets three
probes — startup, readiness, and, where a `path` is declared, liveness. With
no path the check is a TCP connect to the container port: a weaker claim than
an HTTP 200, and a much better one than asserting a readiness nothing
established. `GET /` is deliberately not the default, since plenty of
applications answer it before they are ready and one that 404s there would
never become Ready at all. The startup threshold is separate from the liveness
one because slow startup is a legitimate state and a threshold loose enough to
tolerate it is too loose to catch a wedge afterwards.

A process's `health` is opt-in and has to name its `port`, both refused at
admission rather than ignored: a worker publishes no port to fall back on, and
a run's verdict — a scheduled one's or a deploy task's — is its exit status, so
the field is refused on a `cron` process and on a `task` outright.

`runtime.security` is the posture the application's containers run under, and
until it existed nothing was applied to them at all: they ran as whatever the
image happened to be, in a namespace deliberately relaxed to `privileged` for
the build tooling's sake. That relaxation is rootless BuildKit's requirement,
not the application's, which is exactly why the lever is here, per workload,
rather than at the namespace level — and why nothing in it reaches a build
job.

A project that declares nothing gets the platform's default rather than
nothing: the runtime's own seccomp profile, and no privilege escalation.
Those two cost a working image nothing. The three that would — a read-only
root filesystem, dropped capabilities, a non-root user — are what a project
asks for here, because an image that writes into its own filesystem is
ordinary and a default that broke it would break it on upgrade with nothing
said. Every field is zero-means-the-default, the reading `health`'s timings
have, so a posture is taken back off by clearing it.

**The posture is per project *and* per workload (#399).** `runtime.security`
is the unit's, and a `processes[].security` is that workload's own, merged
over it **field by field**: a field the workload sets wins, a field it leaves
unset inherits the project's. That is what a unit whose images no longer share
a base needs — since a project ships up to five of them, each with its own
base, a single `runAsUser` across four workloads is luck rather than design,
and `fsGroup` has the same shape where two workloads mount two claims.

Every field is zero-means-**inherit** there, on the same reading the unit's
zero-means-the-default has, so a workload adds to the project's posture or
points it somewhere else and **cannot take a constraint off**. A constraint
only some workloads can bear is declared on those workloads rather than on the
project. An empty `security` block is no override at all, which is how one is
taken back off through an API that never distinguishes an absent key from a
cleared one; a withdrawn override leaves the workload the way a withdrawn
posture does, because both halves are written whole on every reconcile.

**The web process has no entry in `processes`, so its posture is
`runtime.security` — which is exactly where it already was.** That asymmetry
is stated rather than modelled around: a second spelling of the web process
would be a workload nothing routes to answering to the name of the one that is.

Nothing is stored resolved. Both halves are snapshotted into the Release, so a
rollback restores the resolution exactly by restoring the two declarations it
was computed from, and `GET /releases/{name}/config-diff` reports the
**resolved** posture per workload beside the unit's own row — a rollback that
took one worker off its own uid is invisible on a diff that reported only a
unit posture that never moved.

It is snapshotted into the Release with the rest of the runtime, so a
rollback restores the posture that release ran under, and a workload that
cannot start under it says so on its Environment — `WorkloadAvailable=False`
with reason `ContainerRefused` for a container the kubelet would not create,
`RestartingUnderPosture` for one that starts and exits under a declared
posture — with the constraints in force named in the message.

The refusal is looked for across **every** workload the environment keeps
running, not only the web process, and whether or not the URL is answering: a
refused worker is refused while production serves perfectly well, and a
release rolling out behind pods still serving the last one is exactly where
nothing else would mention it. One refused pod is the whole diagnosis — the
refusal is of the pod spec, so every replica of that workload carries it — and
the environment goes `Degraded` rather than staying `Deploying`, because
nothing here changes on its own. `status.refusal` carries the pod and the
container behind the sentence, which is the operator's half of it. A *run's*
pod is never counted here: a deploy task's refusal gates its own deploy and a
scheduled run's is the schedule's failure, and neither is the application
being down.

**`runAsNonRoot` without `runAsUser` is refused before it can be deployed
(#393).** That setting makes the kubelet *verify* the image does not run as
uid 0, and it can only do that against a uid — `USER node` and `USER nonroot`
are names, resolved inside the image where the kubelet cannot look, so every
pod is refused with "cannot verify user is non-root" however non-root that
user is. The platform reads the image's own `USER` when it pushes or acquires
the digest, records it as `status.artifact.user` on the Build, and fails the
build that would have produced the Release — naming the workload, the image,
the user it found and `runAsUser` as the fix. It is not refused at the API,
because the same request is exactly right for an image whose `USER` is a
number.

That check is made **per workload**, against the posture that workload
actually runs under: one whose own `runAsUser` is the right number for its
image is left alone even where the unit names none, and one whose inherited
`runAsNonRoot` has no uid behind it is refused naming that workload alone. The
evidence is already per artifact (#300); this reads it against the declaration
that applies to it.

`runtime.init` and `processes[].init` are what a workload needs done inside the
volumes it mounts before its own process starts (#348). A `volume` claim hands
a workload an empty filesystem, and a good deal of vendored software will not
start on one: Gitea wants a directory tree that exists before it looks at it,
Home Assistant a `configuration.yaml` it may then rewrite. `fsGroup` settled
who *owns* a volume and `files` settled how a file reaches a *container* —
neither creates a directory, and a file the platform mounts is read-only, so
neither of them gets an empty volume into a state the process will accept.

Three properties make it the shape it is, and they are the whole of why this
is an init step in the model rather than a task allowed to mount another
process's volume (which would change the one-process rule the `volume` claim
exists to keep) or a documented boundary (which would leave two working
features still adding up to an application that does not start):

- **It is declarative, not a command.** The vocabulary is two typed steps and
  there is deliberately no third that takes an argv. The platform runs them
  itself, in an init container in the workload's own pod, from the operator's
  own image — the same rule the KEDA install job follows, and no shell at any
  point.
- **It is idempotent by construction.** This runs on every start, not only the
  first. A directory that is already there is left exactly as it is, mode and
  owner included; a seed is written only where the destination does not exist.
  A second deploy therefore never clobbers what the application wrote.
- **It runs under the posture the workload itself runs under.** The init
  container takes the same resolution the application's container does —
  `runtime.security` with that workload's own `security` merged over it
  (#399) — the same user, the same dropped capabilities, the same read-only
  root filesystem, so a directory it creates comes out owned by the process
  that will use it. That is `fsGroup` doing the work, and it is why there is
  no `owner` to declare here and no `chown` to run.

Every path is relative to the claim's own `mountPath`, and the pattern that
validates one admits no leading slash and no `..` — so a step cannot reach out
of the volume because there is nothing spellable out there to reach. `mode` is
octal written as a string, because JSON's numbers are not octal and `0750`
decimal is 1356 octal.

The declaration is snapshotted into the Release with the rest of the runtime
and the process list, so a rollback restores the tree and the seeds that
release started with. What cannot be honoured is refused before any pod
exists — a volume this workload does not mount, one it mounts read-only, a
seed from a file the release does not carry — with `Ready=False`, reason
`VolumeInitInvalid`, on the Environment. A step that fails at run time lands on
the same condition, reason `VolumeInitFailed`, in the step's own words: the
program writes them to the pod's termination log and the environment reports
them, rather than leaving a workload that never becomes ready.

Reconcile: ensure per-project namespace, register the git webhook via the Connection
(signing secret generated per project), validate that the referenced Connections carry
the required capabilities. The production Environment is created by the first
production-branch Build — an Environment cannot exist before there is a Release to run.

## `Build` (namespaced: kitchen-system)

One build execution for one commit. Created by the webhook handler (or the API for manual
rebuilds). Immutable spec; all the action is in status.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Build
metadata:
  name: my-shop-bld-8f3a2c1
  ownerReferences: [Project my-shop]
spec:
  projectRef: { name: my-shop }
  git:                                  # the commit this builds. Empty for an *acquisition* —
    sha: 8f3a2c1d...                    # a Build of a project whose source is an image, which
    branch: feat/checkout               # resolves a digest and runs no builder. A SHA and a
    message: "Add checkout flow"        # branch, or neither: half a commit is refused at
    author: bermos                      # admission, and nothing fakes one
    pullRequest: 42                     # unset for direct pushes
  acquire:                              # the other member of the same union: what a Build with no commit
    reference: ghcr.io/vendor/app:stable  # takes. The reference as the project declared it when this was
    digest: sha256:cd34...              # created, and the digest to take — set by the poll, which has
    trigger: poll                       # already asked, and by "take exactly this". Empty digest is
                                        # "whatever the reference names now", which is the seeding's case.
                                        # trigger: seed | poll | request
status:
  phase: Succeeded                      # Queued | Running | Succeeded | Failed | Cancelled
  acquisition:                          # what an acquisition resolved, from where and when. Absent on a
    reference: ghcr.io/vendor/app:stable  # build of a commit, whose answer to all of this is the commit
    image: ghcr.io/vendor/app@sha256:cd34...
    previous: ghcr.io/vendor/app@sha256:ab12...   # what it replaced; empty for a project's first
    trigger: poll                       # what asked for it
    pinned: false                       # whether the project named a digest rather than a tag
    resolvedAt: ...                     # when the registry was asked
  detectedFramework: nextjs
  dockerfileTarget: web                 # the stage this build was told to produce, as it was told
                                        # it: unset for the file's last stage, and never recomputed
                                        # from settings that have moved since
  config:                               # the commit's own kitchen.json, when it carried one
    path: kitchen.json                  # relative to the repository root
    build: { strategy: dockerfile }     # only the keys the file actually set
    runtime: { port: 3000 }
    env: [{ name: NODE_ENV, value: production }]
  image: harbor.example.com/kitchen/my-shop@sha256:ab12...   # digest, never a tag
  artifact:
    repository: harbor.example.com/kitchen/my-shop
    digest: sha256:ab12...              # the identity everything downstream keys on
    user: node                          # the image's own USER, read from its config;
                                        # empty means "not read", never "root"
    attestedAt: ...                     # when the platform attached its build record
    keyID: 9f2c...                      # what it was signed under
    evidence:                           # an index of what is attached, not a copy of it
      - predicateType: https://kitchen.bermos.dev/attestation/build-record/v1
        source: platform                # the reconciler's account of the build
        manifest: sha256:7c30...
      - predicateType: https://slsa.dev/provenance/v1
        source: builder                 # BuildKit's own, countersigned by the platform
        manifest: sha256:7c30...
      - predicateType: https://spdx.dev/Document
        source: builder
        manifest: sha256:7c30...
    message: ""                         # why it is unattested, when it is
  source:                               # how the commit reached the branch, as the provider tells it
    provider: github                    # whose claim this is; the platform did not watch the review
    pullRequest: 42
    title: Add checkout flow
    author: alice                       # opened it
    mergedBy: bob
    approvers: [bob]                    # approvals that still stood when this was asked
    selfApproved: false                 # recorded separately: a self-approval is an approval
    independent: true                   # somebody other than the author approved
    required: true                      # the project asked for this, so "none" would mean something
    checkedAt: ...
  cache:
    enabled: true
    warm: true                          # the cache existed when this build started
    ref: harbor.example.com/kitchen/my-shop:buildcache
    mode: max                           # empty for a buildpacks build
    message: ""                         # why there was no cache, when there was none
  gates:                                # what each gate did — not what it found
    - name: trivy
      phase: Completed                  # it ran. Whether what it found is acceptable is a
      source: platform                  # policy question about the environment, answered at promotion
      predicateType: https://kitchen.bermos.dev/attestation/quality-gate/v1
      attested: ...
      finishedAt: ...
  startedAt: ...
  duration: 143s
  conditions: [...]
```

Reconcile: run a build job in the project namespace, push to the registry Connection,
post a status check back on the commit, and on success **create a Release**.
Retention: keep the last N Builds per project (configurable).

**A Build with no commit runs no builder.** A project whose source is an image is
deployed by an *acquisition*: the reconciler resolves what each of the unit's
references names, freezes the digests onto a Release and hands it to the same
promotion a build hands one to. The Build object stays because everything downstream
of it — `status.artifact`, the evidence index, the quality gates, the audit chain, the
build screens and the CLI — keys on a Build, and `status.acquisition` is what makes it
answerable months later: what was followed, what arrived, what it replaced, and what
asked.

Three things create one. The project's own first reconcile seeds one, so that
connecting software deploys it. The **digest poll** creates one when a watched tag
stops naming the digest that was acquired — a manifest HEAD per watched reference per
`Kitchen.spec.builds.imagePollInterval`, never a pull, and never at all for a
reference pinned to a digest. And `POST /projects/{name}/acquisitions` creates one on
demand, optionally naming the digest to take. A registry that cannot be read is an
acquisition that **fails saying so**, once — the project's `status.imagePoll.message`
is the record that it has already been said — and what is already running goes on
running.

Which job that is comes from the strategy. `dockerfile` runs **BuildKit** on the
repository's own Dockerfile, with the commit as a git context BuildKit fetches itself.
`buildpacks` runs the **Cloud Native Buildpacks** lifecycle (`creator`, in Paketo's
jammy builder) over the repository, which needs the source on disk first — so the job
clones the commit in an init container and hands the lifecycle a directory. A buildpacks
build needs none of the privileges a BuildKit one does: it runs as the builder image's
own unprivileged user throughout.

Either builder reports the digest it pushed through the pod's termination message —
BuildKit as JSON metadata, the lifecycle as its TOML report — which is what puts a
digest rather than a tag in `status.image`.

`status.artifact` is that digest as an identity rather than as a string, and what
the platform has asserted about it. On success the reconciler signs a build record —
project, commit, strategy, framework, times — and attaches it to the digest through
OCI referrers, under Kitchen's own predicate type rather than SLSA's, because a
reconciler's account of a build it orchestrated is a weaker claim than provenance
from the builder itself. Failing to attest does not fail the build: the image is real
and the deployment that follows is honest about what it is running. What an unattested
artifact cannot do is satisfy a policy that requires evidence, which is where the
consequence belongs.

The builder is asked for two more, which the reconciler cannot make on its own:
**SLSA provenance** — the source commit BuildKit actually resolved, the base images it
actually pulled and their digests, and what it was invoked with — and an **SBOM** of the
finished image. BuildKit leaves both unsigned in an index beside the image; the
reconciler harvests them, restates each about the artifact's digest, and signs them
under the platform's key. `status.artifact.evidence[].source` says which claims are the
builder's and which the platform's, because the signature on both is the same one.

Asked for attestations, BuildKit pushes an index rather than a bare manifest and reports
*its* digest — but the statements inside are about the image manifest, so that is what
`status.artifact.digest` and the Release name. Only the Dockerfile strategy produces
provenance: the buildpacks lifecycle is not BuildKit and emits none, and minting one from
the reconciler's own knowledge would be the conflation the split exists to prevent.

`status.artifact.evidence` is an index — predicate types and the manifests to fetch them
by — and not a copy: the attestations live in the registry against the digest, which is
the only copy an installation leaving Kitchen would keep.

`status.source` is how the commit reached the branch, as the git provider tells it —
which pull request it arrived through, who opened it, and whose approvals still stood
when the platform asked. It is resolved **before the Job is created**, so a project with
`source.requirePullRequest` set refuses an unreviewed production commit before spending
any compute; the Build stays, with reason `SourceUnreviewed`, because refusing without a
record would be the platform quietly dropping changes. The provider is asked rather than
the commit read, because a squash merge produces a commit that names neither the request
nor the approver. Preview builds are never asked to prove they were reviewed: they are
what produces the thing being reviewed. A provider that cannot be reached is recorded as
a check that could not be made and does not refuse anything.

`status.gates` is what the platform's **quality gates** did. Each is a pod: an image
somebody else wrote, pointed at the artifact, writing findings to a file that a second
container stores in the registry and the operator signs. They run after the build is
terminal, so they hold nothing up, and they record **findings and never a verdict** —
`Completed` means the gate ran, whatever it found, and `Failed` means it did not run at
all. Whether findings are disqualifying is a property of the environment being promoted
to. Results produced elsewhere are ingested through `POST /builds/{name}/gates` and
carry who reported them, because a scan somebody submitted is a claim about an artifact
and a claim about who said so. See [COMPLIANCE.md](COMPLIANCE.md).

`status.cache` is what the layer cache did, and it is on every build including the
ones that had none. The cache lives in the registry the project already pushes to,
under the same credential: BuildKit exports a cache manifest to
`<image repository>:buildcache` and imports it on the next build, and the buildpacks
lifecycle exports a cache image to `:buildcache-cnb` — different tags, because the
two formats cannot read each other. `Kitchen.spec.builds.cache` configures all of it:
`mode: max` keeps the intermediate layers, so a source change still reuses the
dependency install above it, and `scope: branch` gives each branch its own cache
instead of one per project.

What is recorded is what the platform can stand behind. `warm` says the cache existed
when the build started; nothing says how many layers were reused, because neither
builder reports it. A cold build says so on the commit status too — "image built and
pushed in 4m12s, cache cold" — so that a slow first build does not read as a
regression.

Two things follow from the cache being a tag in the registry. Whether a registry
accepts a cache manifest cannot be asked, only attempted: BuildKit is told
`ignore-error=true`, so a registry that refuses one warns instead of failing a build
whose image is already pushed, and the reconciler notices on the *next* build — the
last build exported and the cache is not there — and builds that one without a cache,
saying so on `status.cache.message`. That is deliberately not sticky: the build after
tries again, so an installation that moves to a registry which does keep caches
recovers on its own. And the default scope is bounded without anything pruning it —
one tag per project, overwritten in place, so what grows is the unreferenced blobs
underneath, which the bundled registry reclaims while running. `scope: branch` is not
bounded: it leaves one tag per branch that ever built, which nothing removes, and is
the setting to weigh against the registry's own retention.

A project on `strategy: auto` takes `Kitchen.spec.builds.defaultStrategy`; when that
is `auto` too, the platform reads the repository at the commit under build and decides
for itself. It lists the build's root directory through the project's git Connection —
no clone, two requests — and matches the first rule that fits: a Dockerfile wins over
everything, then `package.json` and what is in its dependencies (`next`, `nuxt`,
`@sveltejs/kit`, `@remix-run/*`, `@nestjs/core`, `astro`, `react-scripts`, `vite`),
then `go.mod`, `requirements.txt`/`pyproject.toml`/`Pipfile`, `Gemfile`,
`pom.xml`/`build.gradle`, a `.csproj`, and last a bare `index.html`. Whatever it finds
lands in `status.detectedFramework`, and the build page shows it.

A framework that has no server of its own — a Vite or create-react-app bundle, an Astro
site with no adapter, a directory that is already a website — is built into an image
that serves it with NGINX, by telling the lifecycle so (`BP_WEB_SERVER`,
`BP_WEB_SERVER_ROOT`, and the project's own `build` script through
`BP_NODE_RUN_SCRIPTS`).

Two things do not happen. A repository nothing matches **fails the build** with *"no
Dockerfile and no framework detected"* rather than handing a builder a repository it
cannot build; and a repository the platform cannot *read* right now — a provider that is
down, a credential that stopped working — leaves the Build `Queued` with reason
`SourceUnreadable`, because nothing about the commit caused that. A repository that
cannot be read *at all* — it is not there, or the connection's credential may not see it
— is neither: it fails with reason `RepositoryUnreadable`, saying so, because it is not
going to appear because a Build waited, and because nothing was read for a verdict about
a directory to be about. Detection runs only
where configuration left the question open: `strategy: dockerfile` never reads the
repository, and `strategy: buildpacks` reads it only to learn what to tell the
lifecycle, building anyway if it cannot.

Beside detection, and independently of it, the build reads the commit's own
`kitchen.json` — one more request, made whatever the strategy is, from the project's
root directory. What it declares lands on `status.config`, and what it declares wins:
the strategy, the Dockerfile and the stage of it the file names configure the build
Job, and its runtime,
variables and processes are merged over the project's into the Release this build
writes. So a rollback replays the configuration its commit declared rather than
today's, and a build of an old commit builds it the way that commit asked. The file
is recorded on the Build before the Job exists, because it is read once and applied
twice — a later reconcile writes the Release, and re-reading the repository there
would spend a second request answering a question that has an answer, and answer it
differently if the branch moved underneath. A file that is *wrong* fails the build
with reason `ConfigInvalid`; one that cannot be read parks it exactly as detection
does. What it may and may not say is [docs/CONFIG.md](CONFIG.md).

The job's output reaches ClickHouse the same way every other container's does — the
node's collector tails it — so the build pod is labelled `kitchen.bermos.dev/build`
and kept for an hour after it finishes, long enough for the last lines to ship.

## `Release` (namespaced: kitchen-system)

An immutable snapshot: image digest + the project's config *as it was at build time*.
This is what makes rollback instant and exact — old Releases don't drift when the
Project spec changes later.

> Named `Release`, not `Deployment`, to avoid colliding with `apps/v1 Deployment`
> in every kubectl session and conversation forever.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Release
metadata:
  name: my-shop-rel-000042
  ownerReferences: [Project my-shop]
spec:                                   # fully immutable (CEL rule on the CRD)
  projectRef: { name: my-shop }
  buildRef: { name: my-shop-bld-8f3a2c1 }
  image: harbor.example.com/kitchen/my-shop@sha256:ab12...   # the web process's
  workloads:                            # what each *other* workload was built
    - name: api                         # to, for one with a build of its own
      image: harbor.example.com/kitchen/my-shop-api@sha256:9f2c...
  configSnapshot:                       # frozen copy of Project.spec.env,
    env: [...]                          # runtime, processes and files
    runtime: { port: 3000, resources: {...} }   # port resolved: a project that
                                                # named none gets the detected
                                                # framework's, frozen here
    processes: [...]                    # the other workloads as they
                                        # stood at build time
    files: [...]                        # the configuration files, content and
                                        # all — except a secret one's, which is
                                        # a credential and is not in here
status:
  environments: [my-shop-production, my-shop-pr-42]   # where it's live (informational)
```

`configSnapshot.runtime.init` and each `configSnapshot.processes[].init` freeze
what a workload prepares inside its volumes, which is what makes a rollback
restore the tree and the seed content that release started with rather than
today's. A seeded file's content comes from `configSnapshot.files` for a plain
file and from the project's Secret for a secret one, on the same terms the
mounted files do.

`configSnapshot.files` freezes a *plain* configuration file's content, which is
what makes a rollback restore the file that release ran with byte for byte.
Every workload's pod template carries a digest of the files it reads
(`kitchen.bermos.dev/config-files-revision`), so a release differing only in a
file's content still rolls the workloads that read it — without it the platform
would rewrite the file and nothing would restart. A *secret* file's content is
deliberately not here: a Release is readable by everyone who may read the
project, so the snapshot carries the declaration and the content stays in the
Secret, mounted from it and covered by the secrets digest beside it.

`workloads` is the other half of what makes a rollback exact for a project that
ships more than one image. The snapshot freezes what each workload *is*; this
freezes what each was *built to*. Restoring a release restores both, so a
workload added since does not appear and one whose image moved goes back to the
image it had — never today's. It is empty for the great majority of projects,
which ship one image and run it everywhere.

Immutability is a CEL transition rule (`self == oldSelf`) on the CRD, not a webhook:
the platform ships no admission webhook of its own, and the rule is stronger than one
anyway — an edit is refused by the API server before it is ever written, with the
message `Release spec is immutable`. A snapshot that could be edited afterwards would
not be a snapshot, and instant rollback would stop being exact, which is the whole
justification for the kind.

Reconcile: **maintain `status.environments`** by watching Environments and mapping each
back to the Release it references. The relationship is only declared in the other
direction (`Environment.spec.releaseRef`), so without this, answering "which release is
production on?" means listing every Environment and matching refs by hand. It is
informational and eventually consistent by design.

Retention: keep the newest `Kitchen.spec.builds.releaseRetention` Releases per project
(10 by default; `0` keeps every one). A Release an Environment still points at is kept
on top of that count however old it is — an environment parked on release 3 while forty
more were built still has release 3 to roll back to. The image the pruned Release named
is left in the registry: reclaiming that needs a per-provider delete API and a count of
what else shares the digest.

## `Promotion` (namespaced: kitchen-system)

One request to move one Release into one Environment, with the evaluated decision on
its status. It is how an artifact travels a project's staged pipeline
(`Project.spec.promotion.stages`): the same image digest at every stage, never rebuilt,
judged at each boundary by that environment's `spec.requirements` through the policy
engine. Rollback is the same request at an older Release.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Promotion
metadata:
  name: my-shop-promo-1a2b3c4d5e
spec:                                   # fully immutable (CEL rule, like Release)
  projectRef: { name: my-shop }
  environmentRef: { name: my-shop-production }
  releaseRef: { name: my-shop-rel-000042 }
  requestedBy: anna@example.com         # or system:controller/build, system:controller/promotion
  trigger: manual                       # manual | automatic
  reason: ship 1.4                      # optional; lands in the audit record
status:
  phase: Blocked                        # Pending|Evaluating|Allowed|AllowedWithException|Blocked|Applied|Failed
  verdict: blocked                      # allowed | allowed-with-exception | blocked — exactly three
  decisionID: 0d9a1f7e-…                # the stored decision: fired rules, replayable input
  unmetRules: [require-sbom]            # the fired, unwaived rules by stable id
  message: "blocked by bundle sha256:…: require-sbom (the artifact carries no…)"
```

The reconciler resolves the three references (they must tell one story — a mismatch is
`Failed`), materializes the policy input from the artifact's stored evidence, evaluates
through `internal/policy`, records the decision — audit record fail-closed, decision
store, `promotion-decision` attestation on the artifact — and only when allowed flips
`Environment.spec.releaseRef`, mints the `deployment/v1` attestation, and chains the
next stage's Promotion where `autoPromote` says so. `Blocked` and `Failed` are terminal
because the spec is immutable: a retry is a new Promotion, and the old one stays as the
record of what was refused and why. An environment with no `requirements` accepts
anything — the build controller then skips the Promotion entirely and flips the
Environment directly, exactly the pre-pipeline behaviour; previews are never routed
through a Promotion at all (the preview is the review vehicle).

## `Environment` (namespaced: kitchen-system)

A running instance of a Release with a URL. Exactly one `production` per Project;
`preview` Environments are created/deleted by the operator from PR events.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Environment
metadata:
  name: my-shop-pr-42
  ownerReferences: [Project my-shop]
spec:
  projectRef: { name: my-shop }
  type: preview                         # production | preview
  releaseRef: { name: my-shop-rel-000042 }   # rollback = edit this line
  preview:
    pullRequest: 42
    branch: feat/checkout
  dataClass: internal                   # ceiling this environment is rated to hold — the promotion
                                        # rule refuses a project classified above it; absent = unrated
  residency: CH                         # declared location of its data; absent inherits
                                        # Kitchen.spec.residency in the compliance inventory
status:
  phase: Live                           # Pending | Deploying | Live | Degraded | Terminating
  url: https://my-shop-pr-42.apps.example.com
  observedRelease: my-shop-rel-000042
  idle: true                            # parked: allowed to scale to zero, and scaled to zero. It is
                                        # observed from the Deployment the autoscaler moves, not decided
                                        # here — and it is the signal a claim's preview infrastructure
                                        # parks and wakes on
  history:                              # how past releases stopped being current,
    - release: my-shop-rel-000041       # newest first, last 10
      from: "2026-08-13T09:12:00Z"      # when it became / stopped being current
      to: "2026-08-14T10:30:00Z"
      reason: promoted                  # promoted | rolledBack | superseded
      by: my-shop-bld-abc123def456-xk2p9   # the promoting Build, or the API caller
  gitReport:                            # what was last posted back to the git provider
    revision: ab12cd34ef56              # absent where nothing is reported to
    state: success                      # in_progress | success | failure | inactive
    url: https://my-shop-pr-42.apps.example.com
    commentID: "204819274"              # the PR comment that is rewritten in place
    error: ""                           # why the last post did not land
    at: "2026-08-14T10:30:04Z"
  rescan:                               # what the continuous re-evaluation pass last found
    phase: Evaluated                    # Scanning | Evaluated | Failed — Failed is "nothing is
    release: my-shop-rel-000042         # known", never "it found problems"
    artifact: registry.apps.example.com/my-shop@sha256:...
    jobName: my-shop-pr-42-scan-my-shop-rel-000042
    startedAt: "2026-08-24T03:14:00Z"
    finishedAt: "2026-08-24T03:16:11Z"  # the interval is counted from this
    dataSnapshot: grype-db:sha256:...   # which vulnerability database; `unpinned:` = dated, not
    findings: 41                        # reproducible
    verdict: blocked                    # allowed | allowed-with-exception | blocked
    unmetRules: [max-severity]
    decisionID: 0d9a1f7e-...            # the stored decision, with the whole input
    message: blocked by bundle sha256:...
  processes:                            # the other workloads, as last seen
    - name: worker
      type: worker
      workload: my-shop-pr-42-worker    # the Deployment or CronJob it materialized as
      replicas: 2
      readyReplicas: 2
    - name: api
      type: service
      workload: my-shop-pr-42-api
      address: http://my-shop-pr-42-api.kitchen-my-shop.svc.cluster.local:8080
      image: harbor.example.com/kitchen/my-shop-api@sha256:9f2c...   # its own build's
      replicas: 1
      readyReplicas: 1
    - name: migrate                     # a deploy task, which materializes no
      type: task                        # standing object: its runs are its Jobs
      release: my-shop-rel-000042       # the release this run was made for
      attempt: 3                        # which run of it this environment has made
      lastRun:
        name: my-shop-pr-42-migrate-3
        phase: Succeeded
    - name: nightly-report
      type: cron
      workload: my-shop-production-nightly-report
      schedule: "0 3 * * *"
      suspended: false                  # true on a preview that did not opt in
      active: 0                         # runs in flight
      lastRun:                          # whatever it did
        name: my-shop-production-nightly-report-29387520
        phase: Failed                   # Running | Succeeded | Failed
        startedAt: "2026-08-24T03:00:04Z"
        finishedAt: "2026-08-24T03:00:37Z"
        message: "BackoffLimitExceeded: Job has reached the specified backoff limit"
        refused: false                  # true when the kubelet would not create its
                                        # container at all: the run never started
      lastFailure: {...}                # the most recent one that failed
  refusal:                              # a container the kubelet would not create,
    workload: worker                    # absent when there is none
    pod: my-shop-pr-42-worker-7d9f-x2k
    container: app
    reason: CreateContainerConfigError
    message: "the container of worker could not be started: ..."
  conditions: [...]                     # Ready, RouteProgrammed, WorkloadAvailable,
                                        # DeployTasksComplete (only where the release
                                        # declares one), PreviewProtected (previews only),
                                        # ScaleToZero (where the platform idles anything)
```

`history` answers what `releaseRef` alone cannot: **how** the environment moved off each
release. `promoted` — a fresh build's release was auto-promoted over it; `rolledBack` —
someone moved the environment back to an older release; `superseded` — anything else
replaced it (a manual move forward through the API, or a direct spec edit, where `by`
stays empty).

`rescan` is both the answer and the working state of the continuous
re-evaluation pass: it is where the sweep reads its next move, which is what
lets the sweep itself be stateless between passes and the interval be counted
per pair rather than platform-wide. A release move clears it — the answer was
about the artifact that was running, and carrying it forward would report a
scan that never happened. `GET /compliance/drift` is the same information
across the estate, joined to what was decided at promotion.

On a deploy task the row carries two fields nothing else uses: `release` — the
Release its last run was made for — and `attempt`, how many runs of it this
environment has ever started. Together they are what makes "once per deploy" a
fact rather than a hope: the reconciler holds nothing between passes, so a run
is started exactly when the release recorded here is not the one being
deployed. A rollback to a release the environment ran before is a new deploy
and runs the task again; clearing `release` is what a retry through the API is.

`processes` is why a scheduled job that stopped working is something a person
trips over rather than something they have to go and check. `lastFailure` is
kept until a **later failure** replaces it, never until a success does: a job
that fails four nights in five would otherwise read as healthy on the fifth,
and a `CronJob` whose pods fail silently is the classic way the feature
disappoints. Every terminal run also lands in the activity feed as
`run.succeeded` or `run.failed`, naming the process and the run.

`gitReport` is bookkeeping for [deploy status](#deploy-status-back-on-the-commit): an
Environment reconciles far more often than it changes, and without a record of what was
already said, every pass would post the same deployment status again. It is also where a
refused post is recorded — never as a condition, because reporting is commentary on a
deployment rather than a part of it.

Reconcile (the heart of the operator): in the project namespace, ensure an apps/v1
Deployment (from the Release's image + config snapshot), Service, `HTTPRoute` attached to
the shared Cilium `Gateway` (host = generated or custom domain), and synced Secrets.
Auto-promotion: a successful Build on `productionBranch` → new Release → the operator
updates the production Environment's `releaseRef`. Preview lifecycle: PR opened →
Environment created; PR closed/merged → deleted after `ttlAfterClosed`.

A preview whose Project has `previews.protected` (the default) is routed through the
platform's forward-auth gate instead of straight at its Service: same HTTPRoute, gate as
the backend, and the application's address in a header the Gateway sets. Anonymous
requests land on the platform login and only signed-in ones reach the application, which
needs no changes — see [AUTH.md](AUTH.md). Production environments are never gated. If
protection is asked for on a platform that runs no gate, the Environment gets **no route
at all** rather than a public one, and says so in `PreviewProtected`.

Before any of that, the same pass runs the Release's **tasks** and waits for
them (#272). A task is a `batch/v1` Job named `<environment>-<process>-<n>`,
and while one is unfinished the reconcile applies nothing at all — no
Deployment, no Service, no CronJob, no route — so the release that was serving
carries on serving. A failed run puts the Environment in `Degraded` with the
run's message on `DeployTasksComplete`, which is also what the commit's deploy
status reports as a failure. Tasks run in declared order, one at a time, and a
release that arrives while the previous deploy's run is still going waits for
it rather than killing it.

A run whose pod the kubelet **refuses** is ended rather than waited for
(#391). `CreateContainerConfigError` and its siblings are not Job failures:
the kubelet retries the same doomed spec, `backoffLimit` is never approached,
and the Job reports one active pod for ever — so before this the environment
said a task "is running" for as long as somebody left it there. Such a run is
failed with the kubelet's own sentence, the condition carries the terminal
reason `TaskRefused` beside `TaskRunning`, the run's row records
`refused: true`, and the wedged Job is deleted once the verdict is written so
that the next release runs its own task. It is safe to delete precisely
because a refused container never ran: there is no output to lose. A scheduled
run gets the same treatment on its own row and in the activity feed, where
nothing is gated on it but a `Forbid` schedule would otherwise never fire
again.

The same pass materializes the Release's other `processes`: a **worker** becomes
a plain Deployment named `<environment>-<process>` with no Service and no route —
nothing addresses it, so there is nothing to publish and no certificate to
want — and a **cron** becomes a `batch/v1` CronJob of the same name, with the
process's schedule in UTC, its concurrency policy, and its timeout as
`activeDeadlineSeconds`. Both get the Release's image, the environment's
resolved variables, the registry pull secret the web process uses and one
variable of their own, `KITCHEN_PROCESS`: a single image serving three roles
has no other way of telling which one it is.

A run is not retried. `backoffLimit` is zero and the restart policy is `Never`,
so a scheduled run that failed is a failed run and the schedule is what tries
again; the platform keeps the last three successful runs and the last five
failed ones so a person can look at what happened. Everything a process
materializes carries `kitchen.bermos.dev/process`, which is how the pruning
finds what a Release no longer declares (the web process's own Deployment
carries no such label and so can never be caught by it) and how the collector
keys the log store — beside `kitchen.run`, which it lifts off the Job name the
Job controller stamps on every pod. A preview materializes only the processes
that opted in; the rest are reported `suspended` rather than silently dropped.

Where the platform idles environments (`Kitchen.spec.scaleToZero.enabled`), the
Project has not declared its workload `notRequestDriven`, and the Project's
`spec.scaleToZero` covers this type, the reconciler also writes an
`HTTPScaledObject` for it and addresses the application through the KEDA HTTP add-on's
interceptor rather than directly — as the Gateway's backend on an open environment, as
the gate's upstream on a protected one. The workload's replica count then belongs to
KEDA: the reconciler stops writing it, because the number it would write is the one the
autoscaler just moved. Everything that can go wrong here falls back to plain Deployment
routing with the environment's own replicas, and says why in `ScaleToZero` — an
application parked behind an interceptor nothing is watching would never come back.
`ScaleToZero` is also where an environment that does not idle says which of the
reasons applies: `NotRequestDriven` for a workload that does work nobody asked for,
`AlwaysOn` for a mode that simply does not cover this type.

## `Domain` (namespaced: kitchen-system)

A custom domain attached to an Environment (almost always production).

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: Domain
metadata:
  name: shop-example-com
spec:
  hostname: shop.example.com
  environmentRef: { name: my-shop-production }
  tls: acme                             # acme | cloudflared | none (inherits Kitchen default)
status:
  verified: true                        # DNS ownership check (TXT or CNAME)
  verification:                         # the exact records that satisfy it
    txtRecord: _kitchen-challenge.shop.example.com
    txtValue: kitchen-verify=…          # derived from the Domain's UID — stable, recomputable
    cnameTarget: my-shop.apps.example.com
  tlsMode: acme                         # the mode in effect after inheritance
  conditions: [...]                     # Verified, CertificateReady, RouteProgrammed
```

Reconcile: prove ownership first — a TXT record at `_kitchen-challenge.<hostname>`
carrying a token derived from the Domain's UID, or a CNAME from the hostname into the
base domain (pointing your name at the platform is the zone owner's own action, and the
record traffic needs anyway). `Verified` distinguishes the real failure modes: record
absent, record present with the wrong value, lookup failed. Once verified it stays
verified — routing must not flap with a record owners typically delete after setup.

What happens next follows the TLS mode in effect (`spec.tls`, or the platform's mode):

- **acme** — a per-hostname cert-manager `Certificate`, issued through the
  `kitchen-acme-http01` ClusterIssuer. HTTP-01 through the shared Gateway, not the
  platform's DNS-01: the Cloudflare token only writes to the base domain's zone, and a
  custom domain's zone is by definition someone else's. Issuance therefore needs the
  hostname to already resolve to the platform. Once the certificate's secret exists,
  the shared Gateway gains an HTTPS listener for the hostname.
- **cloudflared** — a custom hostname is a tunnel ingress rule in Cloudflare's control
  plane, which the token-managed tunnel gives the operator no way to write or read.
  `CertificateReady` is honestly `Unknown` and its message names the manual step.
- **none** — plain HTTP over the shared port-80 listener; no certificate.

The Domain reconciler never writes the Gateway or an HTTPRoute — `KitchenReconciler`
owns the shared Gateway's listeners and `EnvironmentReconciler` each Environment's
route; both watch Domains and include the verified ones. `RouteProgrammed` *observes*
the result: whether the route carries the hostname and the Gateway accepted it on the
listener this domain's traffic uses. Deleting a Domain removes its Certificate and
secret through a finalizer; the listener and hostname disappear because their writers
only ever include live, verified Domains.

## `ResourceClaim` (namespaced: kitchen-system)

A project's request for something the platform provisions: a database from a
`database`-capable Connection, a bucket from an `objectStore`-capable one, an
OAuth client from the platform's own identity provider, or a persistent volume
from the cluster's StorageClass. Generic on purpose — this is the plugin
abstraction.
OAuth client from the platform's own identity provider, or durable background
work from a `backgroundJobs`-capable one. Generic on purpose — this is the
plugin abstraction.
`database`-capable Connection, an OAuth client from the platform's own
identity provider, or the keys a worker connects to Inngest with from a
`backgroundJobs`-capable one. Generic on purpose — this is the plugin abstraction.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: ResourceClaim
metadata:
  name: shop-db
spec:
  projectRef: { name: my-shop }
  connectionRef: { name: neon-prod }
  type: postgres
  deletionPolicy: Retain                # Retain (default) | Delete — what deleting the claim does to the data
  dataClass: confidential               # never above the project's class; absent = unclassified
  recoveries:                           # point-in-time copies asked for; each becomes a sibling database
    - name: before-the-migration
      at: 2026-08-30T14:05:00Z          # inside the window the status reports
  promotedRecovery: ""                  # which copy the claim binds; empty binds the instance's own database
  backup:                               # postgres only; every field inherits the Connection's, then the
    enabled: true                       # platform's destination. Absent is the usual and the right answer
    schedule: "0 0 3 * * *"             # CloudNativePG's cron: SIX fields, seconds first, UTC
    retentionPolicy: 30d                # how long the destination keeps them; empty keeps everything
    destination:                        # a bucket of this claim's own; absent takes the platform's
      type: s3
      s3: { bucket: shop-archive, prefix: db, credentialsSecretRef: { name: shop-db-backup-destination } }
  config:
    previewMode: fresh                  # what previews get: the provider's declared mode (default), shared or none
    postgres:                           # what the database itself has to be; all of it optional
      version: "17"                     # major version; empty takes the platform's default
      extensions: [postgis, vector]     # created at bootstrap; unsuppliable ones fail the claim
      storage:
        size: 40Gi
        storageClass: fast-ssd
status:
  phase: Bound                          # Pending | Bound | Failed
  secretName: shop-db-binding           # binding keys: url, host, port, user, password, database
  instanceID: proj-abc123               # provider-side ID, opaque; what deprovisioning addresses
  instanceName: kitchen-my-shop-shop-db # what the provider calls it: kitchen-<project>-<claim>
  dataProvenance: production            # the provider's declaration: production | masked | synthetic;
                                        # absent = undeclared, treated by policy as the worst case
  residency: aws-eu-central-1           # where the provider reported the resource actually is
  previewMode: fresh                    # what previews bind to, resolved: branch | fresh | shared | none
  previewReason: a new, empty database… # the provider's own words, or why previews get nothing
  canIdle: true                         # a preview's own resource parks when the preview parks, and comes
  idleReason: a preview's Cluster is…   # back on wake. The reason is answered either way: a provider that
                                        # parks nothing has to say why an open pull request keeps paying
  branches:                             # one per preview Environment, under branch or fresh
    - environment: my-shop-pr-41
      id: br-def456
      secretName: shop-db-binding-my-shop-pr-41
      provenance: production            # a branch of a production database is production-derived
      idle: true                        # parked, because the preview reading it is. The data is untouched
  recovery:                             # what this claim can be recovered to, and what has been
    available: true                     # its provider can do it AND reports a window with something in it
    reason: neon can reconstruct…       # the provider's own account, and why not when it cannot
    window:                             # read from the provider every reconcile, never declared
      earliest: 2026-08-24T09:12:00Z
      latest: 2026-08-31T09:12:00Z
      observedAt: 2026-08-31T09:12:00Z
    recoveries:
      - name: before-the-migration
        at: 2026-08-30T14:05:00Z        # the moment this copy holds, not when it was made
        id: br-ghi789
        secretName: shop-db-recovery-before-the-migration
        provenance: production          # a recovery of production data is production data, earlier
        dataClass: confidential         # inherited from the claim: a new place the same data lives
        phase: Ready                    # Pending | Ready | Failed
    retained:                           # what a promote displaced and did not destroy
      - displacedBy: before-the-migration
        at: 2026-08-31T09:20:00Z
  backup:                               # read from the provider every reconcile, never echoed from the spec
    enabled: true                       # what the platform is actually configuring, after inheritance
    providerManaged: false              # true where the provider keeps its own history and takes no policy
    reason: backed up to s3://…         # why the state is what it is, in the words that name the fix
    schedule: "0 0 3 * * *"             # the policy in force, as the database actually holds it
    retentionPolicy: 30d
    destination: s3://kitchen-archive/databases   # described, never a credential
    lastBackup: 2026-09-03T03:00:11Z    # as the database's own operator reports them
    lastFailure: null
    firstRecoverablePoint: 2026-08-30T03:14:02Z   # the oldest moment the destination can reconstruct;
                                        # empty until a base backup has been taken and read back
    archiving: healthy                  # healthy | failing | unknown — is WAL reaching the destination
    archivingMessage: ""                # the database's own account of it
  conditions: [...]                     # Ready, Provisioned, PreviewBranchesReady, RecoveriesReady
```

Reconcile: require the `database` capability on the Connection (Pending, saying so,
until the Connection reconciler has validated it), provision through the plugin
(`internal/provider/database` — Neon, or CloudNativePG in this cluster), and
write the binding into a Secret in the project namespace, whose keys
`Project.spec.env` reads via `fromResourceClaim`. A provider refusal is phase
`Failed` with the provider's own words in the `Ready` condition. Under a
preview mode of `branch` or `fresh` — the provider's declaration, and the
default — preview Environments each get their own database and their own
binding Secret (`<claim>-binding-<environment>`), which the Environment's workload
reads instead of the shared one; the claim controller holds a finalizer on each
branched Environment and tears database and Secret down when the preview goes — that
plus preview URLs is the whole Vercel+Neon flow, self-hosted. `previewMode:
shared` binds previews to production's own database, and has to be asked for by
name; `none` binds them to nothing, and the Environment says so rather than
failing. `status.keepsPodsRunning` and `status.forcesRecreate` are the
provider's declarations about the workload, which the Environment reconciler
acts on. [docs/api/claims.md](api/claims.md) carries the matrix.

**A preview that idles takes its own infrastructure down with it**, where the
provider can (#294). The signal is the Environment's `status.idle` — allowed
to scale to zero, and scaled to zero — so the database goes down on the same
event the web process does and comes back on the same one; there is no second
timer and no second policy. CloudNativePG hibernates the preview's Cluster
(pods gone, volume kept) and the in-cluster Valkey scales its StatefulSet to
zero; a provider that cannot park says so in `status.idleReason` and keeps
running. `status.branches[].idle` is what each preview's own resource is
doing.

**`config.postgres` is the claim saying which Postgres it means**, because
"Postgres" is not one thing: an application that needs PostGIS, pgvector or a
time-series extension otherwise binds, gets a URL, and dies on a
`CREATE EXTENSION` in its first migration. The provisioner resolves the version
and the extensions to an image *before* it creates anything, and a claim it
cannot satisfy is phase `Failed` with a message naming what could not be
supplied and what is available instead — the refusal is the feature. Extensions
are created in the database at bootstrap, as superuser, so the application
never needs the right to create them itself. Everything in the block is applied
when the database is created and is not reapplied to a running one: a major
version is not something to change under a live Postgres, and asking for a
different database means asking for a different database.

**`spec.recoveries` recovers to a copy; `spec.promotedRecovery` decides to use
one.** Neither supported provider rewinds a database in place, and neither
needs to: recovering makes a *sibling* holding the data as it was at a moment
— for Neon a branch at a parent timestamp, for CloudNativePG a new Cluster
bootstrapped from the claim's own archive at a `recoveryTarget` — with a
binding Secret of its own (`<claim>-recovery-<name>`), while the application
keeps reading what it was reading. Setting `promotedRecovery` rewrites the
claim's own binding Secret with that copy's, which rolls every Environment
reading the claim; what it
displaced is recorded under `status.recovery.retained` and **kept**, on the
same reasoning that makes `deletionPolicy: Retain` the default. Removing a
name from `spec.recoveries` discards that copy and its data.

Whether any of it is possible is `status.recovery.available`, and it is
**observed, never declared**: the provisioner implements the optional
interface or it does not, and the window is read off the provider on every
reconcile rather than being a field somebody sets. There is deliberately no
`pointInTimeRecovery` capability on the Connection — a declared capability is
one somebody can declare falsely. A CloudNativePG claim is recoverable exactly
where `spec.backup` has put an archive behind it and a base backup has landed
in it, so `available` moves with the policy below and `reason` says which of
the two is missing; a preview's database, and a database the platform adopted
rather than created, are never recoverable and say so. [docs/api/claims.md](api/claims.md#recovering-the-data-to-a-moment-in-the-past)
is the surface, and [docs/BACKUP.md](BACKUP.md) is where this sits next to the
platform's own archive, which restores the claim and never the data behind it.

**`spec.backup` is what puts an archive behind a database this platform runs**
— continuous WAL archiving to an object store, plus a base backup on a
schedule, for a `postgres` claim through CloudNativePG. Every field inherits:
the claim's answer, then the Connection's `config.backup`, then the platform's
own `spec.backup.destination` on the Kitchen singleton, so an installation
that configured a destination once has said where its databases go. The
schedule is CloudNativePG's six-field cron and a five-field one is refused
rather than misread. `status.backup` is read from the database on every
reconcile and never echoed from the spec — `firstRecoverablePoint` is the
whole point of it, and `archiving` is reported apart from the schedule because
a base backup with no WAL behind it recovers to the base backup and no
further. Nothing here ever deletes what is at the destination: not switching
the policy off, and not deleting the claim under either deletion policy —
`retentionPolicy` is the only thing that prunes. A database the platform
adopted rather than created is never written to, a provider that keeps its own
history reports `providerManaged` and takes no policy, and a preview's
database is never backed up at all.

**`config.inngest` names an Inngest app rather than creating one.** An
`inngest` claim, through a `backgroundJobs`-capable Connection holding an
Inngest Cloud API key, binds the keys a connect worker dials Inngest with:
`INNGEST_EVENT_KEY`, `INNGEST_SIGNING_KEY`, `INNGEST_ENV` and
`INNGEST_BASE_URL`, read from the account on every reconcile (Inngest's API
lists keys and mints none, so an environment with no event key is phase
`Failed` saying where to create one). `app` is the ID the application's
Inngest client is created with — the app registers itself when the worker
first connects, and the `AppConnected` condition reports whether one has —
`environment` is the Inngest environment production binds to (`production`
by default), and `mode` is `connect`, the only one provisioned: serve mode
would have Inngest call the application and meet the preview gate. Previews
each get an Inngest branch environment, found or created by name, bound
through the account's shared branch keys plus `INNGEST_ENV`, and archived
(never deleted) with the preview. The type refuses a `deletionPolicy` — an
app holds no data the platform could destroy — and declares
`keepsPodsRunning`: the worker's outbound WebSocket never crosses the
interceptor, so every environment of the project keeps its pods, and the
`ConnectWorkers` condition counts them against Inngest's per-account cap (3
worker connections on the free plan, 20 on paid), which the API does not
expose.

Those branch keys are the account's, and `INNGEST_ENV` is the only thing that
picks an environment — Inngest mints no credential scoped to one, so a preview
that overwrites the variable addresses any branch environment of the same
account, another project's included. The tenancy boundary is the Inngest
account rather than the project, and the two ways to move it are the
operator's: a Connection per project, each holding a key for an account of its
own, or the `inngestSelfHosted` provider, where a preview gets a whole server
and keys of its own. [docs/api/claims.md](api/claims.md) says it at length.

A provider that cannot be asked any of this — a hosted one — refuses the claim
rather than provisioning it as though the block had not been written down.

**Preview databases are empty, and the claim says so.** CloudNativePG has no
copy-on-write branch, and its nearest equivalent is a `pg_basebackup` of
production, which is slow, doubles the storage and puts production data in a
preview environment. A preview's database inherits the parent's image,
extensions and storage and none of its data, so its branch entry declares
`provenance: synthetic` — which keeps production data out of previews by
construction rather than by policy. Neon's copy-on-write branch declares
`production`, because it is the parent's data under a new address.

`spec.deletionPolicy` decides what deleting the *claim* does to the provisioned
database: `Retain` (the default) keeps it at the provider, `Delete` deprovisions it
with its data. Retain is the default because a claim can front a production
database — destroying data is opted into, never implied. Branches and binding
Secrets are cleaned up under either policy: they belong to the platform, not to the
data.

For a database with a volume behind it, that reads concretely. `Delete` deletes
the CloudNativePG `Cluster`, and cnpg garbage-collects its PVCs with it —
the data is gone. `Retain` leaves the `Cluster` running in the platform's
database namespace, still holding its volume and still costing whatever the
volume costs; a claim of the same name created later against the same
connection finds it by name and rebinds to it. That namespace is deliberately
*not* the project's application namespace: deleting a project deletes that one,
and a retained database has to survive exactly that.

#### What a retained resource can be rebound by, and what it cannot

**Rebinding is project-qualified.** A provider-side object is named
`kitchen-<project>-<claim>` — cut to the provider's own budget with a digest
of the whole replacing what was cut — and labelled or tagged
`kitchen.bermos.dev/project`. Both halves matter: the name is what a claim
looks up, and the label is what settles the two projects that can spell one
name between them (project `a-b` claim `c` and project `a` claim `b-c`). A
claim finding an object whose recorded project is not its own is **refused**,
with the provider's sentence on its `Ready` condition, rather than bound to
it.

That is the whole of it, and it exists because the name used to be
`kitchen-<claim>`: a name any project could produce. Under `Retain` a
deleted claim leaves its database, bucket or cache instance behind, so a
developer of another project creating a claim of the same name against the
same Connection was bound to the first project's data, credential re-issued
and nothing anywhere saying so.

Two consequences worth knowing:

- **A claim already bound keeps the name it is bound to**, recorded in
  `status.instanceName`. A resource provisioned before the project was in the
  name goes on being addressed by the old one; renaming it would leave its
  data behind and hand the application an empty one.
- **An object from before the project was in the name is never adopted
  silently.** Nothing records whose data is in it, so a claim that finds one
  fails with a message naming the object — nothing is created in its place
  and nothing is destroyed. An operator who knows whose it is hands it over
  by naming it on the claim:

  ```sh
  kubectl annotate resourceclaim <claim> -n kitchen-system \
    kitchen.bermos.dev/adopt-instance=<the object's name>
  ```

  The next reconcile binds to it and records the project on it, so the
  hand-over is asked for once. This is deliberately an operator's act on the
  cluster: the API takes no annotations from a request body, so no developer
  can ask for somebody else's data through it. Deleting the object at the
  provider is the other way out.

### `type: objectStore` — a bucket for what the application writes

The third type: somewhere to put a file the application did not build into
its image — user uploads, generated exports — instead of the container
filesystem, which loses it on the next deploy. It provisions through an
`objectStore`-capable Connection (`s3`), and the binding is the six things an
S3 client needs:

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: ResourceClaim
metadata:
  name: shop-uploads
spec:
  projectRef: { name: my-shop }
  connectionRef: { name: kitchen-objectstore }
  type: objectStore
  deletionPolicy: Retain                # Retain (default) | Delete — the bucket and its objects
  config:
    objectStore:                        # all of it optional; applied when the bucket is created
      versioning: true                  # keep every version of an object
      publicRead: false                 # anyone may read — refused by the bundled store, which nothing outside reaches
      size: 50Gi                        # a hard quota at a MinIO; refused at a store with no admin API
status:
  phase: Bound
  secretName: shop-uploads-binding      # binding keys: endpoint, bucket, region, accessKeyId, secretAccessKey, forcePathStyle
  instanceID: kitchen-my-shop-shop-uploads   # the bucket's name at the store, which is what it is found again by
  instanceName: kitchen-my-shop-shop-uploads # the same name, recorded as what a later reconcile looks up
  dataProvenance: production
  previewMode: fresh                    # every preview gets an empty bucket of its own
  branches:
    - environment: my-shop-pr-41
      id: kitchen-my-shop-shop-uploads-my-shop-pr-41
      secretName: shop-uploads-binding-my-shop-pr-41
      provenance: synthetic             # an empty bucket never held production objects
```

**A bucket per claim with a credential scoped to it**, never a prefix in a
shared bucket — a prefix is not an isolation boundary. Where the store speaks
the MinIO admin API the provisioner mints a user and a policy per bucket, the
policy reaching the one bucket's objects and nothing about the bucket itself;
the store's own credential is what mints them and is never in a binding. A
preview's bucket is its own — shaped like the parent (versioned when it is),
empty, with its own user — and goes with the preview under either deletion
policy, exactly as a database branch does. `Retain` leaves the claim's own
bucket, its objects and its user at the store; `Delete` removes all three.

`forcePathStyle` is the one binding key worth a sentence: MinIO addresses a
bucket in the path and AWS in the host name, and an application that guesses
wrong fails on every request. It is set on the Connection once and carried in
every binding.

The requirements are typed for the same reason `config.postgres` is: a
provider reads each one and either honours it or refuses the claim *before*
creating anything, with the reason on the `Ready` condition. The bundled
store refuses `publicRead` because it is reached inside the cluster alone; a
store without the admin API refuses `size` because a quota is set through it;
a store that does not implement versioning refuses `versioning`.

### The provider contract: declaring what the data is

Production-derived data in a preview environment is the finding an auditor
reaches first, and the platform closes it off by defining the provider
*contract* rather than any particular provider. A provisioner implements
`database.Provisioner` (`internal/provider/database`): four idempotent verbs —
`Provision`, `Deprovision`, `CreateBranch`, `DeleteBranch` — whose results
carry the compliance half of the contract:

- **`Provenance`** on the returned `Instance`/`Branch` declares what the data
  derives from: `production`, `masked` or `synthetic`. Empty means the
  provisioner cannot say — *undeclared* — and undeclared claims are usable
  only where policy would accept production data: the default bundle's
  `data-provenance-preview` rule refuses both in a preview, so a provisioner
  that cannot declare cannot back an environment that requires a class.
- **`Region`** on the `Instance` reports where the provider actually placed
  the resource, in its own vocabulary. Reported, not declared: it becomes
  `status.residency`, the placement of record.

Neon, the provisioner that ships, declares `production` for both verbs: a
claim's Neon project *is* the production database, and a copy-on-write branch
is the parent's data under a preview's address — cheap to make does not make
it not production-derived. A third-party provisioner that masks or
synthesizes on the way to a branch is exactly the point of the contract:
implement the interface, declare `masked` or `synthetic` on the results, and
nothing else in the platform needs to know it exists — the declaration flows
to the claim's status, the policy engine, the inventory and the environment
screen by itself.

Every declaration is recorded twice: on `status.dataProvenance` (per branch on
`status.branches[].provenance` — the branch is what a preview's workload
reads, so it is the branch's value policy judges a preview on), and as a
signed `https://kitchen.bermos.dev/attestation/data-class/v1` statement kept
in the store's `signed_records` table. The statement's subject is a claim
identity digest — sha256 over namespace/name/uid — because a claim has no OCI
repository for evidence to attach to; the audit log carries the declaration in
the bind record besides.

### `type: oidcClient` — single sign-on for the application

The second type, and one the platform provisions itself, so there is no
Connection: the provider is the identity provider the Kitchen object's
`spec.auth` already names, and the operator registers clients there with the
service credential it holds. `connectionRef` is therefore *refused* on this
type at admission — the refusal names the type — and required on every type
a Connection provisions.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: ResourceClaim
metadata:
  name: shop-auth
spec:
  projectRef: { name: my-shop }
  type: oidcClient                      # immutable, like every claim's type
  config:
    callbackPaths:                      # appended to every environment URL
      - /auth/callback                  # default: /auth/callback and /api/auth/callback/kitchen
    redirectURIs:                       # registered verbatim, for addresses the platform does not own
      - http://localhost:3000/auth/callback
    scopes: [openid, profile, email, offline_access]   # the default
status:
  phase: Bound
  secretName: shop-auth-binding         # binding keys: OIDC_ISSUER, CLIENT_ID, CLIENT_SECRET
  instanceID: 9f3c…                     # the client_id at the issuer
  redirectURIs:                         # what the client currently accepts, as last registered
    - https://my-shop.apps.example.com/auth/callback
    - https://my-shop-pr-41.apps.example.com/auth/callback
  conditions: [...]                     # Ready, Provisioned, RedirectURIsInSync
```

Reconcile: register the client if the operator holds no record of one, write the
binding Secret the application's `fromResourceClaim` variables read, and keep the
redirect list level with the project's URLs. The list is built from the production
URL — *computed* from the project name and the base domain, so a claim binds before
the project has ever been deployed and the first deployment is not waiting on
it — plus every Environment's own `status.url`, every verified custom `Domain`
pointing at one of them, and the claim's verbatim `redirectURIs`, each crossed with
`callbackPaths`. A preview appearing or closing is what moves it, and a reconcile
that agrees with `status.redirectURIs` sends the issuer nothing.

`deletionPolicy` has no say here: the client is always deregistered with the claim.
The policy protects *data* from a deletion nobody meant, and an OAuth client holds
none — what it holds is permission to sign people in, which is the thing that must
not outlive the claim. See [AUTH.md](AUTH.md#app-auth-a-claim-for-single-sign-on).

### `type: volume` — a disk for one process

The third type, and the odd one: every other claim produces credentials, this
one produces a mount. It is for the workload that must write to a filesystem
— a legacy application, SQLite — and it takes no Connection either: the
provider is the cluster's StorageClass, which is a prerequisite of every
Kitchen cluster anyway.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: ResourceClaim
metadata:
  name: shop-data
spec:
  projectRef: { name: my-shop }
  type: volume                          # immutable; connectionRef is refused
  deletionPolicy: Retain                # Retain (default) | Delete
  config:
    previewMode: fresh                  # fresh (the default) or none; shared is refused
    volume:
      source: provision                 # provision (the default) cuts a new one | bind mounts one that exists
      process: web                      # the ONE process that mounts it: web, or a spec.processes name
      size: 10Gi                        # required here; refused on a bound volume
      mountPath: /data                  # absolute, inside that process's container
      storageClass: fast-ssd            # empty takes the cluster's default; refused on a bound volume
status:
  phase: Bound
  # secretName stays empty: the binding is a mount, not a Secret
  dataProvenance: production
  previewMode: fresh
  forcesRecreate: true                  # false where the class was detected ReadWriteMany
  volume:
    process: web
    source: provision                   # echoed, so a reader knows which shape this is
    mountPath: /data
    storageClass: fast-ssd
    accessMode: ReadWriteOnce           # detected from the class, never assumed
    accessModeReason: StorageClass fast-ssd is provisioned by ebs.csi.aws.com, which is not known…
    claimName: shop-data-volume         # the PVC, in the application namespace
    persistentVolume: pvc-3f…           # once bound; under Retain, made to outlive the namespace
    previews:                           # one fresh, empty PVC per preview, gone with it
      - environment: my-shop-pr-41
        claimName: shop-data-volume-my-shop-pr-41
        provenance: synthetic
  conditions: [...]                     # Ready, Provisioned, PreviewVolumesReady
```

Reconcile: refuse (`Failed`, with the list) a claim naming no process or one the
project does not have — the web process is `web`, the one name a declared
process cannot take — or a StorageClass the cluster does not have; read the
class's access mode off its provisioner and its
`kitchen.bermos.dev/read-write-many` annotation; create the PVC in the
application namespace and one more per preview Environment; and bind. The
Environment reconciler finds the project's volume claims itself — they are
not variables — and mounts each into exactly the process it names, waiting
(`Ready=False`, `VolumeNotBound`) while one has no volume for that
environment yet.

**What it costs, enforced.** A `ReadWriteOnce` volume attaches to one pod at a
time, so the process mounting it is written with `replicas: 1` — and where
KEDA owns the count, the `HTTPScaledObject`'s ceiling is capped at one — and
`strategy: Recreate`: a rolling update's new pod would wait in `Multi-Attach`
for a volume the old pod never releases, which is a deadlock rather than a
delay. That is downtime on every deploy of that process, and the dashboard
says so where the claim is made. A class detected `ReadWriteMany` lifts both.

**Retention across project deletion.** The PVC lives with the pods, in the
application namespace, and that namespace dies with the project. So under
`Retain` the reconciler patches the bound PersistentVolume's reclaim policy to
`Retain` and labels it with the project and the claim the moment the PVC
binds; the namespace can go and the volume stays, `Released`. A later claim of
the same name in the same project finds it by its labels, pre-binds a new PVC
to it by name, and carries on with the data. `Delete` puts the reclaim policy
back and deletes the PVC, and the volume follows. Preview volumes always go
with their preview.

#### `source: bind` — a volume that was already there

The other half of what a persistent workload is: the data that predates the
cluster. Twelve terabytes on a NAS, an SMB share, a CSI volume attached by
hand. The platform mounts it and **provisions nothing**.

```yaml
  config:
    volume:
      source: bind
      process: web
      mountPath: /media                 # size and storageClass are refused here
      bind:
        persistentVolume: nas-media     # or persistentVolumeClaim, one of this project's own — never both
        accessMode: ReadOnlyMany        # ReadOnlyMany | ReadWriteOnce | ReadWriteMany
status:
  previewMode: shared                   # the same volume, read-only; none where the mode is ReadWriteOnce
  forcesRecreate: false                 # ReadWriteOnce would make it true, exactly as a cut volume does
  volume:
    source: bind
    accessMode: ReadOnlyMany
    claimName: media-volume             # the PVC the platform made to reach it, pre-bound by claimRef
    bound:
      persistentVolume: nas-media
      capacity: 12Ti
      named: nas-media                  # what the claim named to get here
      identity: nfs://nas.lan/export/media
      writable: false
      sharedWith: [sonarr/media]        # the other claims holding this same storage
```

Reconcile: refuse `deletionPolicy: Delete` outright; find the volume, or fail
(`VolumeNotFound`) naming what could not be found — no amount of waiting
conjures an NFS export, and only a PersistentVolumeClaim that has not bound
yet is a wait; refuse an access mode the volume does not offer, naming the
ones it does; then create a PersistentVolumeClaim of the platform's own,
pre-bound to the volume by `claimRef` so that it cannot match somebody else's.

**Two projects may read one volume; one may write it.** Read-only sharing
costs nothing and is most of what an existing export is for, so any number of
claims may hold one. The second *writer* is refused (`VolumeWrittenElsewhere`)
naming the claim and project that already writes it: two projects writing one
filesystem is one project's deploy breaking another's, which is the opposite
of Kitchen owning what it deploys. The comparison is on
`status.volume.bound.identity` — what the volume *points at* — not on names,
because two projects reach one export through two PersistentVolumes and a name
comparison would call them unrelated. The oldest writing claim keeps it.

**Previews mount the same volume, read-only**, which is why this provider is
the one that declares `shared` as its own mode: a preview of an application
whose data is the point of it must read what production reads, and a read-only
mount takes nothing from production and changes nothing on it. The reconciler
writes the read-only flag onto the pod's volume and its mount rather than
trusting the application. Where the claim's own mode is `ReadWriteOnce` the
volume attaches to one pod at a time and production has it: previews get
`none`, with the reason on the claim.

**Teardown unmounts and never deletes.** Deleting the claim removes only the
PersistentVolumeClaim the platform created — and not even that where the claim
named a PersistentVolumeClaim that was already in the namespace. The
PersistentVolume, and every byte on it, stays.

---

## `PlatformUpdate` (cluster-scoped)

One attempt to upgrade Kitchen's own Helm release. Cluster-scoped because the thing
it upgrades is: there is one release, and the namespace it lives in is compiled in.
Records are kept after they finish, so the list is the installation's upgrade history.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: PlatformUpdate
metadata:
  name: update-0-2-1
spec:
  version: "0.2.1"                      # bare SemVer; the only field there is
status:
  phase: Succeeded                      # Pending | Running | Succeeded | Failed
  fromVersion: "0.2.0"
  jobName: kitchen-self-update-update-0-2-1
  message: the platform is on 0.2.1
  conditions: [...]
```

`spec.version` is deliberately the only field. The Job that runs the upgrade is bound
to cluster-admin, so an update that forwarded caller-supplied helm arguments would be
a way to apply anything at all as cluster-admin — the reconciler builds the whole
invocation itself and reads nothing from here but the version.

Reconcile: preflight (self-update enabled, not a downgrade, not already current, minor
crossing allowed, ServiceAccount present, no other upgrade in flight) → a Job running
`helm upgrade --reset-then-reuse-values --atomic` → phase from the Job's outcome.

The Job exists because the operator cannot run its own upgrade: applying the new
manager Deployment terminates the pod, and helm killed mid-upgrade leaves the release
`pending-upgrade` with no process to finish or roll it back. Everything in status is
therefore derived from the Job, never from what the reconciler remembers — the process
that reports an upgrade succeeded is a different one, usually a different version, from
the one that started it.

Needs `selfUpdate.enabled=true` on the chart; without it every PlatformUpdate is
refused with a message saying so. See the [chart README](../charts/kitchen/README.md#letting-the-platform-update-itself).

---

## `SavedQuery` (namespaced: kitchen-system)

A question about the logs, kept under a name. The observability view carries its whole
selection in the URL, so any question is already a link; this is what makes one
*findable* by someone who was never sent the link.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: SavedQuery
metadata:
  name: checkout-500s                   # derived from the title by the API
spec:
  title: Checkout 500s
  description: Errors from the checkout service
  query: "level:error service:shop"     # Kitchen's log query language
  where: ""                             # the ClickHouse escape hatch, if that is what was used
  rangeMinutes: 60                      # relative window; 0 means everything retained
  limit: 500
  view: patterns                        # which tab it is read in: lines | patterns
  includeCluster: false
  savedBy: grace@example.com
```

A saved query **with no alert has no status and no reconciler**, and that is the point
rather than an omission. The rule that a write surface waits for its reconciler is about
objects that do nothing until something acts on them — a `Domain` is not routed and a
`ResourceClaim` is not provisioned until their controllers run. A saved query has its
whole effect by existing: reading it back is the feature.

The window is a duration and never an absolute range. "The spike last Tuesday" stops
being a question and becomes a screenshot, and the retention deletes it out from under
its own name.

### `spec.alert` — the standing question

An alert is the one exception above, because a standing question is not answered by
existing: something has to ask it. `SavedQueryReconciler` does, and a crossing is the
second trigger onto the [notification path](#notificationsubscription-namespaced-kitchen-system).

```yaml
spec:
  alert:
    windowMinutes: 10                   # how far back each evaluation counts, 1..1440
    threshold: 25                       # the number of matching lines it is compared to
    comparison: above                   # above | below — below is the heartbeat
    intervalMinutes: 5                  # how often it is asked; a floor, not a promise
    suspended: false                    # stop evaluating without deleting anything
status:
  lastEvaluationTime: 2026-09-04T02:10:00Z
  lastCount: 63
  firing: true
  firingSince: 2026-09-04T02:05:00Z
  message: 63 line(s) in the last 10 minute(s); the alert fires above 25
```

The alert lives on the saved query rather than in an object of its own because the query
*is* its definition — an alert whose question cannot be opened in the observability view,
tuned and saved again is an alert nobody can tune. `windowMinutes` is separate from
`rangeMinutes` for the same reason both exist: an alert wants a short window it can
evaluate often, and the same question is usually worth reading over a longer one.

It is **edge-triggered**. Crossing records an `alert.firing` activity event — exactly the
way a reconciler records a deploy — and staying crossed records nothing further, so a
threshold that is met all afternoon is one message rather than one every five minutes.
`status.lastCount` and `status.message` are what a person reads in the meantime, and
`status.message` is also where an evaluation that could not be made (no telemetry store,
a store that refused the query) says so — otherwise indistinguishable from an alert that
has simply never fired.

---

## `NotificationSubscription` (namespaced: kitchen-system)

Where the platform sends an account of itself. One address, the events it wants, and the
key every payload to it is signed with.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: NotificationSubscription
metadata:
  name: shop-relay
spec:
  url: https://relay.example.com/kitchen    # absolute, and https
  events:                                   # at least one; an empty list is refused
    - deploy.succeeded
    - build.failed
  projectRef: {name: shop}                  # absent is the platform scope: every project
  secretRef: {name: kitchen-notify-shop-relay}   # holds `secret`; written by the API, never read back
  description: into #shop-deploys
  suspended: false
  maxAttempts: 5                            # 1..10
  timeoutSeconds: 10                        # 1..30
  createdBy: grace@example.com              # a byline
status:
  conditions: [{type: Ready, status: "True", reason: Subscribed}]
  delivered: 412
  failed: 3
  deadLettered: 0
  lastDeliveryTime: 2026-09-04T01:59:12Z
  lastResult: delivered                     # delivered | failed
  lastStatusCode: 204
```

The vocabulary is the platform's rather than the reconcilers': `deploy.succeeded` is a
promotion, an auto-deploy on a push and a rollback alike, because all three are one fact
— what is serving changed. A relay somebody wrote in an afternoon should not have to
learn that they are three code paths. The events are `deploy.succeeded`, `build.failed`,
`environment.unhealthy`, `preview.created`, `preview.destroyed` and `alert.firing`.

An empty `events` is refused at admission rather than read as "everything": a subscription
that silently widened when the platform learned a new event type is one that starts
paging somebody at 03:00 because of an upgrade.

`https` only. A signed payload over plain HTTP is one anybody on the path can read, and
the signature proves only that it was not changed on the way. The signing key is supplied
by the caller and never answered with — see
[docs/api/notifications.md](api/notifications.md), which is also the payload and signature
contract a receiver is written against.

`NotificationSubscriptionReconciler` says whether the subscription can deliver (the URL,
and whether the signing key is still there) and bounds how much delivery history one
subscription keeps. Everything else about it is the deliveries'.

---

## `NotificationDelivery` (namespaced: kitchen-system)

One event on its way to one subscription. Created by `internal/notify` when an event
matches, and owned by the subscription, so deleting one takes its whole history with it.

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: NotificationDelivery
metadata:
  generateName: shop-relay-
  labels:
    kitchen.bermos.dev/subscription: shop-relay
    kitchen.bermos.dev/event: build.failed
spec:
  subscriptionRef: {name: shop-relay}
  event: build.failed
  eventId: 9f1c0b7e5d3a4f628a1c0d9e8b7a6f54   # the receiver's idempotency key
  payload: '{"version":"v1","id":"9f1c…"}'    # the exact bytes that will be sent
  project: shop
status:
  phase: DeadLettered                         # Pending | Delivered | DeadLettered
  attempts: 5
  attempted: [{number: 5, time: 2026-09-04T02:09:41Z, statusCode: 502,
               error: receiver answered 502 Bad Gateway, durationMillis: 41}]
  nextAttemptTime: null
  completedTime: 2026-09-04T02:09:41Z
  lastStatusCode: 502
  lastError: receiver answered 502 Bad Gateway
```

It is a separate object for three reasons, and each is a requirement rather than a
preference:

- **A failing notification must never affect what it reports on.** Deciding to notify
  happens on a reconcile path — the Build controller's, the Environment controller's —
  and talking to somebody else's HTTP server must not. Creating this object is the whole
  of what that path does, off its own goroutine, and even that is best-effort.
- **At-least-once has to survive a restart.** A queue in the operator's memory loses
  what is in flight when the pod moves, which is exactly the moment a platform is most
  worth hearing from. The delivery is in etcd before the first attempt.
- **The dead letter has to be visible.** "It was never delivered" is a thing a person has
  to be able to see, and a ring buffer in a status would be either too small to be useful
  or too large to belong in one.

`spec.payload` holds the bytes rather than the fields they were built from: the signature
is an HMAC over the body, and a body whose key order changed between attempt one and
attempt four would verify on one and not the other. It is also what makes a dead letter
retryable — `POST /notifications/deliveries/{name}/retry` re-sends exactly what would
have been sent, under the same event id.

The backoff is `status.nextAttemptTime` and a requeue rather than a sleep, so a ladder
interrupted by a restart is picked up where it was. A delivered notification is pruned
after an hour, a dead letter after seven days, and a subscription keeps at most 200
deliveries — nothing still pending is ever dropped.

---

## Deploy status back on the commit

The half of git integration that faces the reviewer. A Connection reporting the
`statusChecks` capability is one the platform posts back through; a Connection without it
is used as a source and nothing more, silently — which is also what a provider that can
be a source but cannot report anything yet gets, since the operator asks for the
reporting half with a type assertion (`gitprovider.StatusReporter`) rather than assuming
every provider has one.

Three things are posted, all keyed so that several Kitchen projects can watch one
repository without overwriting each other:

- **A status check per Build**, in context `kitchen/<project>`: `pending` when the
  BuildKit job starts, `success` when the image is pushed, `failure` when the build
  failed and `error` when the platform could not run it at all. `target_url` is the
  build's page in the dashboard.
- **A deployment per Environment**, named after the Environment, carrying the URL:
  `in_progress` while the workload comes up, `success` once it is available, `inactive`
  when a preview is torn down. Previews are marked transient, production is marked
  production.
- **One comment per preview**, rewritten in place on every push rather than appended to,
  found by an invisible `<!-- kitchen-preview: <environment> -->` marker and thereafter
  by the ID recorded in `status.gitReport`. It states that a protected preview asks an
  anonymous visitor to sign in, because a reviewer who is not a platform user would
  otherwise read the gate as a broken link.

**None of it can fail a deployment.** A revoked token is the Connection reconciler's
business — it probes the credential and turns `CredentialsValid` red — and a build that
produced an image produced it whether or not the provider heard about it. Posting
failures are logged, and for an Environment recorded in `status.gitReport.error`, which
is also what makes the next reconcile retry rather than treat the report as delivered.

## Flows, end to end

**Push to `main`:** webhook → `Build` created → BuildKit job (logs → ClickHouse) →
image pushed → `Release` created → production `Environment.releaseRef` updated →
apps/v1 Deployment rolls → status check goes green on the commit.

**PR opened:** webhook → `Build` for the head SHA → `Release` → operator creates
`Environment` (type `preview`, DB branch if claimed) → HTTPRoute programmed →
PR comment with the preview URL. Every later push to the PR: new Build → new Release →
same Environment's `releaseRef` bumped. PR closed: Environment (and DB branch) torn down.

**A Build is named after its commit, and the pull request event usually arrives
second.** A branch is normally pushed before a request is opened for it, and every
provider delivers the push first — so the `Build` for the head SHA already exists, and
was created from a push that knew of no request. The receiver records the request on
that Build as the `kitchen.bermos.dev/pull-request` annotation rather than creating a
second one: the spec is immutable and this is genuinely new information about a commit
already known, not a correction. `Build.PullRequestNumber()` is what everything asks —
the preview routing, the source attestation, the API's `git.pullRequest` — so which
event arrived first stops being visible. When the request is opened long enough after
the push that the build has already finished, the operator routes that build's existing
`Release` to a preview after the fact and records the environment in
`status.preview`, which is also what stops a preview torn down at PR close from being
recreated on a later reconcile.

**Rollback:** `Environment.spec.releaseRef` → previous Release. One field, instant,
exact (config was snapshotted). The UI's rollback button is a one-line patch.

## Conventions

- Every CR uses `metav1.Conditions` in status; the UI reads conditions, not phases,
  for detail (phases are the coarse summary).
- Owner references throughout: deleting a Project cascades to everything.
- Builds/Releases are immutable after creation — a CEL transition rule on the CRD, so
  the API server refuses the edit and no admission webhook is needed — and
  garbage-collected by count per project, never by the operator "cleaning up" something
  an Environment still references.
- Cross-references are by name within `kitchen-system` — no cross-namespace refs in v1alpha1.
