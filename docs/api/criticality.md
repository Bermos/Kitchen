# Kitchen — Criticality and disruption tolerance

**Kitchen does not decide what is critical, and does not set the tolerances.**
That is a board's judgement about the institution's functions, and it is
explicitly out of scope. What the platform does is carry the designation once
somebody has made it, map it onto the resources that actually serve the
function, and hold the estate to the tolerances that came with it.

The mapping is the part worth having. "Which systems support this critical
function, and which third parties are behind them" is one of the most commonly
cited supervisory findings, and every institution that has been cited for it
was maintaining the answer by hand across four systems. Kitchen gets it nearly
free, because the graph is reconciled rather than maintained: the answer is a
traversal made on the request, and there is nothing to keep in step.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table.

## The vocabulary

`criticality` is an ordered designation:

```
nonCritical  <  important  <  critical
```

Absent means **undesignated**, and is answered as that word rather than as a
blank — a blank cell in an export invites a generous reading. `nonCritical` is
a designation somebody made; undesignated is nobody having looked.

`rto` and `rpo` are durations of whole hours and minutes: `4h`, `30m`,
`1h30m`, `0m` for none at all. One spelling, enforced at admission, and it
round-trips exactly — `4h` comes back `4h`. Anything else is a `400` naming
the spelling: `250ms` is a unit somebody guessed, and `4` is a unit nobody
wrote down.

## Designating a project

```sh
curl -X PATCH -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/projects/shop \
  -d '{"criticality": "critical", "rto": "1h", "rpo": "5m"}'
```

`200` with the project. Project `admin`, on the same endpoint as the rest of a
project's settings. Each of the three is optional and absent means untouched;
an empty string removes one.

The change is always allowed. Nothing here refuses a deployment, and no
designation is a gate — criticality is an input to alerting and to policy, and
never a bar of its own. It is a **privileged audit record carrying the
previous value**, like a data class change, because the designation decides
what wakes somebody and what a policy bundle may demand.

## Designating an environment

```sh
curl -X PATCH -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/environments/shop-production/requirements \
  -d '{"criticality": "critical", "rto": "15m"}'
```

`200` with the environment. It travels on the **owner-gated** endpoint, beside
the bar and the data class, for the reason that endpoint exists: what an
environment is worth is its owners' declaration, not the deploying team's.

**Criticality does not inherit the way a data class does, and is not capped.**
A data class is a containment property — whatever holds classified data must be
rated to hold it — so a child narrows its parent and never widens it.
Criticality is a property of *consequence*, and consequence is not contained by
anything:

- A **preview** of a critical project is not a critical function. Nobody's
  payment fails while a pull request's preview is down, and a platform that
  paged for one would have taught its operators to ignore the pager inside a
  month. A preview inherits nothing, ever.
- A **production** environment that declares nothing reads its project's
  designation, because production is where the project's function actually
  runs. That fallback is *derived*, never written back to the object, and every
  answer carrying it also carries `inherited` naming the fields it applies to.
- There is **no ceiling**. A `nonCritical` project may own the staging
  environment four teams integrate against, and a critical project may declare
  one of its environments `nonCritical`. Both are the institution's call, and
  both are recorded.

## The forward mapping — what supports this function

```sh
curl -H "authorization: Bearer $TOKEN" \
  "https://api.kitchen.example.com/api/v1/compliance/criticality?criticality=important"
```

One request, one answer:

```json
{
  "generatedAt": "2026-08-24T09:00:00Z",
  "minimum": "important",
  "functions": [
    {
      "project": "shop",
      "criticality": "critical",
      "rto": "1h",
      "rpo": "5m",
      "environments": [
        {
          "name": "shop-production",
          "type": "production",
          "criticality": "critical",
          "rto": "1h",
          "rpo": "5m",
          "inherited": ["criticality", "rto", "rpo"],
          "url": "https://shop.apps.example.com",
          "release": "shop-rel-9",
          "image": "registry.example.com/shop@sha256:...",
          "domains": ["shop.example.com"]
        }
      ],
      "claims": [
        {
          "name": "shop-db", "type": "postgres",
          "connection": "neon", "provider": "neon", "phase": "Bound",
          "dataClass": "confidential", "residency": "aws-eu-central-1"
        }
      ],
      "connections": [
        {"name": "gh", "provider": "github", "usedFor": ["source"]},
        {"name": "neon", "provider": "neon", "usedFor": ["claim shop-db"]},
        {"name": "registry", "provider": "dockerRegistry", "usedFor": ["registry"]}
      ],
      "thirdParties": ["dockerRegistry", "github", "neon"]
    }
  ],
  "undesignated": 4,
  "depth": "The graph the platform reconciles: ..."
}
```

`?criticality=` narrows to a designation **and worse**, comparing against the
highest designation anywhere under the function — the project's own or any
environment's, so a project nobody designated with one environment somebody did
is still a designated function. `?project=` narrows to one. Filtered to the
caller's visible projects like every cross-project read; an operator's answer
is the whole install.

`undesignated` is how many of the caller's projects carry no designation
anywhere. It is in the answer because a map of three critical functions means
one thing when the estate is four projects and another when it is ninety.

A function with a tolerance and no criticality is still a designated function:
dropping it would lose the RTO the alerting fires against.

## The reverse query — what breaks if this is unavailable

```sh
curl -H "authorization: Bearer $TOKEN" \
  "https://api.kitchen.example.com/api/v1/compliance/dependents?provider=neon"
```

```json
{
  "subject": {"kind": "provider", "name": "neon", "provider": "neon",
              "connections": ["neon", "neon-eu"]},
  "affected": [
    {
      "project": "shop", "environment": "shop-production", "type": "production",
      "criticality": "critical", "rto": "1h", "rpo": "5m",
      "inherited": ["criticality", "rto", "rpo"],
      "through": ["claim shop-db"]
    }
  ],
  "counts": {"critical": 1, "undesignated": 2},
  "tightestRTO": "1h",
  "depth": "..."
}
```

Exactly one of `?connection=` or `?provider=`; both together is a `400`,
because they name different subjects and answering both would say which
environments depend on *either*. A connection nothing depends on is an empty
`affected` and a `200` — "nothing breaks" is an answer, and it is a different
answer from "there is no such connection", which the empty `subject.provider`
says.

`affected` reads worst first. `through` says how each dependency runs — the
project's `source`, its `registry`, or `claim <name>` — and `tightestRTO` is
the smallest recovery objective among the affected environments: how long this
third party may be gone before the first tolerance is breached, which is the
number an incident call actually wants.

## What the traversal does not follow

Both answers carry a `depth` string saying this, because the answer is the
thing that gets exported and read six months later.

The traversal walks the graph the platform reconciles: a project's git source,
its registry, its resource claims and the Connections behind them, plus each
environment's release and its custom domains. It does **not** follow:

- **A third party the application calls at runtime.** A payment gateway
  reached from the application's own code is not a Connection and Kitchen
  cannot see it. This is the honest limit of the whole feature: the map is
  complete for everything the platform provisions and silent about everything
  the application does for itself.
- **A Connection's own upstream.** That `neon` is itself hosted somewhere is
  Neon's supply chain, not Kitchen's graph.
- **The DNS provider behind a custom domain.** The hostname is in the answer;
  who resolves it is not.
- **The platform's own dependencies.** An `oidcClient` claim names its third
  party as `platform identity provider` rather than pretending it has a
  Connection, which is honest and is as far as it goes.

## Where the tolerance is not decorative

The RTO is the threshold the `env.rto-at-risk` signal fires against: an
environment serving nothing warns at half its declared objective and goes
critical once it is past. Two environments with the same outage and different
objectives get different answers. Designating an environment `critical` also
raises every warning about it to a critical finding. Both are
[docs/OBSERVABILITY.md](../OBSERVABILITY.md), §7.

The RPO is carried, mapped and reaches the policy input, and **nothing alerts
on it**. Measuring a recovery point needs a recovery point to measure, and no
provider on this platform declares one. That is said here rather than papered
over with a rule that always passes.

## From a terminal

`kitchen criticality` answers the forward mapping and
`kitchen criticality dependents` the reverse query — see
[docs/CLI.md](../CLI.md). Designating a project or an environment has no
command of its own and goes through `kitchen api`: it is a rare, deliberate
write made by somebody who is not in a terminal at the time.
