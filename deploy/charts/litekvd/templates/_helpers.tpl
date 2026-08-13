{{/* The chart's name, overridable. */}}
{{- define "litekvd.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The name everything is prefixed with.

Truncated at 63 because that is what a label value takes, and StatefulSet pod
names are this plus an ordinal — so leaving room matters more here than it does
for a Deployment.
*/}}
{{- define "litekvd.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 55 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 55 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 55 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "litekvd.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Labels on every object. */}}
{{- define "litekvd.labels" -}}
helm.sh/chart: {{ include "litekvd.chart" . }}
{{ include "litekvd.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
What a Service selects on: every litekvd pod in the release, leader and replica
alike. Never put the role in here — see litekvd.componentLabels for why.
*/}}
{{- define "litekvd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "litekvd.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "litekvd.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "litekvd.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The Secret holding the bearer token, whether this chart made it or not. */}}
{{- define "litekvd.secretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-token" (include "litekvd.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
The flags both roles share, one per line.

Kept in one place because a leader and a replica that disagree about -max-value
or -segment-size are a pair that works until the day it does not.
*/}}
{{- define "litekvd.commonArgs" -}}
- -dir=/data
{{- /* Not the system temp directory. A snapshot on its way to a follower is
       spooled to a file, and the container's /tmp does not exist in this image
       and would be the wrong place for a copy of the live records if it did.
       /data is the volume that is sized for them. */}}
- -spool-dir=/data
- -addr=0.0.0.0:{{ .Values.service.port }}
- -sync={{ .Values.config.sync }}
{{- with .Values.config.syncInterval }}
- -sync-interval={{ . }}
{{- end }}
{{- with .Values.config.segmentSize }}
- -segment-size={{ . }}
{{- end }}
{{- with .Values.config.mergeTrigger }}
- -merge-trigger={{ . }}
{{- end }}
{{- with .Values.config.maxValue }}
- -max-value={{ . }}
{{- end }}
{{- with .Values.config.maxBatch }}
- -max-batch={{ . }}
{{- end }}
{{- with .Values.config.maxScan }}
- -max-scan={{ . }}
{{- end }}
{{- with .Values.config.queue }}
- -queue={{ . }}
{{- end }}
{{- with .Values.config.heartbeat }}
- -heartbeat={{ . }}
{{- end }}
{{- with .Values.config.shutdownTimeout }}
- -shutdown-timeout={{ . }}
{{- end }}
{{- if .Values.auth.enabled }}
- -token-file=/etc/litekvd/token
{{- end }}
{{- end -}}

{{/*
What a controller selects on, and what a PodDisruptionBudget selects on.

This is the workload a pod belongs to and it never changes. The role does
change — that is the whole promotion mechanism — and the two must therefore be
different labels.

Putting the role in a StatefulSet selector was tried and it is broken in a way
that only shows up when you promote: relabelling a pod makes it stop matching
its own controller, which orphans it, and the StatefulSet quietly builds a
replacement while the PodDisruptionBudget reports UnmanagedPods. The failover
drill in deploy/README.md is what found it.
*/}}
{{- define "litekvd.componentLabels" -}}
{{ include "litekvd.selectorLabels" . }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}
