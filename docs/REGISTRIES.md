# Kitchen — image registries

Every project names a registry, and everything a deploy is made of passes
through it: the build pushes the image, the release names it by digest, and the
environment's pods pull it back. The registry is a `dockerRegistry` Connection,
which is one of the two connections a project cannot exist without.

This page is how to make one for each of the registries that actually come up —
the one Kitchen ships, GitHub's, Harbor, and a plain Distribution — and what
goes wrong with each. [docs/api/connections.md](api/connections.md) is the
reference for the request bodies below; this is what to put in them.

Connections are the operator's: `POST /connections` requires the operator role,
and a developer sees registries as names in a picker. If you are deploying an
application and the registry you need is not on the list, that is a thing to
ask for rather than a thing to configure — see
[docs/DEPLOYING.md](DEPLOYING.md).

## What a registry connection is

Three fields, whichever registry it points at:

```json
{"name": "harbor", "provider": "dockerRegistry",
 "config": {"url": "harbor.example.com/kitchen"},
 "credential": {"username": "robot$kitchen", "password": "…"}}
```

- **`config.url` is a prefix, not a host.** It is what an image is named under;
  the build appends the project and the tag, so
  `harbor.example.com/kitchen` produces
  `harbor.example.com/kitchen/shop:<sha>`. The host in front of the first
  slash is what the build authenticates against, over `https` unless the URL
  says `http://`.
- **The credential is a username and a password**, stored as a
  `dockerconfigjson` in a Secret the platform manages. **The API never reads it
  back** — no response echoes it, and rotating it means sending a new one.
- **The push credential is the pull credential.** The build copies that docker
  config into the project's application namespace, and the environment's
  Deployment names the same Secret in its `imagePullSecrets`. A private
  registry therefore needs one credential that can do both, which is the thing
  most of the failures below have in common.

### The credential a build holds, and the one third-party code holds

A connection's credential is per connection and not per project — except on
the bundled registry, where every project has accounts of its own (see
[below](#every-project-is-an-account-of-its-own)). Several pods the platform runs contain code the platform did not
write — the buildpacks lifecycle runs the repository's own build, a quality
gate and a vulnerability scanner are images an operator chose — and reading a
docker config off the filesystem is one line of a `postinstall` script. So a
credential is mounted into the container that needs it and into no other, and
where a **read-only** credential exists, that is the one those containers get
(#424):

| Pod | What holds the connection's credential | What holds a credential that cannot push |
|---|---|---|
| Dockerfile build | `buildkit` — the daemon, which a `RUN` step can very probably reach, so on the bundled registry it is the project's own push account | — |
| Buildpacks build | `exporter`, which pushes, and `analyzer`, which validates write access to the tag | `restorer`; `detector` and `builder`, which run the repository's own build, mount **no** credential at all |
| Quality gate | `publish`, which writes the findings back | `gate` |
| Vulnerability rescan | `sbom` and `publish` | `scan` |
| Vendored SBOM | `publish` | `sbom`, the generator |

**The line is what a container does to the registry, not which one is "the
push".** The buildpacks `analyzer` phase is handed the output tag and verifies
read *and* write access to it before the build starts — deliberately, so that
a build that cannot publish fails in seconds rather than after the whole
build — so it holds the credential that can push, alongside `exporter` (#534).
What the split protects is narrower than "one pushing container" and is
untouched by that: no phase that runs the repository's own code holds any
credential at all.

The bundled registry issues one per project: `<project>.pull`, which may pull
the project's own repositories and push to none, beside `<project>.push`, which
the build pushes with. Both are copied into the project's application
namespace and no other.

**A registry the platform cannot mint from is not left to be discovered.** The
connection's `status.registry` says which of the two this installation is, and
the connections page prints it under the connection:

```json
{"scopedCredentials": false,
 "message": "this registry issues no credential narrower than the connection's own, so a quality gate and a vulnerability scanner read an artifact with the credential builds push with. Store a read-only credential for this registry as the Secret kitchen-connection-ghcr-read to narrow it."}
```

Supplying one is exactly that: a `kubernetes.io/dockerconfigjson` Secret in
`kitchen-system`, named after the connection's own credential Secret with
`-read` on the end, holding an account of the same registry that may pull and
not push — a Harbor robot account with pull rights, a GHCR token with
`read:packages` alone, a second Distribution account. The platform picks it up
on the next reconcile of the connection and every gate and scan after that
reads with it. Nothing about builds changes either way: the push is the push.

Create one on the dashboard's Connections page, or from a terminal:

```sh
kitchen api POST /connections --data '{"name": "ghcr", "provider": "dockerRegistry",
  "config": {"url": "ghcr.io/acme"},
  "credential": {"username": "octocat", "password": "ghp_…"}}'
```

There is no `kitchen connections` command; `kitchen api` carries the route.

## The registry Kitchen ships

A fresh installation already has one. The chart runs
[zot](https://zotregistry.dev/) with a volume, the operator publishes it at
`registry.<baseDomain>` on the shared Gateway and seeds a `dockerRegistry`
Connection pointing at it, so the first project created after `helm install`
has a registry to pick with no account to open anywhere.
[The chart's README](../charts/kitchen/README.md#the-bundled-registry) is the
whole of it — retention, media types, the volume. Three things about it decide
whether you want it, and all three are reasons somebody ends up on this page:

**It is published on the internet on purpose.** The node's container runtime is
what pulls an image, and it trusts neither a Service address with an in-cluster
CA nor plain HTTP unless the node has been configured to. Every other
in-cluster registry solves that at the node — an insecure-registry entry, a CA
in the runtime's trust store, a DaemonSet writing `certs.d` — and Kitchen is a
chart installed into someone else's cluster, where Cilium and a StorageClass
are the only prerequisites. A route on the shared Gateway rides the platform's
own publicly trusted wildcard certificate, which is the one address that asks
the node for nothing. The cost is stated rather than hidden: pulls leave the
cluster and come back through the Gateway, and the registry admits nobody
anonymously because it is reachable from outside.

**It does not exist in `tls.mode: none`.** It has nothing to be published on:
there is no trusted certificate, so nothing is rendered, `RegistryReady` on the
Kitchen object is False with the reason `TLSModeNone`, and every project on
that installation needs a registry connection of its own. If you are choosing
between this and GHCR, that is the first question to settle.

**Every project is an account of its own.** See the next section; it is the
reason a project's pull secret is not a key to every other project's images.

**The seeded Connection is a seed, not a fixture.** The operator creates it
once, records the fact in `status.registry.connection`, and never creates it
again. Delete it and it stays deleted — an installation that would rather use
GHCR or Harbor has to be able to end up with only its own connections, and a
platform that kept reinstating this one would make that impossible. Somebody
following the GHCR section below is exactly the person who will delete it, and
it will not come back. While it is there and still carries
`app.kubernetes.io/managed-by: kitchen`, its URL and credential are kept in
step with the registry.


### Every project is an account of its own

Until this, the bundled registry had two accounts, one that pushed and one that
read, and both were copied into every application namespace. A project's pull
secret was a key to every other project's images, and a Dockerfile build's
credential — which a `RUN` step can very probably reach through the rootless
BuildKit daemon — was a key to push over them. Now every project gets two
accounts, created with the project and deleted with it:

| Account | May | Held by |
|---|---|---|
| `<project>.push` | pull and push `<project>` and `<project>/…` | the build's pushing containers |
| `<project>.pull` | pull `<project>` and `<project>/…` | the kubelet (`imagePullSecrets`), gates, scanners, the buildpacks restorer |
| `kitchen` (`registry.auth.username`) | pull, push and delete anywhere | the platform alone: the seeded Connection, evidence and retention |
| `kitchen-read` (`registry.auth.readUsername`) | pull anywhere | the platform alone |

The passwords live in `kitchen-registry-account-<project>` in `kitchen-system`,
owned by the Project, and no response ever echoes them. The application
namespace gets two docker configs built from them, named exactly as the copies
of the connection's credential always were, so nothing that mounts them
changed.

**The registry checks no password.** zot's accounts and access control are
files it reads at startup, and a restart per project created is not worth
paying, so it is configured for the Docker token flow instead: a client is sent
to `https://registry.<baseDomain>/kitchen/token`, which the route delivers to
the operator's registry token service, and zot accepts the token that service
signs with a key the chart generated. That service is where a password is
checked and where the scope is decided — the repository a token names must be
the project's own name or start with it and a slash. A project being deleted
gets no more tokens.

**A workload's image is now under its project.** A project's second and later
processes used to be pushed beside it, as `<project>-<workload>`, which meant
project `shop`'s `api` and project `shop-api` were the same repository. On the
bundled registry they are pushed to `<project>/<workload>`; a project name
holds no slash, so that is a boundary no other project's name can cross. Other
registries keep the old shape, because GHCR and Harbor are not namespaced
this way by Kitchen.

**Existing projects need nothing done.** On its first reconcile after the
upgrade the operator marks the seeded Connection as one with per-project
accounts, and the project reconciler creates every project's accounts and
writes the project's own credentials over the shared copies in its application
namespace — the same Secret names, so running Deployments pick them up on
their next pull without a rollout. An image an older release still
runs at `<project>-<workload>` stays pullable by that project's read account —
read off the Releases that run it, never matched by pattern — until no Release
names it.

**What this does not close.** zot deduplicates blobs across repositories, and
an account that may pull any repository can fetch a blob by digest, or mount
one into its own repository, if that digest exists anywhere in the registry.
Manifests and tags are not reachable this way, and a digest is not something
one can guess, but a layer's digest is not a secret either: it is in every
manifest that uses it. Treat the registry as separating who can push and who
can enumerate, not as hiding content from a party that already knows a layer's
digest.

## GitHub Container Registry

GHCR is an ordinary OCI registry and the `dockerRegistry` provider speaks to it
unchanged. Both of the things that go wrong are GitHub's rules rather than
Kitchen's, and neither announces itself as a credential problem.

```json
{"name": "ghcr", "provider": "dockerRegistry",
 "config": {"url": "ghcr.io/acme"},
 "credential": {"username": "octocat", "password": "ghp_…"}}
```

`config.url` is `ghcr.io/<owner>`, where the owner is the user or the
organisation the packages belong to — **lowercase**, because image paths must
be and GitHub logins need not be. `Acme` is a valid login and
`ghcr.io/Acme/shop` is not a valid image name. The username is your GitHub
login; GHCR does not really rule on it, the token is what it rules on.

### The token has to be a classic PAT with `write:packages`

This is the part people lose an hour to. GitHub's own
[Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
says it plainly: *"GitHub Packages only supports authentication using a
personal access token (classic)"*. Fine-grained tokens are refused, and so are
GitHub App installation tokens
([community discussion #171423](https://github.com/orgs/community/discussions/171423)).
So the token for a GHCR connection is a
[classic token](https://github.com/settings/tokens/new?scopes=write:packages&description=Kitchen)
with **`write:packages`** ticked, which selects `read:packages` with it — the
push needs the first and the pull needs the second.

**`write:packages` is not covered by `repo`.** They are separate scopes, and
this is where the hour goes: a token with `repo` clones the repository
perfectly well, so the git connection tests green, the build starts, the image
builds, every layer uploads — and the push fails with a **`403`** in the last
seconds of the build. It reads like a broken build rather than a broken
credential, and the build log is where you find it, at the very bottom. If a
build fails at the push and nothing else, that scope is the first thing to
check.

A GHCR connection can also **test green and still fail that way**. Testing a
`dockerRegistry` connection asks the registry the question `docker login`
asks — `GET /v2/`, and then the token service the challenge names — and no
registry answers a login with what the credential may later push. The test
rules on the credential's existence, not on its scopes.

### A newly pushed package is private

The first push of an image creates the package, and GitHub creates it
**private**. A private package needs a credential to pull exactly as it needed
one to push.

Kitchen does that itself: the build syncs the connection's docker config into
the project's application namespace as `kitchen-registry-<connection>`, and the
environment's Deployment names that same Secret in `imagePullSecrets`. The
push credential and the pull credential are one credential, so the ordinary
case just works and there is nothing to configure.

It stops working when they stop being the same credential — a token rotated to
one that can write to the organisation but cannot read that package, or a
project pointed at a second GHCR connection whose token has never seen it. What
that looks like is a build that reads green and pods in **`ImagePullBackOff`**:
the deploy fails *after* the image exists, so the build log has nothing in it
and the answer is on the environment instead, where the diagnostics strip fires
`workload.imagepull` and names the image it could not pull (see
[docs/OBSERVABILITY.md](OBSERVABILITY.md)).

Two ways to stop depending on the credential for the pull, both of them
GitHub's own package settings:

- **Link the package to its repository.** Give the image the
  `org.opencontainers.image.source` label pointing at the repository — a
  `LABEL` line in the application's own Dockerfile — or connect it by hand
  from the package's own settings page. A linked package
  can then be set to inherit that repository's access, so anyone who can read
  the repository can pull it.
- **Make the package public.** *Package settings → Change visibility →
  Public.* Anonymous pulls then work, and the credential is only doing the
  push.

Neither is required — the credential sync covers a private package — but both
are worth knowing, because a package that has been made public is the one case
where a wrong pull credential goes unnoticed until the day the package is
private again.

## Harbor

Harbor's unit of access is a **project**, and its credential is a **robot
account** scoped to one:

```json
{"name": "harbor", "provider": "dockerRegistry",
 "config": {"url": "harbor.example.com/kitchen"},
 "credential": {"username": "robot$kitchen", "password": "…"}}
```

The URL is `<host>/<harbor-project>` — the Harbor project is the path segment,
and every Kitchen project becomes a repository under it. Make the robot account
in *Harbor project → Robot Accounts*, with **push** and **pull** on
repositories; push alone leaves the pods unable to pull what the build just
made. Harbor prefixes the name it gives you with `robot$`, and that whole
string is the username — including the `$`, which a shell will eat if the
credential goes through one unquoted.

Harbor answers `/v2/` with a Bearer challenge and checks the password at its
own token service, which is what the connection test follows, so a wrong robot
password is one of the failures a test does catch.

## A plain Distribution registry

[Distribution](https://distribution.github.io/distribution/) — the registry
`registry:3` runs — and anything else that speaks the OCI distribution API
needs nothing special:

```json
{"name": "internal", "provider": "dockerRegistry",
 "config": {"url": "registry.internal.example.com/kitchen"},
 "credential": {"username": "kitchen", "password": "…"}}
```

Two things decide whether it works, and neither is Kitchen's:

- **The node has to trust its certificate.** The pull is the kubelet's, not the
  pod's, so a certificate signed by an internal CA works only where every node
  already trusts that CA. This is the whole reason the bundled registry is
  published on the platform's public wildcard instead. A plaintext registry can
  be named as `http://registry.internal:5000/kitchen` and the build will honour
  it, but the node still has to be configured to allow an insecure registry
  before anything can be pulled.
- **Nothing garbage-collects it for you.** Every build pushes a tag and nothing
  here ever deletes one. Distribution's garbage collector needs the registry
  stopped, which is why the bundled one is zot; on your own Distribution, that
  is a maintenance job you own.

## Testing a connection, and what a test cannot say

`POST /connections/test` — the *Test* button on the connections page — runs the
credential past the registry **without storing anything**, so a token that
turns out to be wrong leaves nothing to clean up. It asks `/v2/` with the
credential and follows a Bearer challenge to the token service the registry
names, which is the same exchange `docker login` performs.

That means it rules on exactly one thing: whether the registry accepts this
username and password. It cannot tell you whether the credential may push to a
repository that does not exist yet, whether a GHCR token carries
`write:packages`, or whether the package it will create can be pulled
afterwards. A green test and a `403` at the end of a build are consistent with
each other, and the second is the one that has the answer in it.
