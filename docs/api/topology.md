# Kitchen — The architecture

What the projects on this platform are made of, what each one depends on, and
the traffic the platform observed flowing along it. The dashboard's Fleet →
Architecture screen draws it; `kitchen topology` answers it as data.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to. The
design is the "Topology" section of
[the service-topology spike](../spikes/service-topology-2026-09.md).

## The declared graph

`GET /topology` answers `{nodes, edges}`, read on every request off the
objects that already point at each other. Nothing stores the graph, so nothing
has to be kept in step with it, and there is no write beside it: the graph
changes when an environment, a claim, an offering or a domain does, through the
routes those objects already have.

```json
{
  "nodes": [
    {"id": "environment/shop-production", "kind": "environment", "name": "shop-production",
     "project": "shop", "type": "production", "phase": "Live",
     "url": "https://shop.apps.example.com", "processes": [{"name": "web", "type": "web"}]},
    {"id": "offering/pricing/pricing-api", "kind": "offering", "name": "pricing-api",
     "project": "pricing", "type": "http", "detail": "open"},
    {"id": "resource/shop-db", "kind": "resource", "name": "shop-db", "project": "shop",
     "type": "postgres", "phase": "Bound"}
  ],
  "edges": [
    {"id": "consumes:environment/shop-production->offering/pricing/pricing-api", "kind": "consumes",
     "from": "environment/shop-production", "to": "offering/pricing/pricing-api"},
    {"id": "uses:environment/shop-production->resource/shop-db", "kind": "uses",
     "from": "environment/shop-production", "to": "resource/shop-db"}
  ]
}
```

| Node `kind` | What it is | `type` |
|---|---|---|
| `project` | A project drawn as a whole: one the caller holds no role on, or one with nothing deployed yet | — |
| `environment` | An environment, with its phase, address and processes | `production`, `stage` or `preview` |
| `offering` | Something a project offers the others (`spec.offers`) | its protocol |
| `resource` | A resource claim of any type but `service` | the claim type |
| `provider` | The Connection a resource was provisioned through | the connection's provider |
| `domain` | A custom domain | — |
| `internet` | Everybody off the platform, in front of every published address | — |

**Every edge points from what would break to what it would break on**, which is
also the way a request travels:

| Edge `kind` | From → to |
|---|---|
| `routes` | `internet` → a published environment or a domain; a domain → its environment |
| `consumes` | An environment → the offering its project binds — one edge per environment, in the state the binding is in for that class of environment |
| `serves` | An offering → the environment answering it |
| `uses` | An environment → each of its project's resources |
| `providedBy` | A resource → its connection |

An edge that is not in effect carries `state` — `PendingApproval` for a
binding the providing project has not answered, `unresolved` for a class of
environment the binding reaches nothing for, with the claim's own words in
`reason` — or the claim's phase.

`?project=` narrows the answer to one project and everything one edge away
from it: the other ends of its bindings, the connections behind its resources,
the internet in front of it.

### What a member is told about somebody else's project

The graph is narrowed to the caller's own projects. A project they hold no
role on appears only where one of theirs already names it — the provider of an
offering they bind, or a consumer of one they make — and then as a `project`
node with `foreign: true` and nothing but its name. That is what the offering
catalogue and a project's Offerings pane answer already; nothing about its
environments, resources or addresses is.

## The observed half

`GET /topology/traffic` lays the flow collector's edges
([Traffic](telemetry.md#traffic)) onto the same nodes, over a window
(`?since=`/`?until=`, defaulting to the last hour, answered back as `since` and
`until`; `?project=` narrows as `/traffic` does).

```json
{
  "since": "2026-09-25T09:00:00Z",
  "until": "2026-09-25T10:00:00Z",
  "nodes": [{"id": "project/blog", "kind": "project", "name": "blog", "project": "blog", "foreign": true}],
  "edges": [
    {"from": "environment/shop-production", "to": "environment/pricing-production",
     "protocol": "HTTP", "flows": 3600, "rps": 1, "errors": 0, "drops": 0, "p95Ms": 41,
     "status": "declared",
     "along": ["consumes:environment/shop-production->offering/pricing/pricing-api",
               "serves:offering/pricing/pricing-api->environment/pricing-production"]},
    {"from": "project/blog", "to": "environment/shop-production",
     "protocol": "HTTP", "flows": 12, "rps": 0.003, "errors": 2, "drops": 0, "p95Ms": 9,
     "status": "undeclared"}
  ]
}
```

A flow names workloads, and the platform named those workloads after what
materialized them, so attribution is the controller's naming read backwards:
an environment's own workload and each process's are the environment; a
workload a claim's provider runs under the claim's instance name is that
resource; anything else in a project's namespace is the project as a whole; an
endpoint the collector could not name is `internet`, one it resolved by name
off the platform is an `external` node, and the platform's own components —
the gateway, the forward-auth gate, the scale-to-zero interceptor — are one
`platform` node. Pairs that land on the same two nodes are summed; their `p95Ms`
is the slowest of theirs, an upper bound rather than a percentile of the
merged traffic. An environment talking to itself is not an edge.

`status` is how the pair relates to the declared graph:

| `status` | Meaning |
|---|---|
| `declared` | It runs along declared edges, listed in order in `along` — directly, or through the domain or offering between the two ends |
| `undeclared` | One project's workload calls another's with nothing declaring it. Today this works; under default-deny between applications it is exactly what breaks |
| `platform` | It touches the platform's own components, which no binding governs |
| `external` | It leaves the platform, or arrives from off it without a published address explaining it |

`nodes` carries only what the declared graph does not have: `platform`,
`external` hosts, and whole projects the flows reached. A member sees a caller
from a project they hold no role on as that project, by name, not as one of
its environments.

A binding in effect that carried nothing over the window is the spike's *dead
edge*; the dashboard lists them as quiet bindings, computed from the two
answers rather than served.
