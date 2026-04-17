{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-sustain.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "k8s-sustain.fullname" -}}
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

{{/*
Create chart label.
*/}}
{{- define "k8s-sustain.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "k8s-sustain.labels" -}}
helm.sh/chart: {{ include "k8s-sustain.chart" . }}
{{ include "k8s-sustain.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "k8s-sustain.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Image reference.
*/}}
{{- define "k8s-sustain.image" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "k8s-sustain.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- include "k8s-sustain.fullname" . }}
{{- end }}
{{- end }}

{{/*
Webhook server name (appends -webhook to the full name).
*/}}
{{- define "k8s-sustain.webhookName" -}}
{{- printf "%s-webhook" (include "k8s-sustain.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels for the webhook Deployment / Service.
*/}}
{{- define "k8s-sustain.webhookSelectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: webhook
{{- end }}

{{/*
Dashboard server name (appends -dashboard to the full name).
*/}}
{{- define "k8s-sustain.dashboardName" -}}
{{- printf "%s-dashboard" (include "k8s-sustain.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Prometheus address. Auto-detects the bundled prometheus subchart service
when prometheusAddress is empty and prometheus.enabled=true.
*/}}
{{- define "k8s-sustain.prometheusAddress" -}}
{{- if .Values.prometheusAddress }}
{{- .Values.prometheusAddress }}
{{- else if .Values.prometheus.enabled }}
{{- printf "http://%s-prometheus-server.%s.svc:80" .Release.Name .Release.Namespace }}
{{- else }}
{{- "http://localhost:9090" }}
{{- end }}
{{- end }}

{{/*
Selector labels for the dashboard Deployment / Service.
*/}}
{{- define "k8s-sustain.dashboardSelectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-sustain.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: dashboard
{{- end }}
