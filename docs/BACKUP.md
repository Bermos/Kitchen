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
| **etcd**, through the CRDs | Every Project, Connection, Build, Release, Environment, Domain, ResourceClaim and SavedQuery, plus the Kitchen singleton | **Yes** |
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
- **Application data.** A database a `ResourceClaim` provisioned belongs to the
  provider running it. The claim is restored; what it points at is Neon's, or
  whoever else's, to keep.
- **The platform's upgrade history** (`PlatformUpdate` objects), which describes
  a cluster that will not exist by the time anyone is restoring.
- **Secrets outside the platform namespace.** The registry pull credential in
  each application namespace is a copy the operator syncs, written again on the
  next build.

## Taking a backup

**Platform → Backup** on the dashboard. The screen says what an archive would
carry before you take one; the button streams it to your browser.

The archive is a credential. It holds every secret the platform has, in the
clear. Keep it where you would keep the cluster's root credentials, and keep it
**off the cluster it came from** — a backup that only exists on the machine
that died is not a backup. Taking one is recorded in the audit log as an
`export` against the Kitchen object, because "who took a copy of everything,
and when" is exactly the sentence an audit log exists to be able to produce.

The same thing over the API, for a cron job of your own:

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
