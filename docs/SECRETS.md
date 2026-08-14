# Secret management

Kitchen's secret store is [Infisical](https://infisical.com): secrets are
defined and rotated there, and show up in the cluster as native k8s Secrets in
each project's application namespace — which is what
`Project.spec.env[].secretRef` (and, for platform plugins, a Connection's
`credentialsSecretRef`) point at. Nothing deploys with a kubectl-managed
secret.

## The pieces, and who owns them

| Piece | Owner | Why |
|---|---|---|
| Infisical **instance** | You (Cloud or self-hosted) | It brings its own Postgres and Redis; the platform's Postgres belongs to the auth service. The default is Infisical Cloud; a self-hosted instance keeps secrets on your infrastructure. |
| Infisical **secrets operator** | The Helm chart (`infisical-operator.enabled`, on by default) | The sync engine, bundled like cert-manager. Its CRDs are ordinary templates (so `helm upgrade` applies schema changes) and it ships no admission webhook — the same properties that made bundling cert-manager safe. |
| `Connection` (provider `infisical`) | You or the chart (`infisical.connection.create`) | Carries the instance URL and the machine identity. Capability: `secretStore` — the operator matches on the capability, never the provider name. |
| `InfisicalSecret` CRs | The Kitchen operator | One per project and environment type, in the app namespace. They cannot be chart templates: the namespaces they live in only exist once a Project does. |
| Synced k8s Secrets | The Infisical operator | `kitchen-secrets-production` and `kitchen-secrets-preview` in the app namespace, kept current on its resync interval. |

## Setting it up

1. In Infisical, create a **machine identity** with **universal auth** and
   grant it read access to the projects to sync.

2. Give the platform a `secretStore` Connection. Either from chart values:

   ```sh
   --set infisical.connection.create=true \
   --set infisical.connection.clientId=<machine identity client id> \
   --set infisical.connection.clientSecret=<machine identity client secret>
   # self-hosted instead of Cloud:
   --set infisical.connection.host=https://infisical.example.com
   ```

   or by hand, in the platform namespace:

   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: infisical-machine-identity
     namespace: kitchen-system
   stringData:
     clientId: <machine identity client id>
     clientSecret: <machine identity client secret>
   ---
   apiVersion: kitchen.bermos.dev/v1alpha1
   kind: Connection
   metadata:
     name: infisical
     namespace: kitchen-system
   spec:
     provider: infisical
     credentialsSecretRef: { name: infisical-machine-identity }
     config:
       host: https://app.infisical.com   # or your own instance
   ```

3. Opt a Project in:

   ```yaml
   spec:
     secrets:
       connectionRef: { name: infisical }
       projectSlug: shop            # the Infisical project
       secretsPath: /               # folder to sync, recursively (default /)
       productionEnv: prod          # Infisical env production syncs from (default)
       previewEnv: staging          # Infisical env previews sync from (default)
   ```

   The operator now keeps two `InfisicalSecret` CRs in `kitchen-shop`, and the
   Infisical operator syncs them into the Secrets `kitchen-secrets-production`
   and `kitchen-secrets-preview`. Progress is on the Project's
   `SecretStoreConnected` and `SecretsSynced` conditions.

4. Reference secrets from env vars by the **alias** `kitchen-secrets`:

   ```yaml
   spec:
     env:
       - name: DATABASE_URL
         secretRef: { name: kitchen-secrets, key: DATABASE_URL }
   ```

   The Environment reconciler resolves the alias per environment type — the
   same variable reads `prod` values in production and `staging` values in
   previews, which is how one secret *name* deliberately resolves to
   different *values* per environment. An explicit secret name is used as
   written.

## Environment mapping

Infisical environments map onto Kitchen's per project:

| Kitchen environment | Infisical environment (default) | Synced Secret |
|---|---|---|
| production | `prod` | `kitchen-secrets-production` |
| every preview | `staging` | `kitchen-secrets-preview` |

All previews share one Infisical environment on purpose: a preview is
unreleased *code*, not unreleased *credentials*, and per-PR environments in
the store would have nobody to fill them.

## Rotation

Rotate the secret in Infisical and wait out the resync interval (the Infisical
operator's default is about a minute) — the synced k8s Secret is updated in
place, and nothing in the platform redeploys. Note that a running pod picks up
the new value on its next restart: env vars are read at container start. The
machine identity itself rotates the same way — it lives in one secret in the
platform namespace, referenced (never copied) by every sync CR.

## Scope decisions worth remembering

- **The instance is not bundled (yet).** Evaluated for issue #13: self-hosted
  Infisical needs its own Postgres and Redis. Sharing the auth service's
  single-database Postgres would couple their lifecycles, and the platform
  runs no Redis at all. Bundling the full server (its `infisical-standalone`
  chart, or backend + dedicated Postgres + Redis) is a reasonable follow-up
  once the platform wants to own that footprint.
- **`InfisicalSecret` CRs are addressed as unstructured** objects, like
  cert-manager's kinds: the operator writes one small spec, and importing the
  Infisical operator's Go types would tie the build to its release cadence.
- **A missing sync engine degrades, not breaks.** With
  `infisical-operator.enabled=false` and no out-of-band operator, a Project
  asking for secrets reports `SecretsSynced=False` /
  `SecretStoreOperatorUnavailable` and retries — the same
  condition-and-requeue shape as cert-manager's webhook on first install.
