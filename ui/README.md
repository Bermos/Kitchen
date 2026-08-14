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
  (docs/AUTH.md). The access token lives in `sessionStorage`; a 401 routes
  back through the login, which is a redirect and no interaction while the
  identity provider still has a session.
- **Status display.** Phases are the coarse summary; the views read
  `status.conditions` for detail (docs/CRDS.md), and the Operator toggle in
  the top bar surfaces the full condition tables on every object.
- **"Live" logs.** The log endpoints are bounded queries, so live is honest
  polling while a build runs or an environment deploys; streaming is an open
  item on the API.
- **Navigation.** ⌘K opens a palette over everything the API lists — projects,
  environments, builds, domains, pages. The sidebar carries live counts from
  the same collections.
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
