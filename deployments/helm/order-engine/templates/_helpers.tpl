{{- define "order-engine.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "order-engine.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "order-engine.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/name: {{ include "order-engine.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "order-engine.selectorLabels" -}}
app.kubernetes.io/name: {{ include "order-engine.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "order-engine.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "order-engine.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end }}

{{/* Immutable digest preferred; tag is the development fallback. */}}
{{- define "order-engine.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else if .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- else -}}
{{- fail "set image.digest (preferred for production) or image.tag" -}}
{{- end -}}
{{- end }}

{{/* Defense-in-depth beyond values.schema.json. */}}
{{- define "order-engine.validateRouting" -}}
{{- if not (has .Values.routing.mode (list "grpc" "http")) -}}
{{- fail (printf "routing.mode must be \"grpc\" or \"http\", got %q" .Values.routing.mode) -}}
{{- end -}}
{{- end }}
