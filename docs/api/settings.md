# Kitchen — Settings and updates

The settings a chart value would otherwise be the only way to reach, and the
self-update the platform performs on itself.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Settings

`GET /settings` is the `Kitchen` singleton as a view: the base domain, the
derived API and issuer URLs, the gateway's address and conditions, the defaults
the platform builds and retains telemetry with, and **who the platform's
operators are**:

```json
{"baseDomain": "apps.example.com", "buildConcurrency": 2, "logRetentionDays": 30,
 "operators": [{"subject": "user_01H8X…", "email": "anna@example.com"}]}
```

`operators` is `spec.access.operators`, the list every `operator` requirement
in the table above is resolved against. It is on this route because this route
already carries the base domain, the issuer and the gateway address and is the
operator's for that reason — and because a list that is enforced against and
seeded on upgrade, but served by nothing, is one somebody has to open `kubectl`
to read.

Three states, and they are three: `null` means nobody has ever said who the
operators are and the reconciler will seed the list from the accounts that
exist; `[]` means somebody narrowed it to nobody; a list means what it says.
The field carries no `omitempty` for exactly that reason.

`PATCH /settings` changes the fields that are safe to change at runtime:

```json
{"buildStrategy": "auto", "buildConcurrency": 2, "logRetentionDays": 30,
 "operators": [{"email": "anna@example.com"}, {"subject": "user_01H8X…"}]}
```

Fields left out stay as they are, `operators` included — a settings patch that
does not mention the list cannot disturb it. When it does, the list replaces
the old one wholesale, and each entry names its account the same two ways a
[membership write](./projects.md#who-is-on-a-project) does: an `email` the platform resolves
to the issuer's `sub` before anything is written, or a `subject` taken as
given. Exactly one of the two, an address the identity provider has never heard
of is a `404` about the person, and the same account twice is a `400`.

**The last operator cannot be removed**, for the reason the last admin on a
project cannot:

```json
{"error": "the operator list cannot be emptied: a platform with no operator has nobody left who can appoint one, and the only way back is editing the Kitchen object with kubectl. Name whoever is to stay — remove the others, and keep the last"}
```

That is a `409`. Handing the platform to somebody else in one write is fine —
the rule is about the list being emptied, not about who is on it. Every change
to it is recorded in the [audit log](./audit.md#the-audit-log) as an update to the `Kitchen`,
naming who came on and who came off, the way a membership change names the
member.

**A patch that carries `operators` also carries the caller's
`resourceVersion`**, so two operators editing the list at the same time is a
`409` for the second rather than a lost update. It has to be: the list is
replaced wholesale and the last-operator check was made against the list the
request read, so without the lock two admins removing each other put each
other back — `[A, B, C]`, A removes C and B removes A, and the result is
`[B, C]` with C returned to a list they had been taken off. Re-read and try
again. A patch that does not mention `operators` is not locked: its fields are
independent scalars, and failing "set the build concurrency to 4" because
somebody moved the log retention a moment earlier would be a conflict about
nothing.

Everything else on the singleton — the base domain, the issuer, the ingress —
shapes URLs and credentials the platform has already handed out, so changing
those stays a deliberate kubectl operation.

## Updating the platform

`GET /updates` answers what the installation is running, what has been
published since, and what it has already attempted:

```json
{
  "enabled": true,
  "currentVersion": "0.2.0",
  "latestVersion": "0.3.0",
  "available": true,
  "upgradableTo": ["0.2.2", "0.2.1"],
  "allowMinor": false,
  "checkedAt": "2026-02-03T10:15:00Z",
  "items": [{"name": "update-0-2-1-h4k9c", "version": "0.2.1", "phase": "Succeeded", "fromVersion": "0.2.0"}]
}
```

`upgradableTo` is what this installation would actually accept, so it is not
simply everything newer: `latestVersion` here is `0.3.0` while the offer stops
at `0.2.2`, because `allowMinor` is false and pre-1.0 the minor is where
breaking changes land. `enabled` is false on an installation whose chart was
not installed with `selfUpdate.enabled=true`, and `reason` then says so — the
running version is still reported, because that is the first thing anyone
asks. An installation that cannot reach the chart registry gets
`discoveryError` and no candidates, and can still be given a version by hand.

The published versions are read from the chart's OCI repository and cached for
an hour, so `checkedAt` says when the list was taken rather than implying it is
current. `?refresh=true` asks the registry again instead — what the settings
page's re-check does, and the answer to a release published minutes ago that
the platform would otherwise not see for an hour. Forced listings are floored
at one every ten seconds: a registry that rate-limits this installation
answers with an error the client caches for five minutes, which is worse than
the staleness being skipped. A value the flag cannot be read as is a `400`
rather than a silent `false`.

`POST /updates` starts one:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"version": "0.2.1"}' \
  https://kitchen.apps.example.com/api/v1/updates
```

A version is the only field, and an unknown field is a `400` rather than
something ignored. That is not tidiness: the job that runs the upgrade holds
cluster-admin, so an endpoint that forwarded helm arguments would be a way to
apply anything at all with it. The operator builds the whole `helm upgrade`
invocation itself.

The answer is `201` with the created upgrade; watch it with
`GET /updates/{name}` for the phase, and `GET /updates/{name}/logs` for what
helm is actually saying. The version in the sidebar changes when the new
operator comes up, which is the last thing to happen rather than the thing to
watch. `409` means self-update is not enabled on this installation.
Everything else the platform refuses — a downgrade, a version it is already on,
a minor crossing without `selfUpdate.allowMinor`, a second upgrade while one is
in flight — is accepted here and refused by the operator, which records the
reason on the `PlatformUpdate` rather than losing it: the checks are about the
state of the cluster at the moment the job would start, not about the request.

Requests are attributed: the caller's name is annotated onto the object and
reported as `requestedBy`.

See [Letting the platform update itself](../../charts/kitchen/README.md#letting-the-platform-update-itself)
for what enabling it grants.

### An upgrade's output

`GET /updates/{name}/logs` is helm's own output for one upgrade, out of the
telemetry store rather than off the pod:

```json
{"items": [{"timestamp": "2026-02-03T10:16:04.118Z", "source": "platform",
            "pod": "kitchen-self-update-update-0-2-1-h4k9c-9v2pd", "container": "helm",
            "stream": "stdout", "message": "Release \"kitchen\" has been upgraded. Happy Helming!"}]}
```

| Parameter | Meaning |
|---|---|
| `limit` | Lines to return, default 200, capped at 5000. The newest are kept |
| `since` / `until` | RFC 3339 bounds on the window |
| `search` | Keep only lines containing this substring, case-insensitively |

The selection itself is composed from the update — the job's pod, in
`kitchen-system`, in the container helm ran in — and is not a parameter.
`q` and `where`, which [the log endpoints](logs.md) take, are not accepted
here: this reads the platform's own namespace, where the API, the operator and
the identity provider also write, so what the caller may say is what narrows.

The job's pod is reclaimed an hour after it finishes and the lines outlive it,
so an upgrade that ran last month answers as readily as the one running now.
An update with no `status.jobName` — one that failed the operator's preflight,
or that has not been reached yet — answers `200` with an empty `items`: it
never had a pod, and what happened to it is on the record itself, in its phase
and its message. An update that does not exist is a `404`, and an installation
without a telemetry store is a `503`.

The endpoint streams when asked to, exactly as the log endpoints do:
`Accept: text/event-stream` answers the current page and then every line that
arrives after it, one `data:` event per line; a plain GET on the same URL still
answers the bounded page.

Be honest about what there is to watch. The upgrade runs
`helm upgrade --atomic --wait`, which says that it has started, and then says
almost nothing until it has either finished or rolled back — there is no
progress to render in between. This is where a *failure* is explained: which
resource never became ready, what the rollback did. For "is it done yet", the
phase on `GET /updates/{name}` is the answer.
