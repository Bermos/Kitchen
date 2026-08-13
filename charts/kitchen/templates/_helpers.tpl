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
Public base URL of the operator API, mirroring the operator's own default of
https://kitchen.<baseDomain>.
*/}}
{{- define "kitchen.apiExternalURL" -}}
{{- if .Values.kitchen.api.externalURL }}
{{- .Values.kitchen.api.externalURL | trimSuffix "/" }}
{{- else if .Values.kitchen.baseDomain }}
{{- printf "https://kitchen.%s" .Values.kitchen.baseDomain }}
{{- end }}
{{- end }}

{{/*
Hostname the git webhook receiver is published under.
*/}}
{{- define "kitchen.webhookHost" -}}
{{- if .Values.webhookReceiver.route.host }}
{{- .Values.webhookReceiver.route.host }}
{{- else }}
{{- $url := include "kitchen.apiExternalURL" . }}
{{- $hostPort := $url | replace "https://" "" | replace "http://" "" | splitList "/" | first }}
{{- $hostPort | splitList ":" | first }}
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
{{- if and .Values.kitchen.create .Values.kitchen.ingress.cloudflared.enabled (not .Values.kitchen.ingress.cloudflared.tunnelSecretName) }}
{{- fail "kitchen.ingress.cloudflared.tunnelSecretName is required when cloudflared is enabled: create a secret holding the tunnel token under the key `token` first." }}
{{- end }}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.leaderElection) }}
{{- fail "leaderElection must stay enabled when replicaCount > 1, otherwise every replica reconciles concurrently." }}
{{- end }}
{{- end }}
