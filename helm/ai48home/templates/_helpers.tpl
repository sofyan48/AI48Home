{{- define "ai48home.fullname" -}}
{{- default (printf "%s-%s" .Release.Name (default .Chart.Name .Values.nameOverride)) .Values.fullnameOverride | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{- define "ai48home.selectorLabels" -}}
app.kubernetes.io/name: {{ default .Chart.Name .Values.nameOverride | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "ai48home.labels" -}}
{{ include "ai48home.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}

{{/* An explicit per-component empty class opts out of dynamic provisioning. */}}
{{- define "ai48home.storageClass" -}}
{{- if hasKey .persistence "storageClass" -}}
storageClassName: {{ .persistence.storageClass | quote }}
{{- else if .global.storageClass -}}
storageClassName: {{ .global.storageClass | quote }}
{{- end -}}
{{- end -}}

{{/*
Resolves a generated secret value: an explicit value wins, then the value already
stored in the release Secret, then a new random key. Reusing the stored value keeps
the key stable across upgrades; Home imports config.yaml into the database only once.
Usage: include "ai48home.generatedSecret" (dict "ctx" $ "key" "management-key" "value" .Values.secrets.managementKey)
*/}}
{{- define "ai48home.generatedSecret" -}}
{{- if .value -}}
{{- .value -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .ctx.Release.Namespace (printf "%s-config" (include "ai48home.fullname" .ctx)) -}}
{{- $stored := "" -}}
{{- if and $existing $existing.data -}}
{{- $stored = index $existing.data .key | default "" | b64dec -}}
{{- end -}}
{{- $stored | default (randAlphaNum 48) -}}
{{- end -}}
{{- end -}}

{{/*
Home path prefixes for Ingress/HTTPRoute as a JSON array. Home serves from the
root (no base path), so it needs its UI, asset and API prefixes. A legacy single
`path` value is still accepted. Usage: include "ai48home.homePaths" .Values.gateway.home
*/}}
{{- define "ai48home.homePaths" -}}
{{- $paths := .paths | default list -}}
{{- if and (not $paths) .path -}}
{{- $paths = list .path -}}
{{- end -}}
{{- if not $paths -}}
{{- $paths = list "/admin" "/app" "/assets" "/user" "/v8" -}}
{{- end -}}
{{- if has "/home" $paths -}}
{{- fail "Home has no /home base path; use home.paths (default: /admin /app /assets /user /v8) or a separate hostname" -}}
{{- end -}}
{{- toJson $paths -}}
{{- end -}}

{{- define "ai48home.databaseHost" -}}
{{- if .Values.postgresql.enabled -}}
{{ include "ai48home.fullname" . }}-postgresql
{{- else -}}
{{ required "postgresql.external.host is required when postgresql.enabled=false" .Values.postgresql.external.host }}
{{- end -}}
{{- end -}}
