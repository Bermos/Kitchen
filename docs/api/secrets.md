# Kitchen — A project's own secrets

A credential the platform did not mint: a database the project runs itself, a
third-party API key, an SMTP password. The application needs it, and nothing
about the platform can produce it.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Why this is a resource of its own

Until it existed, an application had two ways to be given a credential and
both were bad.

**`kubectl create secret` in the application namespace.** That is precisely the
thing the platform exists to avoid — it is invisible to the audit log, it is
undone by anything that recreates the namespace, and it puts cluster access
back on the path of a routine change.

**`env[].value`.** The credential goes into the Project spec in cleartext,
where every member of the project can read it, the API reads it back, and the
audit log records it in a before-and-after.

So a project secret is neither. The value goes in and never comes back out —
the same bargain a [connection's credential](connections.md) makes — and the
project's configuration holds a *reference* rather than a value.

## Setting one

```sh
curl -sS -X PUT -H "authorization: Bearer $TOKEN" \
  -d '{"value": "s3cr3t"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/secrets/SMTP_PASSWORD
```

The name is the last path segment and is the key an environment variable
references: letters, digits, `-`, `_` and `.`, at most 253 characters. `value`
is required and at most 64 KiB — a credential is a line of text, and the limit
is here so the refusal names the cap rather than being the API server's
complaint about an object already too big to write.

**Setting and rotating are the same request.** A `PUT` on a name that is
already there replaces the value and answers `200`; a name that is new answers
`201`. There is deliberately no way to ask which it will be first: that
question is only answerable by something that can read a value.

The answer is the name and the reference, and cannot be anything else:

```json
{"name": "SMTP_PASSWORD", "reference": {"name": "kitchen-project-secrets", "key": "SMTP_PASSWORD"}}
```

`reference` is served rather than left to be worked out, so that nothing has to
know the name of the object the platform keeps secrets in. It is exactly what
`fromSecret` takes:

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"env": [{"name": "SMTP_PASSWORD",
        "fromSecret": {"name": "kitchen-project-secrets", "key": "SMTP_PASSWORD"}}]}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/env
```

## Reading them

`GET /projects/{name}/secrets` answers `{"items": [...]}` of exactly the shape
above — names and references, sorted by name. **There is no route on this
platform that answers a value**, here or anywhere else, so a listing reveals
nothing and is a `viewer`'s read like the variables it belongs to.

Who set each one and when is the audit log's answer rather than a field here,
which is the stronger one: it cannot be edited, and it is one query.

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  "https://kitchen.apps.example.com/api/v1/audit?kind=ProjectSecret&project=shop"
```

Every write leaves a record classified as a credential change — the same
`?privileged=true` view a connection's rotation appears in — carrying the
secret's name, whether the write replaced a value, and never the value.

## Removing one

`DELETE /projects/{name}/secrets/{secret}` answers `204`, or `404` for a name
the project does not have.

A secret an environment variable still reads is **refused** with a `409` naming
the variables that read it. That is a refusal rather than a warning: a variable
pointing at a secret that is not there leaves the container unable to start,
and the undo for a value nobody has any more is to go and find it again.

## Where the value actually goes

The API writes one Secret per project in the platform namespace,
owner-referenced by its Project, with one key per secret. `ProjectReconciler`
mirrors it into the application namespace as `kitchen-project-secrets` on every
reconcile — which is what makes the copy the recoverable one: a namespace
somebody empties is refilled by the next pass, because the values were never
only there.

Two consequences worth knowing:

- **A rotation reaches what is already running.** The workloads that read a
  project secret carry a digest of the values they read on their pod template,
  so replacing one rolls those pods and leaves every other workload alone. That
  is different from an environment variable's own value, which lands in the
  next release. It has to be: the release snapshot holds the *reference*, so
  without the roll a rotated value would reach some pods and not others,
  whenever each happened to restart next. The digest covers every Secret the
  workload reads, not only these — see [below](#what-rolls-and-what-does-not).
- **The secrets go with the project.** The application namespace is deleted by
  the project's finalizer and takes the mirrored copy with it; the source in
  the platform namespace is deleted by the same finalizer, by name, because
  nothing owner-references across namespaces and the finalizer is this
  platform's garbage collector.

## What rolls, and what does not

The digest is not about project secrets in particular. Every workload an
environment materializes — the web process and each worker — is stamped with a
digest of **the Secrets that workload reads**, whichever they are:

- a variable naming one key of one Secret, which is what `fromSecret` and a
  [claim's binding](claims.md) both become;
- an `envFrom` taking every key of one;
- a file mounted from one.

So a claim's provider replacing a binding's password reaches the pods reading
that binding, exactly as a project secret does, and a workload reading neither
is not touched. Only the referenced keys are hashed, so adding a value to a
Secret several workloads share rolls the ones whose values moved and no
others.

Two deliberate exceptions:

- **A scheduled job is not stamped.** Its next run is a new pod, which reads
  whatever the Secret holds when it starts. There is nothing to roll.
- **`imagePullSecrets` is not in the digest.** The kubelet reads the pull
  credential at pull time rather than handing it to the process, so a rotated
  registry password is in use the moment it is written. Rolling on it would be
  a restart that changes nothing — and on a workload deployed by recreation,
  an outage that changes nothing.

**The restart says why.** Each roll a rotation causes is an entry in the
[activity feed](audit.md) typed `secret.rotated`, naming the workload and what
it reads. A workload that runs one at a time — a `singleton` process, or one
mounting a volume that attaches once — is deployed by recreation rather than
rolled, so its old pod stops before the new one starts; the entry says so,
because that is a brief gap rather than a rollout.

## What this is not

**Syncing from an external secret manager.** Kitchen is the store here, which
for a self-hosted platform is the common case. Pulling values from something
that already holds them is a different shape and a separate open item.

## From the terminal

`kitchen secret` is the whole of it, and never reads a value back either:

```sh
kitchen secret list
kitchen secret set SMTP_PASSWORD                       # prompts, without echoing
kitchen secret set SMTP_PASSWORD --value-file ./smtp   # or from a file
pass show smtp | kitchen secret set SMTP_PASSWORD --value-stdin
kitchen secret rm SMTP_PASSWORD
```

`kitchen secret list` prints the reference beside each name, so the
`kitchen env set … --from-secret` that follows never has to be typed from
memory. See [CLI.md](../CLI.md).
