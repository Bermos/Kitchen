{{/*
Chart name, overridable.
*/}}
{{- define "kitchen.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified resource name.
*/}}
{{- define "kitchen.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "kitchen.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Labels on every object the chart owns.
*/}}
{{- define "kitchen.labels" -}}
helm.sh/chart: {{ include "kitchen.chart" . }}
{{ include "kitchen.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kitchen
{{- end }}

{{/*
Selector labels. `control-plane: controller-manager` is kept for parity with
the kustomize deployment, so existing tooling keeps matching the manager pods.
*/}}
{{- define "kitchen.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{- define "kitchen.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (printf "%s-controller-manager" (include "kitchen.fullname" .)) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Container image reference, digest taking precedence over tag.
*/}}
{{- define "kitchen.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
The scheme every generated URL is reached over, following kitchen.tls.mode as
the operator does. In mode "none" the shared Gateway gets an HTTP listener and
nothing else, so publishing https there names a scheme nothing serves.
*/}}
{{- define "kitchen.scheme" -}}
{{- if eq .Values.kitchen.tls.mode "none" }}http{{ else }}https{{ end }}
{{- end }}

{{/*
Public base URL of the operator API, mirroring the operator's own default of
kitchen.<baseDomain> under the platform's scheme.
*/}}
{{- define "kitchen.apiExternalURL" -}}
{{- if .Values.kitchen.api.externalURL }}
{{- .Values.kitchen.api.externalURL | trimSuffix "/" }}
{{- else if .Values.kitchen.baseDomain }}
{{- printf "%s://kitchen.%s" (include "kitchen.scheme" .) .Values.kitchen.baseDomain }}
{{- end }}
{{- end }}

{{/*
Hostname derived from the API's public base URL, without scheme or port.
*/}}
{{- define "kitchen.externalHost" -}}
{{- $url := include "kitchen.apiExternalURL" . }}
{{- $hostPort := $url | replace "https://" "" | replace "http://" "" | splitList "/" | first }}
{{- $hostPort | splitList ":" | first }}
{{- end }}

{{/*
Hostname the git webhook receiver is published under.
*/}}
{{- define "kitchen.webhookHost" -}}
{{- if .Values.webhookReceiver.route.host }}
{{- .Values.webhookReceiver.route.host }}
{{- else }}
{{- include "kitchen.externalHost" . }}
{{- end }}
{{- end }}

{{/*
Hostname the REST API is published under. It shares the receiver's name by
default — one public name for the operator, split by path — which is also what
`spec.api.externalURL` describes.
*/}}
{{- define "kitchen.apiHost" -}}
{{- if .Values.api.route.host }}
{{- .Values.api.route.host }}
{{- else }}
{{- include "kitchen.externalHost" . }}
{{- end }}
{{- end }}

{{/*
ClickHouse: names, selector labels and connection details. The same secret
shape is produced whether the chart runs ClickHouse or points at an external
one, so consumers never have to care which.
*/}}
{{- define "kitchen.clickhouseFullname" -}}
{{- printf "%s-clickhouse" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.clickhouseSecretName" -}}
{{- printf "%s-clickhouse" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.clickhouseSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}-clickhouse
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kitchen.clickhouseHost" -}}
{{- if .Values.clickhouse.enabled }}
{{- printf "%s.%s.svc" (include "kitchen.clickhouseFullname" .) .Release.Namespace }}
{{- else }}
{{- .Values.clickhouse.external.host }}
{{- end }}
{{- end }}

{{- define "kitchen.clickhouseHTTPPort" -}}
{{- if .Values.clickhouse.enabled }}
{{- .Values.clickhouse.service.httpPort }}
{{- else }}
{{- .Values.clickhouse.external.httpPort }}
{{- end }}
{{- end }}

{{- define "kitchen.clickhouseNativePort" -}}
{{- if .Values.clickhouse.enabled }}
{{- .Values.clickhouse.service.nativePort }}
{{- else }}
{{- .Values.clickhouse.external.nativePort }}
{{- end }}
{{- end }}

{{/*
The password: explicit if given, otherwise the one already in the cluster, and
only generated when there is neither. Regenerating it on every upgrade would
lock the collectors out of their own store.
*/}}
{{- define "kitchen.clickhousePassword" -}}
{{- if .Values.clickhouse.auth.password }}
{{- .Values.clickhouse.auth.password }}
{{- else }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kitchen.clickhouseSecretName" .) }}
{{- if and $existing $existing.data (index (default dict $existing.data) "password") }}
{{- index $existing.data "password" | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Postgres: the auth service's system of record. Same shape as the ClickHouse
helpers — one secret describes the connection whether the chart runs Postgres
or points at an existing one.
*/}}
{{- define "kitchen.postgresFullname" -}}
{{- printf "%s-postgres" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.postgresSecretName" -}}
{{- printf "%s-postgres" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.postgresSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}-postgres
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kitchen.postgresHost" -}}
{{- if .Values.postgres.enabled }}
{{- printf "%s.%s.svc" (include "kitchen.postgresFullname" .) .Release.Namespace }}
{{- else }}
{{- .Values.postgres.external.host }}
{{- end }}
{{- end }}

{{- define "kitchen.postgresPort" -}}
{{- if .Values.postgres.enabled }}
{{- .Values.postgres.service.port }}
{{- else }}
{{- .Values.postgres.external.port }}
{{- end }}
{{- end }}

{{- define "kitchen.postgresPassword" -}}
{{- if .Values.postgres.auth.password }}
{{- .Values.postgres.auth.password }}
{{- else }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kitchen.postgresSecretName" .) }}
{{- if and $existing $existing.data (index (default dict $existing.data) "password") }}
{{- index $existing.data "password" | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Auth service: names, the hostname it is published under and the two generated
credentials. Both are read back from the cluster on upgrade — regenerating the
signing secret would invalidate every session, and regenerating the service key
would lock the operator out of client registration.
*/}}
{{- define "kitchen.authFullname" -}}
{{- printf "%s-auth" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.authSecretName" -}}
{{- printf "%s-auth" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.authSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}-auth
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kitchen.authHost" -}}
{{- if .Values.auth.route.host }}
{{- .Values.auth.route.host }}
{{- else if .Values.kitchen.baseDomain }}
{{- printf "auth.%s" .Values.kitchen.baseDomain }}
{{- end }}
{{- end }}

{{/*
The OIDC issuer. Everything that speaks to the identity provider — the UI, the
operator API, deployed apps — starts from this URL, so its scheme has to be one
the Gateway actually serves: discovery, the JWKS fetch and every redirect are
built from it.
*/}}
{{- define "kitchen.authIssuer" -}}
{{- with include "kitchen.authHost" . }}
{{- printf "%s://%s" (include "kitchen.scheme" $) . }}
{{- end }}
{{- end }}

{{/*
Where the identity provider may send someone back to after they sign in to the
Kitchen UI. The UI is served from the operator's public name, so its callback
follows from the same URL the API answers on.
*/}}
{{- define "kitchen.authUIRedirectURIs" -}}
{{- if .Values.auth.ui.redirectURIs }}
{{- join "," .Values.auth.ui.redirectURIs }}
{{- else }}
{{- with include "kitchen.apiExternalURL" . }}
{{- printf "%s/auth/callback" . }}
{{- end }}
{{- end }}
{{- end }}

{{- define "kitchen.authImage" -}}
{{- if .Values.auth.image.digest }}
{{- printf "%s@%s" .Values.auth.image.repository .Values.auth.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.auth.image.repository (default .Chart.AppVersion .Values.auth.image.tag) }}
{{- end }}
{{- end }}

{{- define "kitchen.authSecretValue" -}}
{{- $key := index . 1 }}
{{- $length := index . 2 }}
{{- $root := index . 0 }}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace (include "kitchen.authSecretName" $root) }}
{{- if and $existing $existing.data (index (default dict $existing.data) $key) }}
{{- index $existing.data $key | b64dec }}
{{- else }}
{{- randAlphaNum (int $length) }}
{{- end }}
{{- end }}

{{- define "kitchen.authSigningSecret" -}}
{{- default (include "kitchen.authSecretValue" (list . "secret" 48)) .Values.auth.secret }}
{{- end }}

{{/*
The operator's credential for dynamic client registration. 64 characters is the
minimum the auth service's api-key plugin accepts.
*/}}
{{- define "kitchen.authServiceKey" -}}
{{- default (include "kitchen.authSecretValue" (list . "serviceKey" 64)) .Values.auth.serviceKey }}
{{- end }}

{{- define "kitchen.authBootstrapToken" -}}
{{- default (include "kitchen.authSecretValue" (list . "bootstrapToken" 32)) .Values.auth.bootstrap.token }}
{{- end }}

{{/*
Where the operator and the preview gate reach the identity provider from
inside the cluster. It is the service when the chart runs one, and the public
issuer otherwise — an external identity provider is reached the same way from
everywhere.
*/}}
{{- define "kitchen.authInternalURL" -}}
{{- if .Values.auth.enabled }}
{{- printf "http://%s.%s.svc:%v" (include "kitchen.authFullname" .) .Release.Namespace .Values.auth.service.port }}
{{- else }}
{{- include "kitchen.authIssuer" . }}
{{- end }}
{{- end }}

{{/*
The forward-auth gate protected previews are served through. It is deployed by
the operator, not by the chart: it cannot start before an OAuth client has
been registered for it, and only the operator can do that. What the chart
contributes is the switch, the hostname and the image.
*/}}
{{- define "kitchen.previewGateEnabled" -}}
{{- if and .Values.auth.enabled .Values.previewGate.enabled }}true{{ end }}
{{- end }}

{{- define "kitchen.previewGateHost" -}}
{{- if .Values.previewGate.host }}
{{- .Values.previewGate.host }}
{{- else if .Values.kitchen.baseDomain }}
{{- printf "previews.%s" .Values.kitchen.baseDomain }}
{{- end }}
{{- end }}

{{/*
Whether there is a telemetry store to talk to at all — one the chart runs, or
one it was pointed at. Both cases produce the same connection secret.
*/}}
{{- define "kitchen.hasTelemetryStore" -}}
{{- if or .Values.clickhouse.enabled .Values.clickhouse.external.host }}true{{ end }}
{{- end }}

{{/*
The log collector. It is only rendered when there is somewhere to ship to:
`clickhouse.acknowledgeNoStore` is an explicit choice to install without
telemetry, and a DaemonSet crash-looping against a store that does not exist
is not a useful way to report that.
*/}}
{{- define "kitchen.logsEnabled" -}}
{{- if and .Values.logs.enabled (eq (include "kitchen.hasTelemetryStore" .) "true") }}true{{ end }}
{{- end }}

{{- define "kitchen.logsFullname" -}}
{{- printf "%s-logs" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.logsSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}-logs
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kitchen.logsServiceAccountName" -}}
{{- if .Values.logs.serviceAccount.create }}
{{- default (include "kitchen.logsFullname" .) .Values.logs.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.logs.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The shared Gateway listener routes attach to. With edge TLS on, port 80 carries
nothing but the operator's redirect to HTTPS, so anything that actually serves
has to name the HTTPS listener: a route bound to both would win on hostname
specificity and answer over cleartext. In the other TLS modes there is no HTTPS
listener, and port 80 is where the platform answers.
*/}}
{{- define "kitchen.gatewaySection" -}}
{{- if eq .Values.kitchen.tls.mode "acme" }}https{{ else }}http{{ end }}
{{- end }}

{{/*
Whether the platform is configured to obtain its own wildcard certificate. The
chart never creates the Certificate — the operator does, once cert-manager is
serving — so this only reports whether it has been told how.
*/}}
{{- define "kitchen.acmeConfigured" -}}
{{- $acme := .Values.kitchen.tls.acme }}
{{- if and (eq .Values.kitchen.tls.mode "acme") $acme.email $acme.dns01.cloudflare.apiTokenSecretName }}true{{ end }}
{{- end }}

{{/*
Guard rails. The operator resolves the platform namespace from a compiled-in
constant, so a chart installed elsewhere would reconcile into a namespace it
does not run in.
*/}}
{{- define "kitchen.validate" -}}
{{- if and .Values.namespaceCheck (ne .Release.Namespace "kitchen-system") }}
{{- fail (printf "Kitchen must be installed into the kitchen-system namespace (got %q). Re-run with `--namespace kitchen-system --create-namespace`, or set namespaceCheck=false if you have changed the operator's PlatformNamespace constant." .Release.Namespace) }}
{{- end }}
{{- if and .Values.kitchen.create (not .Values.kitchen.baseDomain) }}
{{- fail "kitchen.baseDomain is required when kitchen.create is true: generated URLs are <slug>.<baseDomain>. Set it, or set kitchen.create=false to manage the Kitchen object yourself." }}
{{- end }}
{{- if not (has .Values.kitchen.tls.mode (list "acme" "cloudflared" "none")) }}
{{- fail (printf "kitchen.tls.mode must be one of acme, cloudflared, none (got %q)" .Values.kitchen.tls.mode) }}
{{- end }}
{{- if not (has .Values.kitchen.builds.defaultStrategy (list "auto" "dockerfile" "buildpacks")) }}
{{- fail (printf "kitchen.builds.defaultStrategy must be one of auto, dockerfile, buildpacks (got %q)" .Values.kitchen.builds.defaultStrategy) }}
{{- end }}
{{- $acme := .Values.kitchen.tls.acme }}
{{- if and (ne .Values.kitchen.tls.mode "acme") (or $acme.email $acme.dns01.cloudflare.apiTokenSecretName) }}
{{- fail (printf "kitchen.tls.acme is configured but kitchen.tls.mode is %q: the wildcard certificate is only read by the Gateway's HTTPS listener, which exists in acme mode alone. Set mode=acme, or clear the acme block." .Values.kitchen.tls.mode) }}
{{- end }}
{{- if and $acme.email (not $acme.dns01.cloudflare.apiTokenSecretName) }}
{{- fail "kitchen.tls.acme.email is set without kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName: every generated URL is a subdomain, so the platform needs a wildcard certificate, and ACME issues wildcards over DNS-01 only." }}
{{- end }}
{{- if and $acme.dns01.cloudflare.apiTokenSecretName (not $acme.email) }}
{{- fail "kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName is set without kitchen.tls.acme.email: the CA registers the account against a contact address." }}
{{- end }}
{{- if and .Values.kitchen.create .Values.kitchen.ingress.cloudflared.enabled (not .Values.kitchen.ingress.cloudflared.tunnelSecretName) }}
{{- fail "kitchen.ingress.cloudflared.tunnelSecretName is required when cloudflared is enabled: create a secret holding the tunnel token under the key `token` first." }}
{{- end }}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.leaderElection) }}
{{- fail "leaderElection must stay enabled when replicaCount > 1, otherwise every replica reconciles concurrently." }}
{{- end }}
{{- if and .Values.clickhouse.enabled .Values.clickhouse.external.host }}
{{- fail "clickhouse.enabled and clickhouse.external.host are mutually exclusive: either the chart runs ClickHouse or it points at yours." }}
{{- end }}
{{- if and .Values.clickhouse.external.host (not .Values.clickhouse.auth.password) }}
{{- fail "clickhouse.auth.password is required with an external ClickHouse: the chart would otherwise hand the collectors a password it invented." }}
{{- end }}
{{- if and (not .Values.clickhouse.enabled) (not .Values.clickhouse.external.host) (not .Values.clickhouse.acknowledgeNoStore) }}
{{- fail "no telemetry store: enable clickhouse.enabled, set clickhouse.external.host, or set clickhouse.acknowledgeNoStore=true to install without one (logs, metrics and traces then have nowhere to land)." }}
{{- end }}
{{- if and .Values.logs.enabled (lt (int .Values.logs.batch.maxEvents) 1) }}
{{- fail "logs.batch.maxEvents must be at least 1: a batch of nothing never ships." }}
{{- end }}
{{- if and .Values.logs.enabled (lt (int .Values.logs.buffer.maxEvents) (int .Values.logs.batch.maxEvents)) }}
{{- fail "logs.buffer.maxEvents must be at least logs.batch.maxEvents, otherwise the collector drops events it could never batch up." }}
{{- end }}
{{- if and .Values.postgres.enabled .Values.postgres.external.host }}
{{- fail "postgres.enabled and postgres.external.host are mutually exclusive: either the chart runs Postgres or it points at yours." }}
{{- end }}
{{- if and .Values.postgres.external.host (not .Values.postgres.auth.password) }}
{{- fail "postgres.auth.password is required with an external Postgres: the chart would otherwise hand the auth service a password it invented." }}
{{- end }}
{{- if .Values.auth.enabled }}
{{- if and (not .Values.postgres.enabled) (not .Values.postgres.external.host) }}
{{- fail "auth.enabled needs a database: enable postgres.enabled or set postgres.external.host. The identity provider keeps accounts, sessions and OAuth clients in Postgres." }}
{{- end }}
{{- if and .Values.auth.route.enabled (not (include "kitchen.authHost" .)) }}
{{- fail "auth.route.enabled needs a hostname: set kitchen.baseDomain or auth.route.host. The hostname is also the OIDC issuer, so clients cannot be configured without it." }}
{{- end }}
{{- if and (not .Values.auth.route.enabled) (not (include "kitchen.authHost" .)) }}
{{- fail "auth.enabled needs a hostname even without a route: set kitchen.baseDomain or auth.route.host. The identity provider signs tokens with its issuer URL." }}
{{- end }}
{{- if and .Values.auth.github.clientId (not (or .Values.auth.github.clientSecret .Values.auth.github.existingSecret)) }}
{{- fail "auth.github.clientId needs a secret: set auth.github.clientSecret or auth.github.existingSecret." }}
{{- end }}
{{- if and .Values.auth.github.existingSecret (not .Values.auth.github.clientId) }}
{{- fail "auth.github.existingSecret needs auth.github.clientId as well: the client id is not read from the secret." }}
{{- end }}
{{- end }}
{{- if .Values.previewGate.enabled }}
{{- if not .Values.auth.enabled }}
{{- fail "previewGate.enabled needs auth.enabled: the gate signs visitors in against the platform's identity provider. Set previewGate.enabled=false to serve previews without one — Projects then have to set spec.previews.protected=false as well." }}
{{- end }}
{{- if not (include "kitchen.previewGateHost" .) }}
{{- fail "previewGate.enabled needs a hostname: set kitchen.baseDomain or previewGate.host. It is the redirect URI every protected preview's login comes back to." }}
{{- end }}
{{- if lt (int .Values.previewGate.replicas) 1 }}
{{- fail "previewGate.replicas must be at least 1: protected previews route through the gate, so none running means none reachable." }}
{{- end }}
{{- end }}
{{- end }}
