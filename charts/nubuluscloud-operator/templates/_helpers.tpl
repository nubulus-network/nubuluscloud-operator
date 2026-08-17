{{- define "nubuluscloud-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "nubuluscloud-operator.fullname" -}}
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

{{- define "nubuluscloud-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "nubuluscloud-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "nubuluscloud-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nubuluscloud-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "nubuluscloud-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "nubuluscloud-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The agent image, refused rather than defaulted.

The message is long on purpose. Failing here costs a re-run of helm install;
failing later costs an operator that crash loops with the reason in a log
nobody is watching yet.
*/}}
{{- define "nubuluscloud-operator.agentImage" -}}
{{- required "agent.image is required: set it to the tunnel agent image the operator should deploy for every Tunnel. There is no default because a wrong one would pull an unexpected image into your cluster and hand it a credential." .Values.agent.image -}}
{{- end -}}
