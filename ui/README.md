# Kitchen — the dashboard

The management UI from [issue #7](https://github.com/Bermos/Kitchen/issues/7):
a Vue 3 SPA that talks only to the [operator REST API](../docs/API.md). Built
with [Nuxt UI](https://ui.nuxt.com) in Vue mode — Reka UI underneath — themed
after the Kitchen design mockups (IBM Plex, dark, conditions-first).

## How it hangs together

- **Serving.** The image build compiles this app and embeds `dist/` into the
  manager binary (`internal/ui`), which serves it at `kitchen.<baseDomain>`
  next to `/api/`. Deep links fall back to `index.html`; `/assets/*` are
  content-hashed and cached forever.
- **Configuration.** The SPA boots from `/config.json` — issuer, client id,
  API URL — which the operator fills from the `Kitchen` singleton. One build
  works on every installation.
- **Login.** OIDC Authorization Code + PKCE as the public client `kitchen-ui`
  (seeded by the chart). The token request carries
  `resource=<api url>` so the access token is a JWT the operator can validate
  (docs/AUTH.md).
- **Staying signed in.** The sign-in asks for `offline_access`, and the
  refresh token that comes back is traded for a new access token a minute
  before the old one expires — so a tab left open does not reload through the
  identity provider once an hour. A 401 renews and retries the request once
  before anyone is sent back to the login. Refresh tokens are single-use, so
  renewal is serialised across tabs with a Web Lock; a replayed one costs the
  whole session by design. The session lives in `localStorage`, which is what
  makes it one session per browser rather than one per tab — the reasoning,
  and what it costs, is in [docs/AUTH.md](../docs/AUTH.md).
- **How a screen is put together.** [docs/UI.md](../docs/UI.md) is the design
  guide: one page width, one rhythm, one header (`PageHeader`), one heading
  scale, one table, and the rule that decides which of the two dashboards a
  screen belongs to. `src/lib/design.test.ts` enforces the half of it a machine
  can hold, and runs in `npm test`.
- **Status display.** Phases are the coarse summary; the views read
  `status.conditions` for detail (docs/CRDS.md), and the Operator toggle in
  the top bar surfaces the full condition tables on every object — behind
  `<OperatorOnly>`, which is where every Kubernetes noun on a developer screen
  lives.
- **"Live" logs.** The log endpoints stream as Server-Sent Events when asked to,
  and the viewers fall back to re-running the bounded query every few seconds
  where a stream cannot be established. The request list tails the same way.
- **Navigation.** ⌘K opens a palette over everything the API lists — projects,
  environments, builds, domains, pages. The sidebar carries live counts from
  the same collections.
- **Small screens.** The shell is responsive at one breakpoint: below `lg`
  (1024px) the sidebar leaves the flow and becomes a drawer over the page —
  same element, moved by a media query, so it keeps its scroll position — and
  the header grows a hamburger while the palette, the mode toggle and the
  account button collapse to their icons. It is `inert` while it is off the
  screen, so nothing in it is reachable by keyboard or screen reader, and it
  closes on Escape, on the backdrop, and on every navigation. Views follow two
  rules rather than a breakpoint each: a wide table sits in an
  `overflow-x-auto` box **and** carries a `min-w-[…]` floor, because `w-full`
  alone makes the browser crush the name column to one character per line
  instead of scrolling; and anything holding such a box inside a grid or a flex
  row needs `min-w-0`, since those items refuse to shrink below their content.
- **Requests.** The environment page's Signals section reads the four request
  endpoints: golden-signal tiles, traffic/error/latency charts marked with the
  activity feed's deploys, a route table whose selected row filters the header,
  the charts and the list, and the requests themselves — each one click from
  the log lines the environment wrote around it. An environment nothing
  publishes on the shared Gateway says exactly that and leads with what is real
  for any workload instead: what it wrote, what it used against its limits, and
  how often it restarted.
- **Observability.** The query bar takes a real ClickHouse boolean expression
  over the logs table (`GET /api/v1/logs?where=…`, read-only and capped
  server-side), and a refused expression shows ClickHouse's own diagnostic.
  The build and environment log viewers jump there with the scope prefilled.

## Developing

```sh
npm install
npm run dev        # http://localhost:5173, proxies /api to localhost:8082
```

Against a real installation, set the dev-time fallbacks the operator would
otherwise serve in `/config.json`:

```sh
VITE_ISSUER=https://auth.apps.example.com \
VITE_API_URL=https://kitchen.apps.example.com \
VITE_API_PROXY=https://kitchen.apps.example.com npm run dev
```

(and add `http://localhost:5173/auth/callback` to `auth.ui.redirectURIs` in
the chart's values so the issuer sends you back).

`npm run build` + `npm run typecheck` + `npm test` is what CI runs;
`make ui-build` at the repo root stages the build for embedding.
