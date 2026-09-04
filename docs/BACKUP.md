# Kitchen — backup and restore

Kitchen owns the cluster it is installed into, which means nobody else is
backing it up. This is what one archive holds, what it deliberately does not,
how to take one, and — the half that is worth anything — how to put it back.

An untested restore is worth exactly nothing. The procedure below is the one
CI runs on every change to this repository: the `Chart install on kind` job
installs the chart, creates a project, takes a backup, wipes the platform's
state, restores it and asserts the project is back. See
[`.github/workflows/helm.yml`](../.github/workflows/helm.yml), the step called
*Back up, wipe and restore*.

## The three stores, and what happens to each

| Store | Holds | In the archive |
| --- | --- | --- |
| **etcd**, through the CRDs | Every Project, Connection, Build, Release, Environment, Domain, ResourceClaim, SavedQuery and NotificationSubscription, plus the Kitchen singleton | **Yes** |
| **The identity provider's Postgres** | Accounts, sessions, OAuth clients, passkeys, API keys | **Yes**, as a data dump |
| **ClickHouse** | Logs, metrics, traces, flow data, the audit log | **No** |

**Telemetry is not backed up and is not expected to survive.** That is a
decision, not an omission: it is analytics, it already expires on
`spec.observability.clickhouse.retentionDays`, and an archive that carried it
would be orders of magnitude larger than the configuration it exists to
protect. Restore a platform and its dashboards start empty. The one thing in
that store worth thinking twice about is the **audit log**, which answers to a
retention of its own (`spec.compliance.audit.retentionDays`, at least 90 days)
— if a policy requires it to outlive the cluster, export it separately.

The credentials matter more than the objects. Every Secret in `kitchen-system`
travels with the archive: the Cloudflare API token, git app keys, connection
credentials the API wrote, the attestation signing key, and the identity
provider's own signing secret. A restore without them brings back a platform
that cannot talk to anything.

Also not in the archive, and named in its own manifest so nobody has to guess:

- **Container images.** Builds push them to a registry, which is backed up — or
  not — wherever that registry runs. The bundled registry's volume is not here.
- **Objects in buckets.** An `objectStore` claim's bucket belongs to the store
  running it, and the bundled store's volume is not here any more than the
  registry's. The claim is restored and rebinds to a bucket that survived — a
  `Retain`ed bucket is found again by name — but the objects survive only if
  the volume did, or if something else is backing the store up.
- **Application data.** A database a `ResourceClaim` provisioned belongs to the
  provider running it. The claim is restored; what it points at is Neon's, or
  whoever else's, to keep. **That includes the databases this platform runs
  itself**: a CloudNativePG claim's data lives in a volume in
  `spec.databases.namespace`, which this archive does not carry and this
  restore does not recreate. It survives a restore only if the namespace and
  its volumes did — or if something else is backing them up. CloudNativePG has
  its own backup machinery for exactly this, and pointing it somewhere is a
  decision an installation that keeps production data here has to make
  deliberately — a per-claim backup policy that makes that decision once, on
  the platform, is the second phase of
  [issue #245](https://github.com/Bermos/Kitchen/issues/245).
- **Volumes a `volume` claim mounts into an application.** They live in the
  project's application namespace, on whatever StorageClass the claim named,
  and this archive carries neither them nor their data: the claim is
  restored, and a restored claim under `Retain` re-binds to its volume only if
  the volume survived — which it does on the same cluster, and does not on a
  new one. Backing up that data is the cluster storage's job, or a
  [snapshot](#pvc-snapshots-an-option-not-the-plan) where the cluster can
  take one.
- **The platform's upgrade history** (`PlatformUpdate` objects), which describes
  a cluster that will not exist by the time anyone is restoring.
- **Notifications in flight** (`NotificationDelivery` objects), including the
  dead letters. A delivery is one message on its way somewhere, so restoring a
  queue of them would post a week-old batch of deploys at whoever is watching
  the new cluster — and restoring the dead letters would invite somebody to
  send them again. The *subscriptions* are carried, signing keys included, so a
  restored platform keeps talking to the same receivers; what was announced is
  in the activity feed either way.
- **Secrets outside the platform namespace.** The registry pull credential in
  each application namespace is a copy the operator syncs, written again on the
  next build.

## Two different recoveries, and this document is only one of them

The list above has a shape to it: **this archive restores the platform's
configuration and its accounts, and never the application data a claim points
at.** That is not a gap to be filled later — the data belongs to the provider
running it — but it does leave a second kind of recovery, and conflating the
two is how somebody ends up restoring a platform and expecting a database to
come back with it.

| | Restores | How | Where it is |
| --- | --- | --- | --- |
| **Platform archive** | Configuration, credentials and accounts — every Project, Connection, Environment, Domain and ResourceClaim, plus the identity provider's Postgres | The restore Job, below | This document |
| **Claim recovery** | One claim's application data, as it was at a moment | Per claim, through its own provider | [docs/api/claims.md](api/claims.md#recovering-the-data-to-a-moment-in-the-past) |

A full disaster recovery is both, in that order: restoring the platform brings
back the *claims*, and what each of them points at is recovered separately, or
not at all. The two answer different questions and fail in different ways —
this one is all-or-nothing and is taken on a schedule, and the other reaches
back only as far as the claim's own provider keeps history, which the claim
reports rather than the platform deciding.

**Claim recovery exists where the provider can actually do it.** A Neon claim
can be recovered to any moment inside the project's retention window, because
Neon's storage keeps continuous history; a claim through the self-hosted
provider cannot, because there is no archive behind it to recover from — a
per-claim backup policy, which is what would put one there, is
[issue #245](https://github.com/Bermos/Kitchen/issues/245)'s second phase. The
claim says which of the two it is, in the provider's own words, rather than
either being assumed.

Neither kind of recovery rewinds anything in place. The platform restore
writes objects back into an empty platform; a claim recovery makes a *copy* of
the data at the chosen moment and leaves the original alone until somebody
promotes the copy. Both are described in their own document, and neither
substitutes for the other.

## Taking a backup

**Platform → Backup** on the dashboard. The screen says what an archive would
carry before you take one; the button streams it to your browser.

The archive is a credential. It holds every secret the platform has, in the
clear. Keep it where you would keep the cluster's root credentials, and keep it
**off the cluster it came from** — a backup that only exists on the machine
that died is not a backup. Taking one is recorded in the audit log as an
`export` against the Kitchen object, because "who took a copy of everything,
and when" is exactly the sentence an audit log exists to be able to produce.

The same thing from a terminal, which is what a scheduled backup is built out
of — a backup that only happens when somebody remembers to click is not a
backup, and [Automating it](#automating-it) below is the whole of that
sentence:

```sh
kitchen backup                                   # into the current directory
kitchen backup /backups/kitchen.tar.gz --force   # somewhere else, overwriting
```

With `--json` it answers one object naming the file and what went into it, read
back off the archive rather than remembered, so a cron job can check that what
it just took carries the accounts as well as the objects. Or over the API
directly:

```sh
curl -fsSL -X POST -H "authorization: Bearer $TOKEN" \
  -o kitchen-backup.tar.gz \
  https://kitchen.apps.example.com/api/v1/platform/backup
```

Both routes are the operator's. There is no smaller archive for a member.

### What is inside

```
manifest.json                  what this is, and what it leaves out
resources/projects/shop.json   one document per custom resource
secrets/cloudflare-token.json  one per Secret in kitchen-system
accounts/tables.json           the identity provider's tables, in restore order
accounts/user.copy             each one in PostgreSQL's COPY text format
```

`manifest.json` names the release that wrote the archive, the installation it
came from, and the exclusions above. It is the first entry in the tar, so
`tar xzOf backup.tar.gz manifest.json` answers "what is this?" without
unpacking a file full of secrets.

The accounts half is **data only**. The schema is better-auth's and the identity
provider migrates it from its own plugin set on every start
([`auth/src/db.ts`](../auth/src/db.ts)), so an archive carrying DDL would be
carrying a second, staler opinion about what the tables look like. The
consequence is the version rule below.

## Automating it

**Platform → Backup**, the Schedule section. Give it a five-field cron
expression and a destination, and the operator writes a CronJob of the same
exporter the button uses — so a scheduled archive and a manual one are the same
file and restore identically.

```
Schedule     0 3 * * *          five fields, UTC
Destination  s3://kitchen-backups/prod
Retention    keep the last 30
```

The same three from a terminal. The schedule and the retention are ordinary
settings; the destination has a route of its own because it carries a
credential, and the settings route must never carry one:

```sh
# where archives go, once — the response echoes the bucket and no key
kitchen api PUT /platform/backup/destination --data '{
  "type": "s3",
  "s3": {"bucket": "kitchen-backups", "prefix": "prod", "region": "eu-central-1",
         "accessKeyId": "…", "secretAccessKey": "…"}
}'

# when it runs and how much is kept
kitchen api PATCH /settings --data '{"backupSchedule": "0 3 * * *", "backupKeepLast": 30}'

kitchen backup run     # take one now, to the destination
kitchen backup list    # what is actually in the bucket
```

`kitchen backup run` is the step worth taking on the day you configure a
destination. It is how you find out that the credential works then, rather than
at 02:00 six weeks later.

All four of those are the operator's routes, and no credential `kitchen login`
can store holds that role yet — see
[docs/CLI.md](CLI.md#the-platform-commands-are-the-dashboards-for-now). Until
that changes, the dashboard is where this is configured, and the commands are
here because the shape of the answer is worth reading whichever surface you
use.

### What one run does, and why in that order

1. **Export** the archive onto disk in the run's own pod.
2. **Upload** it to the destination.
3. **Verify** it by reading it back — a ranged GET of the first 64 KiB, which
   is enough because the manifest is the first entry in the tar, checked
   against the release and the timestamp this run just wrote.
4. **Prune** by the retention, and only now.

Pruning last is the whole of it. A prune that ran before the new archive had
been read back would be a system that deletes last week's backup on the night
this week's fails, which is the one way a backup system can be worse than not
having one. And a scheduled backup nothing has ever read back is an untested
restore, which the first line of this document says is worth nothing.

Retention only ever considers keys under the configured prefix that are named
the way this platform names an archive. A bucket you also keep other things in
does not lose them — `kitchen backup list` marks every object with whether it
is one this platform wrote, precisely so nobody has to wonder.

### The thing that actually loses data

**Nothing here is worth much without the part that says the backups have
stopped.** A backup system's characteristic failure is not a corrupt archive,
it is six weeks of no archive that nobody noticed, and a CronJob whose pods
fail silently is exactly how that happens.

So the platform reports on its own backup, unasked, in three places that are
one fact:

- the **`BackupReady` condition** on the Kitchen object;
- a **`backup` row in `status.components`** — the list an operator already
  reads, beside the workloads and the clock check;
- **`lastSuccess`** on the Backup screen, and on `GET /platform/backup`.

`BackupReady` is **false with reason `NotScheduled`** on an installation that
has configured nothing. That is deliberate. An installation with no scheduled
backup is not broken, it is unprotected, and this is the one place that says so
without being asked.

The other reasons: `RunFailed` (the newest run failed, with its message),
`NeverSucceeded` (a run was scheduled and no archive has ever reached the
destination), `RunLate` (a run started and has not finished within its timeout),
`Suspended` (paused on purpose, saying how old the last archive is) and
`BackedUp`.

The check worth alerting on from outside is still the age of the newest object
at the destination — `kitchen backup list --json` — rather than the exit code
of the last run. A run that exits 0 having uploaded nothing is the failure that
matters, and the bucket is the only thing that can disprove it.

### The details worth knowing before you set one

- **Cron is UTC**, here as everywhere else on this platform — worth saying out
  loud in a system that measures node clock drift. `0 3 * * *` is 03:00 UTC,
  not 03:00 where you are sitting.
- **Pick a quiet hour.** The accounts half of an archive is taken through the
  identity provider's database, and a dump competing with sign-ins helps
  nobody. Two runs never overlap — `concurrencyPolicy: Forbid` — and one run is
  bounded by `spec.backup.timeout`, 30 minutes by default.
- **A schedule with no destination is refused**, at admission and by the API.
  There is deliberately no local destination: the only place on the cluster to
  put an archive is a volume on the cluster the archive exists to survive the
  loss of.
- **Suspend rather than delete** for maintenance. It keeps the configuration,
  which is the difference between a pause and a hope that somebody puts it back.
- **No credential is ever read back.** The destination's key pair is a Secret
  the operator writes, labelled `app.kubernetes.io/managed-by: kitchen` and
  removed with the destination. Nothing serves it, and a form that redisplays
  the destination sends no key — which is why leaving the key fields empty
  means "leave the credential alone", and moving to the pod's own credential
  chain is an explicit `ambientCredentials: true`.
- **Prefer no long-lived key at all where you can.** A destination naming no
  Secret uses the ambient credential chain — IRSA, EKS Pod Identity, an
  instance role — which is the better answer wherever it is available, because
  there is then nothing to leak.
- **Ask the store to encrypt it.** `serverSideEncryption: AES256` or
  `aws:kms` with a `kmsKeyId`. See the next section for why.
- **Retention is not a safety property.** It deletes, so it is something
  whoever reaches the credential can use. Object Lock or object versioning is
  the store's answer to that, and Kitchen does not manage it.
- **The run's own identity** is a `<release>-backup` ServiceAccount the chart
  creates (`backup.rbac.create`). It is read-only, and it is not a privilege
  reduction: a backup reads every credential the platform holds, which is the
  operator's own power. It exists so the grant is enumerable, visible in one
  file, and gone with the release.

### And where the archive goes is now a credential store

This is worth settling before the first upload rather than after. An archive
holds every secret the platform has, in the clear — the Cloudflare token, the
git app keys, the attestation signing key, the identity provider's signing
secret, and every connection credential the API wrote. Putting one in a bucket
nightly makes **that bucket the cluster's root credential store**, and it should
be locked down as one: its own bucket, no public access, server-side encryption,
credentials that can write and list but that are not the same credentials the
platform holds, and object versioning or object lock if the threat you care
about includes somebody deleting the backups before deleting the cluster.

Keep the destination's own credential outside the platform too. It is in the
archive, and the archive is in the destination.

## Restoring

**Restore into the release the archive was written by.** A restore refuses an
archive from another release unless `restore.force` says otherwise — the rows
only fit the schema their own release migrates into place. Restore first,
upgrade afterwards.

There is no restore button, and there cannot be one. A restore happens into a
cluster whose accounts database is gone, so the credentials to log into the
dashboard are inside the archive and there is nobody left to authenticate.
That puts it where installing the chart and following the bootstrap link
already are: cluster bootstrap, the exception
[CLAUDE.md](../CLAUDE.md) keeps to "nothing needs kubectl".

### The procedure

1. **Install the chart at the archive's release, into an empty cluster.** The
   prerequisites are Kitchen's usual ones — Cilium, a default StorageClass —
   and `--create-namespace`.

   ```sh
   helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
     --version 0.9.0 --namespace kitchen-system --create-namespace \
     --set kitchen.baseDomain=apps.example.com \
     --set kitchen.tls.acme.email=ops@example.com \
     --set kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName=cloudflare-api-token \
     --wait
   ```

   The Cloudflare token secret is a bootstrap step as it always is: create it
   before the install, or take it out of the archive.

2. **Wait for the identity provider.** It creates its own tables on first
   start, and the accounts dump has nowhere to go until it has.

3. **Put the archive somewhere the cluster can read it, and run the Job.**

   ```sh
   kubectl -n kitchen-system create secret generic kitchen-backup \
     --from-file=backup.tar.gz=./kitchen-backup-prod-2026-08-19T090000Z.tar.gz
   helm upgrade kitchen oci://ghcr.io/bermos/charts/kitchen \
     --version 0.9.0 --namespace kitchen-system --reuse-values \
     --set restore.enabled=true --set restore.secretName=kitchen-backup
   kubectl -n kitchen-system logs job/kitchen-restore-1 --follow
   ```

   A Secret is bounded by etcd's object limit, about 1 MiB. Past that, put the
   archive on a volume and set `restore.existingClaim` instead.

4. **Watch the reconcilers catch up.** Everything comes back with the status it
   had, which is what keeps a restored platform from re-running every build in
   its history. The operator then reconverges: namespaces, Deployments,
   HTTPRoutes and certificates are rebuilt from the restored objects.

5. **Take a backup of the restored platform.** It is the cheapest way to find
   out whether the restore actually worked.

`--set restore.id=2` runs it again: a Job's pod template is immutable, so the
id is part of its name and a second attempt's log stands beside the first.

### What a restore does not overwrite

Three Secrets travel in the archive and are deliberately **not** written back,
and the Job's log names each one:

| Secret | Why |
| --- | --- |
| `<release>-postgres` | The accounts database's password belongs to the Postgres this install just created, not to the one that was lost. Its *contents* are restored instead. |
| `<release>-clickhouse` | Same, for a telemetry store whose contents are not restored at all. |
| `<release>-registry` | The bundled registry admits the credential it was installed with, and the operator keeps the seeded Connection in step with it. |

They are in the archive because evidence is not the same thing as a restore
step: recovering images out of an old registry volume needs the old registry
password.

The identity provider is rolled at the end of a restore. Its signing secret is
what every restored session and API key was signed with, and the pods that are
running came up with the one the fresh install generated — until they restart,
every restored login is refused by a platform that otherwise looks entirely
healthy.

### Restoring over a live platform

The same Job works, and it is an update rather than a merge: an object in the
archive replaces the one in the cluster, and an object in the cluster that is
not in the archive is left alone. Nothing is pruned, which is what makes a
restore safe to run twice. The accounts half is not so gentle — it replaces the
database's contents wholesale, in one transaction — so everybody signed in
since the backup was taken is signed out.

## PVC snapshots: an option, not the plan

Where the cluster has a working CSI snapshot controller, a `VolumeSnapshot` is
the cheap answer for the two volumes this archive cannot carry: the accounts
database and the telemetry store. The Backup screen says whether it is actually
available here, and it checks rather than assumes — issue #64 found a cluster
running Kitchen whose snapshot controller had no CRDs installed at all, where a
`VolumeSnapshot` is accepted by nothing and nobody is told. Both halves have to
be there: the `snapshot.storage.k8s.io` API **and** a `VolumeSnapshotClass` to
snapshot into.

It stays an option rather than the plan, for two reasons. A snapshot lives on
the same storage as the volume it copies, so it survives a bad upgrade and not
a lost cluster. And a Postgres volume snapshotted while it is running is a
crash-consistent copy, which Postgres will recover from on start — usually.
The archive above is a logical dump taken through the database, which has no
such "usually".

## CRD migrations

There is no conversion webhook, the API is `v1alpha1`, and today's honest
answer is that nothing has broken yet. What that means for a backup:

- **An archive is bound to the release that wrote it.** The manifest records it
  and a restore checks it. `--force` exists for when you know the schemas are
  compatible; it is not a routine flag.
- **The first breaking schema change needs a plan before it is made, not
  after.** Kitchen is pre-1.0, so a breaking change bumps the minor
  (`bump-minor-pre-major`), and `selfUpdate.allowMinor` is off by default
  precisely so those upgrades are opted into with the release notes in hand.
- **Additive changes are already safe.** A field added with a default is
  populated by the API server on read, so an older archive restores into a
  newer schema; that is why the version check is a refusal you can override
  rather than a hard wall.
- **When conversion does arrive**, it is the archive's `format` number that
  gains a version, not this document: a reader refuses a format it does not
  understand rather than restoring three quarters of one.

## Restoring somewhere else

An archive carries the base domain it came from, in the Kitchen singleton and
in every URL derived from it. Restoring it into an installation on a different
domain restores the *old* domain along with it — which is right for a
disaster-recovery rebuild of the same platform, and wrong for standing up a
copy. For a copy, restore, then change the base domain on the settings screen
and let the reconcilers rebuild the routes and the certificate.
