{{- define "azure-k8s-role-assigner.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "azure-k8s-role-assigner.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "azure-k8s-role-assigner.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{- define "azure-k8s-role-assigner.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "azure-k8s-role-assigner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: controller
app.kubernetes.io/part-of: azure-k8s-role-assigner
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "azure-k8s-role-assigner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "azure-k8s-role-assigner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
