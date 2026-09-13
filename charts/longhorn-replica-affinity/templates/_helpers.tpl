{{- define "lra.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "lra.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "lra.name" . | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "lra.webhookName" -}}{{ include "lra.fullname" . }}-webhook{{- end -}}
{{- define "lra.reconcilerName" -}}{{ include "lra.fullname" . }}-reconciler{{- end -}}

{{- define "lra.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "lra.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "lra.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "lra.tlsSecret" -}}
{{- if eq .Values.tls.mode "self-signed" -}}
{{ include "lra.fullname" . }}-tls
{{- else -}}
{{ .Values.tls.secretName | default (printf "%s-tls" (include "lra.fullname" .)) }}
{{- end -}}
{{- end -}}
