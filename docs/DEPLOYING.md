# Kitchen — deploying an application

You have an application in a git repository, somebody else runs the Kitchen
installation, and you want a URL. This is that path, in order.

It is the one document here addressed to you rather than to whoever installed
the platform. Where a step is genuinely theirs and not yours, it says so —
knowing which half of a problem is yours is most of what makes a platform
usable, and the boundary is not always where you would guess.

Throughout, the installation is at `kitchen.apps.example.com`, its base domain
is `apps.example.com`, and the project is `shop`. Substitute your own.

## What you need before you start

Three things, and two of them are the operator's to give you:

1. **The installation's URL** — the dashboard, at `kitchen.<baseDomain>`.
2. **An account on it.** Public sign-up is off by default, so this is not
   self-service; see [below](#1-an-account).
3. **A git connection that can see your repository.** Connections are the
   operator's — `POST /connections` requires the operator role — so if your
   repository is on a host or an organisation the installation has never
   talked to, that is a thing to ask for rather than a thing to configure.
   Ask which connection covers your repository; you will pick it by name.

You do **not** need cluster access, a kubeconfig, or to know that Kubernetes
is under there. If a step here seems to want one, it is a bug in the platform
rather than a step you are missing.

## 1. An account

Kitchen's identity provider does not take open sign-ups. There are two ways
an account comes to exist, and both start with the operator:

- **The bootstrap link.** `helm install` prints a one-time
  `https://auth.<baseDomain>/bootstrap?token=…`, which creates the first
  administrator and then closes for good — it refuses with a `410` as soon as
  the installation has any human account at all. That one is the operator's
  own, and it is not a second way in for you.
- **Social sign-up**, when the operator has turned it on
  (`auth.allowSocialSignUp=true`) and seeded a GitHub OAuth application. Then
  you sign in at `auth.<baseDomain>` with GitHub and an account is created for
  you.

Sign-up with an email and a password is off unconditionally, and so is
password reset — no mail transport ships with the platform.

**An organisation invitation cannot create an account either**, and neither
can the operator create one through any API: [docs/AUTH.md](AUTH.md) lists
both as open items rather than as things you have missed. So the honest answer
to "how do I get in" today is: ask the operator, and if social sign-up is off,
turning it on is the thing they have to do. Once your account exists, an
invitation to an organisation and a role on a project work normally.

Sign in at the dashboard, `https://kitchen.apps.example.com`.

## 2. A project

A project is one repository. Create it **in the dashboard**. The New project
dialog asks, in order: the repository, a name (derived from the repository
until you edit it), the git connection and the registry, the production
branch, and — for a monorepo — the root directory and Dockerfile path. Preview
environments are on by default, with a switch to turn them off.

The **root directory is the build root**: the directory that is built, and the
directory everything else the project declares is relative to — the Dockerfile
path, and the commit's own [`kitchen.json`](CONFIG.md). An application in
`apps/shop` whose build file is `apps/shop/docker/prod.Dockerfile` sets the two
fields to `apps/shop` and `docker/prod.Dockerfile`. Nothing above it is part of
the build, whichever strategy runs it.

**A multi-stage Dockerfile ships its last stage** unless the project says which
stage to ship. That is worth a moment, because getting it wrong does not look
like a failure: a file that builds a test image or a toolchain after the
runtime ends on one of those, and the build succeeds, pushes it and deploys it.
When the preflight finds a Dockerfile with named stages the dialog offers them,
and the choice is stored as the project's Dockerfile stage — settable
afterwards on the project's Settings tab, and overridable per commit with
`build.dockerfileTarget` in [`kitchen.json`](CONFIG.md). A stage the file does
not declare fails the build naming the ones it has, and naming a stage on a
project built with buildpacks fails it too: that lifecycle has no stages.

As you fill it in, it runs a preflight against the actual repository and tells
you what it found: the framework it detected, the port that framework listens
on, and the files it matched. A root directory that is wrong is a message in
the form rather than a failed build five minutes later. A preflight that comes
back unhappy does **not** block you — you may know something it does not.

You cannot do this step from the CLI unless you are already signed in as a
person. `kitchen projects create` exists, but it is refused for an API key:
a project's creator becomes its admin, and an admin is who issues keys. That
is deliberate rather than an omission — see [the CLI](CLI.md#signing-in).

**Creating the project starts the first build.** It is not waiting for a push.
If you were expecting to have to do something, that is the thing you do not
have to do.

## 3. The first deploy, and the URL

The build reads your repository and decides how to build it: a `Dockerfile`
wins over everything; failing that, the platform recognises Next.js, Nuxt,
SvelteKit, Remix, NestJS, Astro, create-react-app, Vite, plain Node, Go,
Python, Ruby, Java, .NET, and a directory that is already a website. A
repository it recognises nothing in **fails the build saying so**, rather than
handing a builder something it cannot build.

When it succeeds, production is published at:

```
https://shop.apps.example.com
```

The project name is the hostname. It is derived rather than assigned, which is
why you can know the URL before the first deploy has finished.

Two things the platform puts in every environment that your application can
use and could not have worked out for itself:

| Variable | What it is |
|---|---|
| `PORT` | The port to listen on. **Listen on it.** A buildpacks-built image has no other way to be told, and a port hard-coded to 3000 works only when the platform happens to agree. |
| `KITCHEN_URL` | Where *this* environment is published. A preview's hostname carries a pull request number nothing in your repository has heard of, so this is the only way the application can know its own address. |

Both are set ahead of your own variables, so a project that sets `PORT` still
wins.

## 4. Configuration: variables, and where each kind lives

Three kinds, and picking the wrong one is the most common early mistake.

### Plain variables

```sh
kitchen env list                       # every name, and whether it is set
kitchen env set NODE_ENV=production
kitchen env rm FEATURE_FLAG
```

Or the Variables panel in the dashboard.

**Values are never readable.** No route on the platform answers one, so
`kitchen env list` prints names and whether a value is set and nothing else,
and there is no `env pull`. That is not a gap to work around — it is why a
leaked dashboard session is not a leaked configuration.

A variable can have a different value in previews:

```sh
kitchen env set --preview STRIPE_KEY=pk_test_...
```

### The project's own secrets

For a credential the platform did not mint — a third-party API key:

```sh
kitchen secret set stripe-live --value-stdin < key
kitchen secret list                            # names only, never values
kitchen env set --from-secret STRIPE_KEY=kitchen-project-secrets:stripe-live
```

The last line is the one that matters: a secret is a stored value, and an
environment variable is what makes it reach the application. **A rotated
secret reaches what is already running** — the platform restarts the workloads
that read it — where a changed environment variable waits for the next
release.

### A database, and anything else the platform provisions

A **claim** asks for something the project needs — today, a Postgres. The
platform provisions it, writes the credentials into a binding, and a variable
reads them from there:

```sh
kitchen api POST /claims --data '{
  "name": "shop-db", "project": "shop", "connection": "postgres",
  "type": "postgres"}'
```

Every preview environment gets a database of its own — a branch of
production's from Neon, a fresh empty one from the platform's own Postgres —
so a review never runs against production data unless the claim says
`"previewMode": "shared"` by name. [Claims](api/claims.md) carries what each
provider declares.

**Nothing is injected automatically** — you choose the variable name and point
it at the binding:

```sh
kitchen env set --from-claim DATABASE_URL=shop-db:url
```

`shop-db` is the claim and `url` is one of the keys a Postgres binding
carries: `url`, `host`, `port`, `user`, `password`, `database`. The values
themselves are never answered back by any route, so the application reads
`DATABASE_URL` and nothing else in the platform can. [Connections and
claims](api/connections.md) is the whole of it, and the dashboard has a form
for it.

**Which to use:** a value you would paste in a chat is a plain variable; a
value you would not is a secret; a thing that has to be *created* is a claim.

## 5. Settings that belong in the repository

Anything about how the application is built and how it runs can live in a
`kitchen.json` at the top of the repository, read at every commit the platform
builds:

```json
{
  "$schema": "https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/schemas/kitchen.schema.json",
  "runtime": {
    "port": 3000,
    "health": {"path": "/healthz"}
  },
  "processes": [
    {"name": "worker", "type": "worker", "command": ["node", "worker.js"]}
  ]
}
```

The point of committing it rather than clicking it is that it travels with the
commit: a rollback replays the configuration its release was frozen with, and
a pull request that adds a worker adds the worker's declaration in the same
diff, so its preview runs with it. `kitchen config check` refuses a bad file
before you push it, with no credential and no network.

It carries no credential and cannot change anything about the project's
standing on the platform. [docs/CONFIG.md](CONFIG.md) is every key and the
short list of what a file in a repository is deliberately not allowed to
decide.

**Give it a health path.** Without one the platform can only make a TCP
connect to your port, which says the process started rather than that it
works — so a deploy takes traffic before the application is ready to serve it,
on every deploy and on every rollback, which is the one deploy that must not
add a second outage to the one it is fixing.

**And put a migration in a `task`, not in your entrypoint.** A readiness check
stops traffic reaching a pod that is not ready; it does nothing about the
previous release's pods being retired while a migration is half applied, and a
migration run from the entrypoint runs once per replica, at once, on every
rollout. A task is one run per deploy that the platform waits for:

```json
{"processes": [
  {"name": "migrate", "type": "task", "command": ["npm", "run", "migrate"], "timeout": "10m"}
]}
```

Nothing of that release takes traffic until it succeeds, and if it fails the
deploy stops there with the run's output on the environment — whatever was
serving keeps serving. It runs in previews too, against the preview's own
database branch. **Undoing a schema change is yours, not the platform's**:
write forward-only, idempotent migrations, because a rollback runs the task the
older release declared and nothing runs a "down" step.

## 6. Previews

**Every pull request gets its own environment**, at:

```
https://shop-pr-42.apps.example.com
```

You do not ask for one. Open the request, and the platform builds the commit,
deploys it, and writes a comment on the request carrying the URL, the status,
the commit and a link to the environment in the dashboard. It rewrites that
same comment on every deploy rather than adding another, and turns it into
"the preview environment has been removed" when the request closes.

**Previews are gated behind platform login by default.** An anonymous visitor
is sent to sign in first — the comment says so, because a reviewer who is not
a platform user otherwise reads the sign-in page as a broken link. Somebody
signed in who is not on the project meets a page saying exactly that, and
naming what would fix it: **any role on the project is enough, `viewer`
included**. Your application needs no changes for this; the gate sits in front
of it, and it reserves the path prefix `/_kitchen/gate/` on protected
hostnames, so do not serve anything there.

If your previews are meant to be shown to people who will never have an
account, turning protection off is a project setting and a deliberate one.

Two things worth knowing about a preview that looks slow or gone:

- **A preview idles to zero when nobody is looking at it**, by default, and
  the next request wakes it. The first one after a quiet spell pays a cold
  start. If your application does work nobody asked for — a poller, an ingest
  loop — say so with `runtime.notRequestDriven` in `kitchen.json`, or its
  data will have gaps in it that mean something it did not do.
- **It is torn down as soon as its pull request closes**, not after a grace
  period. `previews.ttlAfterClosed` is on the project and is not honoured
  yet; do not plan around it.

## 7. From a terminal

The CLI is a client of the same API the dashboard uses. It holds no
kubeconfig and talks to no cluster.

```sh
kitchen login --api https://kitchen.apps.example.com --api-key-stdin < key
kitchen link --project shop          # writes .kitchen/project.json — commit it
kitchen deploy                       # build the current commit, and follow it
kitchen logs --follow
kitchen status                       # what is running, and what has been built
```

The key comes from a project's People tab in the dashboard — a key is a
non-human member of exactly one project, which is why it is issued beside the
people rather than somewhere of its own. There is no browser
sign-in: the identity provider's OAuth plugin implements no device grant, so
there is nothing for a CLI to poll — [the CLI](CLI.md#why-there-is-no-browser-sign-in)
has the whole argument, and it is a decision rather than a gap.

`kitchen deploy` builds **a commit that has been pushed** — the build clones
it, so a commit that only exists on your laptop cannot be built. It streams
the build's own log, and exits `9` if the build fails, which is how a script
tells "the deploy did not reach the platform" from "the application did not
compile". It exits `12` for the third answer: the build succeeded and the
environment refused the release — a deploy task that failed, most often — so
what was serving before it still is.

In CI, skip `login` and set `KITCHEN_API_KEY`.

`.kitchen/project.json` holds no credential and is meant to be committed:
everybody working on the repository deploys the same project.

Every command takes `--json`, puts JSON on stdout and nothing else under it,
and never blocks on a prompt. If you are writing a script or an agent against
it, [docs/AGENTS.md](AGENTS.md) is the contract in one page.

## 8. A custom domain

The generated URL is real and permanent; a custom domain is an address you own
pointed at an environment.

```sh
kitchen api POST /domains --data '{"hostname": "shop.example.com", "environment": "shop-production"}'
```

There is no `kitchen domains` command yet — `kitchen api` reaches the route,
and the dashboard has a panel for it.

Creating it changes no traffic, and it is a `developer` operation rather than
the operator's. The response, and `GET /domains/{name}`, carry a
`verification` block with the exact record to create in **your** zone, so you
never have to compute one: a TXT record at `_kitchen-challenge.shop.example.com`
whose value the platform gives you, or a CNAME from `shop.example.com` to
`shop.apps.example.com` that both proves ownership and points traffic at the
platform. Create the record, and the `Verified`, `CertificateReady` and
`RouteProgrammed` conditions walk the rest of the way. In ACME mode the
certificate is issued over HTTP-01 through the platform's Gateway, so it
finishes only once the hostname resolves there.

The DNS records are yours. The platform never touches your zone.

## 9. When it breaks

**Read the build first.** A failed build answers with the container that
stopped it, its exit code and the last of what it printed — the underlying
Job's own message is the same sentence for every build that ever failed, so
ignore that one.

```sh
kitchen builds --json | jq '.items[0].failure'
kitchen logs --build shop-bld-abc123def456 --json
```

A build that is sitting in `Queued` has a reason, and the reason tells you
whose problem it is:

| Reason | Whose |
|---|---|
| `SourceUnreadable` | Not yours. The platform cannot reach the git provider right now; the build waits and retries. |
| `RepositoryUnreadable` | The connection's credential cannot see the repository. Operator's, or the wrong repository. |
| `FrameworkNotDetected` | Yours. No Dockerfile, and nothing recognised. |
| `ConfigInvalid` | Yours. The commit's `kitchen.json` is wrong; the message says how. |
| `DockerfileTargetNotFound` | Yours. The Dockerfile declares no stage by the name asked for; the message names the ones it has. |
| `DockerfileTargetNotSupported` | Yours. A Dockerfile stage was asked for on a commit built with buildpacks, which has none. |
| `SourceUnreviewed` | The project requires a reviewed pull request for production commits and this one arrived without. |

**Running, but wrong:**

```sh
kitchen logs --follow                              # production, as it happens
kitchen logs --since 1h --search 500               # a duration means "that long ago"
kitchen logs --environment shop-pr-42 --follow     # a preview, by name
```

**Roll back.** A release is an immutable snapshot of an image *and the
configuration it was built with*, so a rollback is not a rebuild — it is
pointing the environment at an older one, and it is instant.

```sh
kitchen rollback
```

With no release named it goes back one. Before it asks
`Move shop-production from … to …?` it prints the difference between where the
environment is and where it is going — the variables that change, the runtime
fields that move, the processes that differ, and **no values, because it
fetches none**. A confirmation that repeats the release name back is not a
safety mechanism, since the release name is the one thing you already knew;
the diff is.

If the target environment declares requirements of its own, the answer is a
`202` and a promotion rather than an immediate move —
`kitchen promotions <name>` follows it.

## What is the operator's, not yours

Ask rather than work around, and none of it needs you to have cluster access
either:

- **Connections** — a git provider, a registry, a database provider. Creating
  one requires the operator role.
- **An account for a new colleague**, until the invitation gap above closes.
- **The base domain, TLS, and the wildcard certificate.** Everything under
  `*.apps.example.com` is already routed; your custom domain is yours.
- **Whether previews idle to zero**, how many builds run at once, and the
  platform's default build strategy.
- **Anything that reads as "the platform is broken"**: builds that never
  start, an environment stuck deploying, a URL that resolves nowhere. The
  platform is meant to be legible from underneath, and that view is theirs.

Three things you might expect to be theirs and are not: **custom domains and
resource claims**, both of which a project `developer` creates and deletes;
**your project's settings**, since you are its admin if you created it; and
**your project's members** — you invite people to your own project.

## Where to go next

| | |
|---|---|
| [CONFIG.md](CONFIG.md) | `kitchen.json`: every key, and what a repository may not decide |
| [AGENTS.md](AGENTS.md) | The same ground for a coding agent, with the machine contract |
| [CLI.md](CLI.md) | Every command, its flags, and the JSON surface |
| [API.md](API.md) | The routes, the roles that gate them, and how to get a token |
| `docs/api/<resource>.md` | One page per resource, with a runnable `curl` for each operation |
