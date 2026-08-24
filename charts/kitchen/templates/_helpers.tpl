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
The self-update job's ServiceAccount. Separate from the manager's, because it
is bound to cluster-admin and that grant should be one obvious object rather
than an extra rule on the account everything else already runs as.
*/}}
{{- define "kitchen.selfUpdateServiceAccountName" -}}
{{- default (printf "%s-self-update" (include "kitchen.fullname" .)) .Values.selfUpdate.serviceAccountName }}
{{- end }}

{{/*
The KEDA install job's ServiceAccount. Separate from the manager's for the same
reason the self-update account is: it is bound to cluster-admin, and that grant
should be one obvious object that goes away with the feature.
*/}}
{{- define "kitchen.kedaInstallServiceAccountName" -}}
{{- default (printf "%s-keda-install" (include "kitchen.fullname" .)) .Values.scaleToZero.install.serviceAccountName }}
{{- end }}

{{/*
The preview gate's ServiceAccount. Separate from the manager's for the same
reason the self-update account is: it is the identity of a different workload,
and the whole point of it is that the grant is small and visible — get, list
and watch on projects and kitchens, and nothing else at all.
*/}}
{{- define "kitchen.previewGateServiceAccountName" -}}
{{- printf "%s-preview-gate" (include "kitchen.fullname" .) }}
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

{{/*
The trace receiver's Service, and the endpoint applications are told to export
to. The endpoint is written into the Kitchen object rather than left to the
CRD's default because every name this chart generates carries the release's,
and the operator has no way to know what that was.
*/}}
{{- define "kitchen.otlpServiceName" -}}
{{- printf "%s-otlp" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.otlpEndpoint" -}}
{{- if .Values.kitchen.observability.traces.endpoint }}
{{- .Values.kitchen.observability.traces.endpoint }}
{{- else }}
{{- printf "http://%s.%s.svc.cluster.local:%v" (include "kitchen.otlpServiceName" .) .Release.Namespace .Values.kitchen.observability.traces.port }}
{{- end }}
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
The bundled image registry: a name, the hostname it is published on, and the
credential builds push with. The chart runs the registry, its Service and its
volume; the operator publishes the route and seeds the Connection, because
both need the shared Gateway it creates.
*/}}
{{- define "kitchen.registryFullname" -}}
{{- printf "%s-registry" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.registrySecretName" -}}
{{- printf "%s-registry" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.registrySelectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}-registry
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kitchen.registryImage" -}}
{{- printf "%s:%s" .Values.registry.image.repository .Values.registry.image.tag }}
{{- end }}

{{/*
Where the registry is published. It is a hostname on the wildcard certificate
and nothing else: the node pulls over it, so it has to be a name the node can
resolve and a certificate the node already trusts.
*/}}
{{- define "kitchen.registryHost" -}}
{{- if .Values.registry.host }}
{{- .Values.registry.host }}
{{- else if .Values.kitchen.baseDomain }}
{{- printf "registry.%s" .Values.kitchen.baseDomain }}
{{- end }}
{{- end }}

{{/*
The registry's password. Evaluate this ONCE per render and pass the result
around: with no explicit value and nothing to look up it ends in
`randAlphaNum`, so a second call returns a different password than the first.
*/}}
{{- define "kitchen.registryPassword" -}}
{{- if .Values.registry.auth.password }}
{{- .Values.registry.auth.password }}
{{- else }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kitchen.registrySecretName" .) }}
{{- if and $existing $existing.data (index (default dict $existing.data) "password") }}
{{- index $existing.data "password" | b64dec }}
{{- else }}
{{- randAlphaNum 32 }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The htpasswd line zot authenticates against — bcrypt, which is the only hash
it reads. Hashing is salted, so a fresh hash every render would roll the
Secret and restart the registry on every upgrade; the stored line is reused
whenever it still describes the same username and password.

Takes the password in a dict (`ctx`, `password`) rather than deriving it. A
second call to `kitchen.registryPassword` would hash a *different* password
than the one the Secret publishes, leaving the registry to reject the only
credential the platform has. That fails on a first install alone — on upgrade
the lookup makes both calls agree, which is what hid it in 0.8.0.
*/}}
{{- define "kitchen.registryHtpasswd" -}}
{{- $ctx := .ctx }}
{{- $password := .password }}
{{- $username := $ctx.Values.registry.auth.username }}
{{- $existing := lookup "v1" "Secret" $ctx.Release.Namespace (include "kitchen.registrySecretName" $ctx) }}
{{- $data := default dict (default dict $existing).data }}
{{- $stored := "" }}
{{- if index $data "htpasswd" }}{{- $stored = index $data "htpasswd" | b64dec }}{{- end }}
{{- $sameUser := and (index $data "username") (eq (index $data "username" | b64dec) $username) }}
{{- $samePassword := and (index $data "password") (eq (index $data "password" | b64dec) $password) }}
{{- if and $stored $sameUser $samePassword }}
{{- $stored }}
{{- else }}
{{- htpasswd $username $password }}
{{- end }}
{{- end }}

{{/*
Whether the platform actually gets a registry. It needs somewhere to be
published and a certificate the node trusts to be published under, so
`tls.mode: none` renders no registry at all rather than one nothing can pull
from — the operator says the same thing in its RegistryReady condition.
*/}}
{{- define "kitchen.registryEnabled" -}}
{{- if and .Values.registry.enabled (ne .Values.kitchen.tls.mode "none") }}true{{ end }}
{{- end }}

{{/*
Whether there is a telemetry store to talk to at all — one the chart runs, or
one it was pointed at. Both cases produce the same connection secret.
*/}}
{{- define "kitchen.hasTelemetryStore" -}}
{{- if or .Values.clickhouse.enabled .Values.clickhouse.external.host }}true{{ end }}
{{- end }}

{{/*
The telemetry agent. It is only rendered when there is somewhere to ship to:
`clickhouse.acknowledgeNoStore` is an explicit choice to install without
telemetry, and a DaemonSet crash-looping against a store that does not exist
is not a useful way to report that.
*/}}
{{- define "kitchen.collectorEnabled" -}}
{{- if and .Values.collector.enabled (eq (include "kitchen.hasTelemetryStore" .) "true") }}true{{ end }}
{{- end }}

{{- define "kitchen.collectorFullname" -}}
{{- printf "%s-collector" (include "kitchen.fullname" .) }}
{{- end }}

{{- define "kitchen.collectorSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kitchen.name" . }}-collector
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kitchen.collectorServiceAccountName" -}}
{{- if .Values.collector.serviceAccount.create }}
{{- default (include "kitchen.collectorFullname" .) .Values.collector.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.collector.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Where the collector keeps its file offsets inside the container, and the port
its health endpoint answers on. Neither is configurable: the first is the mount
point of a hostPath whose node-side path is, and the second is reached only by
the kubelet's probes.
*/}}
{{- define "kitchen.collectorStateDir" -}}
/var/lib/otelcol
{{- end }}

{{- define "kitchen.collectorHealthPort" -}}
13133
{{- end }}

{{/*
`kitchen.source`: what produced a signal, so the dashboard can ask for one kind
without knowing which labels imply it. k8s_attributes cannot express a
conditional, which is why this one attribute is OTTL.

The chain reads as an if/else-if: the namespace decides first, then a build
label, then any project or environment, and everything else is the cluster's.
That last name matters — the agent tails every container on the
node, so "no Kitchen label" is not "a platform component", it is Cilium,
cert-manager, a CSI sidecar, whatever else the cluster runs. Calling those
`platform` made the platform's own diagnostic facet return the entire cluster,
and other people's warnings read as Kitchen faults.

Every statement is guarded on the attribute still being unset, which is what
makes the chain an if/else-if — and also what leaves alone a value the sender
supplied: the operator stamps `kitchen.source` on the usage metrics it exports
over OTLP, and those describe a pod other than the one that sent them.

Paths are written `resource.attributes[...]` even though the statements
declare `context: resource`. An unqualified path still works, but the parser
rewrites it and logs what it rewrote on every start.
*/}}
{{- define "kitchen.collectorSourceStatements" -}}
- set(resource.attributes["kitchen.source"], "platform") where resource.attributes["kitchen.source"] == nil and resource.attributes["k8s.namespace.name"] == {{ .Release.Namespace | quote }}
- set(resource.attributes["kitchen.source"], "build") where resource.attributes["kitchen.source"] == nil and resource.attributes["kitchen.build"] != nil
- set(resource.attributes["kitchen.source"], "runtime") where resource.attributes["kitchen.source"] == nil and (resource.attributes["kitchen.project"] != nil or resource.attributes["deployment.environment.name"] != nil)
- set(resource.attributes["kitchen.source"], "cluster") where resource.attributes["kitchen.source"] == nil
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
The singleton's `access` block: who owns the platform.

Three answers, in order, and the order is the whole of the design.

1. `kitchen.access.operators`, when it is set. The installation has named its
   operators in the file it is declared in, and that is the one answer an
   installation federated to an issuer of its own can give — there is no
   account directory to seed from on a Keycloak, so without this nobody would
   ever hold the operator role.
2. The list that is already live, when this is an upgrade that re-applies the
   singleton. `kitchen.applyOnUpgrade=true` makes the object a hook with
   `hook-delete-policy: before-hook-creation`, which *deletes and recreates*
   it — and an absent operator list is how the reconciler is told that nobody
   has ever said who the operators are, so rendering nothing here would seed
   the list straight back from every account the identity provider holds.
   Somebody who narrowed the platform to two operators would get all fourteen
   back on the next upgrade, silently, and a deliberate `operators: []` would
   be destroyed outright. So the live list is read and re-emitted, empty
   included: an upgrade re-applies the platform's configuration without
   re-granting the platform.
3. Nothing, on a fresh install with no value set — which is the absence the
   reconciler seeds from, on purpose.

The lookup is what the same file already does for the identity provider's
generated credentials, for the same reason: something that must survive an
upgrade cannot be reconstructed from values. It is guarded on `.Release.IsUpgrade`
because a lookup of a kind the cluster does not yet know is an error rather
than an empty answer, and on a first install this chart's own CRDs are not
registered at render time. Under `helm template` and `--dry-run` it answers
empty, so a rendered diff shows the list absent while a real upgrade preserves
it; that is a property of every lookup in this chart and of no guard here.
*/}}
{{- define "kitchen.accessBlock" -}}
{{- if .Values.kitchen.access.operators -}}
access:
  operators:
{{- range .Values.kitchen.access.operators }}
{{- if kindIs "string" . }}
    - subject: {{ . | quote }}
{{- if contains "@" . }}
      # An entry naming an address resolves against a token's `email` claim,
      # and only when the issuer has verified it.
      email: {{ . | quote }}
{{- end }}
{{- else }}
    - subject: {{ get . "subject" | quote }}
{{- with get . "email" }}
      email: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- else if and .Release.IsUpgrade .Values.kitchen.applyOnUpgrade -}}
{{- $live := lookup "kitchen.bermos.dev/v1alpha1" "Kitchen" "" "default" -}}
{{- $access := default dict (dig "spec" "access" dict (default dict $live)) -}}
{{- if hasKey $access "operators" -}}
{{- $operators := index $access "operators" -}}
{{- if $operators -}}
access:
  operators:
{{ toYaml $operators | indent 4 }}
{{- else -}}
access:
  # Somebody narrowed the platform to nobody on purpose. An empty list is not
  # an absent one, and only an empty list survives being re-applied as one.
  operators: []
{{- end }}
{{- end }}
{{- end }}
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
{{- if and .Values.kitchen.create (eq .Values.kitchen.tls.mode "acme") (ne (include "kitchen.acmeConfigured" .) "true") }}
{{- fail "kitchen.tls.mode is acme but kitchen.tls.acme is not configured: set kitchen.tls.acme.email and kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName. The API server refuses a Kitchen in acme mode without them, because the shared Gateway's HTTPS listener would terminate with a certificate nothing issues. To bring a cluster up before DNS and certificates are ready, set kitchen.tls.mode=none — every published URL is then http://." }}
{{- end }}
{{- if .Values.kitchen.access.operators }}
{{- if not .Values.kitchen.create }}
{{- fail "kitchen.access.operators is set but kitchen.create is false: the list is written into the Kitchen object, which this chart is not creating, so naming operators here would grant nobody anything. Set kitchen.create=true, or write spec.access.operators on the Kitchen object you manage yourself." }}
{{- end }}
{{- range .Values.kitchen.access.operators }}
{{- if kindIs "string" . }}
{{- if not . }}
{{- fail "kitchen.access.operators has an empty entry: each one is an email address, the issuer's `sub`, or a map with a `subject`. The API server refuses an access entry whose subject is empty, so the whole install would fail on the singleton." }}
{{- end }}
{{- else if kindIs "map" . }}
{{- if not (get . "subject") }}
{{- fail (printf "kitchen.access.operators has a map entry with no `subject` (%v): a map entry names the account in `subject` — the issuer's `sub`, or an email address — and may carry an informational `email` beside it." .) }}
{{- end }}
{{- else }}
{{- fail (printf "kitchen.access.operators entries must be strings or maps (got %s): each one is an email address, the issuer's `sub`, or a map of `subject` and `email`." (kindOf .)) }}
{{- end }}
{{- end }}
{{- end }}
{{- if and .Values.kitchen.create .Values.kitchen.ingress.cloudflared.enabled (not .Values.kitchen.ingress.cloudflared.tunnelSecretName) }}
{{- fail "kitchen.ingress.cloudflared.tunnelSecretName is required when cloudflared is enabled: create a secret holding the tunnel token under the key `token` first." }}
{{- end }}
{{- range $values := list "selfUpdate" "restore" "scaleToZero" "collector" "clickhouse" }}
{{- if not (get $.Values $values) }}
{{- fail (printf "%s values are missing: upgrade with --reset-then-reuse-values so new chart defaults are merged with existing overrides." $values) }}
{{- end }}
{{- end }}
{{- if .Values.selfUpdate.enabled }}
{{- if not .Values.rbac.create }}
{{- fail "selfUpdate.enabled requires rbac.create: the update job runs as its own ServiceAccount bound to cluster-admin, which this chart is not creating. Set rbac.create=true, or leave selfUpdate.enabled=false and upgrade with helm." }}
{{- end }}
{{- if not .Values.selfUpdate.chart }}
{{- fail "selfUpdate.chart is required when selfUpdate.enabled is true: the update job has nothing to upgrade from. Set it to the published chart (oci://ghcr.io/bermos/charts/kitchen) or to your own mirror." }}
{{- end }}
{{- end }}
{{- if .Values.restore.enabled }}
{{- if and (not .Values.restore.secretName) (not .Values.restore.existingClaim) }}
{{- fail "restore.enabled is true but neither restore.secretName nor restore.existingClaim names the archive: the Job has nothing to restore. Put the backup in a Secret under `backup.tar.gz`, or on a volume and set restore.existingClaim." }}
{{- end }}
{{- if and .Values.restore.secretName .Values.restore.existingClaim }}
{{- fail "restore.secretName and restore.existingClaim are both set: the archive comes from one place. Clear whichever is not the one holding it." }}
{{- end }}
{{- if not (hasPrefix "/" .Values.restore.path) }}
{{- fail (printf "restore.path must be an absolute path inside the container (got %q): its directory is the mount point and its filename is what the archive is projected as." .Values.restore.path) }}
{{- end }}
{{- if has (dir .Values.restore.path) (list "/manager" "/gate" "/backup" "/restore") }}
{{- fail (printf "restore.path is %q, and its directory %q is one of the operator image's own binaries. A volume mounted over a regular file does not start the container at all — it fails before anything runs, with no logs to read. Put the archive somewhere else, such as /archive/backup.tar.gz." .Values.restore.path (dir .Values.restore.path)) }}
{{- end }}
{{- end }}
{{- if .Values.scaleToZero.install.enabled }}
{{- if not .Values.rbac.create }}
{{- fail "scaleToZero.install.enabled requires rbac.create: the install job runs as its own ServiceAccount bound to cluster-admin, which this chart is not creating. Set rbac.create=true, or leave scaleToZero.install.enabled=false and install KEDA and its HTTP add-on yourself." }}
{{- end }}
{{- if not .Values.scaleToZero.enabled }}
{{- fail "scaleToZero.install.enabled is set but scaleToZero.enabled is false: the operator installs KEDA for a platform that idles environments, so this would grant an account cluster-admin and then install nothing. Set scaleToZero.enabled=true, or leave both off." }}
{{- end }}
{{- if not .Values.scaleToZero.install.chartRepository }}
{{- fail "scaleToZero.install.chartRepository is required when scaleToZero.install.enabled is true: the install job has nowhere to pull the two charts from. Set it to https://kedacore.github.io/charts or to your own mirror." }}
{{- end }}
{{- end }}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.leaderElection) }}
{{- fail "leaderElection must stay enabled when replicaCount > 1, otherwise every replica reconciles concurrently." }}
{{- end }}
{{- if and .Values.kitchen.compliance.audit.enabled (lt (int .Values.kitchen.compliance.audit.retentionDays) 90) }}
{{- fail (printf "kitchen.compliance.audit.retentionDays must be at least 90 (got %d): the incident reporting duty the log exists to serve runs from when an institution became aware, which can be well after the transition that caused it, and a log that has already aged out cannot substantiate the report." (int .Values.kitchen.compliance.audit.retentionDays)) }}
{{- end }}
{{- range .Values.kitchen.compliance.exceptions.ladder }}
{{- if not (has (default "" .role) (list "developer" "admin" "operator")) }}
{{- fail (printf "kitchen.compliance.exceptions.ladder entries need role one of developer, admin, operator (got %q): the ladder maps a requested duration to the approval it takes, and a rung nobody can hold approves nothing." (default "" .role)) }}
{{- end }}
{{- if not .maxDuration }}
{{- fail "kitchen.compliance.exceptions.ladder entries need maxDuration (e.g. 24h): a rung without a duration bounds nothing." }}
{{- end }}
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
{{- if and .Values.namespace.create (not (has .Values.namespace.podSecurity (list "privileged" "baseline" "restricted"))) }}
{{- fail (printf "namespace.podSecurity must be one of privileged, baseline, restricted (got %q)" .Values.namespace.podSecurity) }}
{{- end }}
{{- if and .Values.namespace.create .Values.collector.enabled (ne .Values.namespace.podSecurity "privileged") }}
{{- fail (printf "namespace.podSecurity is %q but collector.enabled is true: the telemetry agent mounts the node's log directory and root filesystem, and Pod Security admits hostPath at the privileged level alone, so its pods would be refused at admission and no logs, metrics or traces would be collected at all. Set namespace.podSecurity=privileged, or collector.enabled=false." .Values.namespace.podSecurity) }}
{{- end }}
{{- if .Values.collector.enabled }}
{{- if lt (int .Values.collector.export.batch.minSize) 1 }}
{{- fail "collector.export.batch.minSize must be at least 1: a batch of nothing never ships." }}
{{- end }}
{{- if lt (int .Values.collector.export.queueSize) (int .Values.collector.export.batch.minSize) }}
{{- fail "collector.export.queueSize must be at least collector.export.batch.minSize, otherwise the queue is full before it holds a batch and the collector drops signals it could never have written." }}
{{- end }}
{{- if and (gt (int .Values.collector.export.batch.maxSize) 0) (lt (int .Values.collector.export.batch.maxSize) (int .Values.collector.export.batch.minSize)) }}
{{- fail "collector.export.batch.maxSize must be 0 (no ceiling) or at least collector.export.batch.minSize: a ceiling below the floor is a batch that can never be formed." }}
{{- end }}
{{- if lt (int .Values.collector.metrics.intervalSeconds) 1 }}
{{- fail "collector.metrics.intervalSeconds must be at least 1: a scrape interval of zero is not a scrape." }}
{{- end }}
{{- if eq (int .Values.collector.otlp.grpcPort) (int .Values.kitchen.observability.traces.port) }}
{{- fail (printf "collector.otlp.grpcPort and kitchen.observability.traces.port are both %d: OTLP/gRPC and OTLP/HTTP are two listeners in one container, so they cannot share a port." (int .Values.collector.otlp.grpcPort)) }}
{{- end }}
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
{{- if .Values.registry.enabled }}
{{- if and .Values.kitchen.create (ne .Values.kitchen.tls.mode "none") (not (include "kitchen.registryHost" .)) }}
{{- fail "registry.enabled needs a hostname: set kitchen.baseDomain or registry.host. The bundled registry is published on the shared Gateway, and the node pulls images over that name." }}
{{- end }}
{{- if not .Values.registry.auth.username }}
{{- fail "registry.auth.username is required when registry.enabled: the registry admits no anonymous access, because publishing it on the base domain puts it on the internet." }}
{{- end }}
{{- if lt (int .Values.registry.retention.keepTags) 1 }}
{{- fail "registry.retention.keepTags must be at least 1: keeping no tags at all would delete the image every environment is currently running." }}
{{- end }}
{{- end }}
{{- if .Values.scaleToZero.enabled }}
{{- if not .Values.scaleToZero.interceptor.service }}
{{- fail "scaleToZero.interceptor.service is required when scaleToZero.enabled: an idling environment has no pods of its own, so the operator has to route its URL at the interceptor that starts them. Leave it at the default unless the HTTP add-on was installed under another name." }}
{{- end }}
{{- if not .Values.scaleToZero.interceptor.namespace }}
{{- fail "scaleToZero.interceptor.namespace is required when scaleToZero.enabled: the KEDA HTTP add-on is its own Helm release, so the operator has to be told which namespace it went into." }}
{{- end }}
{{- $port := int .Values.scaleToZero.interceptor.port }}
{{- if or (lt $port 1) (gt $port 65535) }}
{{- fail (printf "scaleToZero.interceptor.port must be a TCP port between 1 and 65535 (got %d)" $port) }}
{{- end }}
{{- end }}
{{- end }}
