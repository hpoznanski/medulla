{{- define "medulla.fullname" -}}
{{- printf "%s" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "medulla.labels" -}}
app.kubernetes.io/name: medulla
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "medulla.selectorLabels" -}}
app.kubernetes.io/name: medulla
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
