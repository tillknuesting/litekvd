{{/*
The pod spec both StatefulSets share.

Everything that differs between a leader and a replica is passed in through
.role and .extraArgs, so that the only thing the two workloads disagree about
is the thing they are supposed to disagree about. A leader and a replica with
different -max-value or -segment-size is a pair that works until it does not.
*/}}
{{- define "litekvd.podSpec" -}}
{{- $root := .root -}}
serviceAccountName: {{ include "litekvd.serviceAccountName" $root }}
automountServiceAccountToken: false
terminationGracePeriodSeconds: {{ $root.Values.terminationGracePeriodSeconds }}
securityContext: {{- toYaml $root.Values.podSecurityContext | nindent 2 }}
{{- with $root.Values.image.pullSecrets }}
imagePullSecrets: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $root.Values.nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $root.Values.tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $root.Values.affinity }}
affinity: {{- toYaml . | nindent 2 }}
{{- end }}
{{- if $root.Values.topologySpread.enabled }}
topologySpreadConstraints:
  - maxSkew: {{ $root.Values.topologySpread.maxSkew }}
    topologyKey: {{ $root.Values.topologySpread.topologyKey }}
    whenUnsatisfiable: {{ $root.Values.topologySpread.whenUnsatisfiable }}
    labelSelector:
      matchLabels: {{- include "litekvd.selectorLabels" $root | nindent 8 }}
{{- end }}
containers:
  - name: litekvd
    image: "{{ $root.Values.image.repository }}:{{ $root.Values.image.tag | default $root.Chart.AppVersion }}"
    imagePullPolicy: {{ $root.Values.image.pullPolicy }}
    securityContext: {{- toYaml $root.Values.securityContext | nindent 6 }}
    args:
      {{- include "litekvd.commonArgs" $root | nindent 6 }}
      {{- with .extraArgs }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    ports:
      - name: http
        containerPort: {{ $root.Values.service.port }}
        protocol: TCP
    {{- if $root.Values.probes.startup.enabled }}
    # Opening a store reads every key before it answers anything, so a large
    # one takes a while and is not sick while it does. This is what stops the
    # kubelet killing a pod for doing its job; liveness only starts after it
    # has passed once.
    startupProbe:
      httpGet:
        path: /health
        port: http
      periodSeconds: {{ $root.Values.probes.startup.periodSeconds }}
      failureThreshold: {{ $root.Values.probes.startup.failureThreshold }}
    {{- end }}
    livenessProbe:
      httpGet:
        path: /health
        port: http
      {{- toYaml $root.Values.probes.liveness | nindent 6 }}
    readinessProbe:
      httpGet:
        path: /health
        port: http
      {{- toYaml $root.Values.probes.readiness | nindent 6 }}
    resources: {{- toYaml $root.Values.resources | nindent 6 }}
    volumeMounts:
      - name: data
        mountPath: /data
      {{- if $root.Values.auth.enabled }}
      - name: token
        mountPath: /etc/litekvd
        readOnly: true
      {{- end }}
{{- if $root.Values.auth.enabled }}
volumes:
  - name: token
    secret:
      secretName: {{ include "litekvd.secretName" $root }}
      items:
        - key: token
          path: token
      defaultMode: 0400
{{- end }}
{{- end -}}

{{/*
The volumeClaimTemplate both StatefulSets share, or an emptyDir when
persistence is off — which is a thing to do in a test and nowhere else, since
it means a restarted pod comes back empty and a replica that comes back empty
takes a whole snapshot from its leader.
*/}}
{{- define "litekvd.volumeClaim" -}}
- metadata:
    name: data
    labels: {{- include "litekvd.labels" . | nindent 6 }}
    {{- with .Values.persistence.annotations }}
    annotations: {{- toYaml . | nindent 6 }}
    {{- end }}
  spec:
    accessModes: {{- toYaml .Values.persistence.accessModes | nindent 6 }}
    {{- if .Values.persistence.storageClass }}
    storageClassName: {{ .Values.persistence.storageClass | quote }}
    {{- end }}
    resources:
      requests:
        storage: {{ .Values.persistence.size | quote }}
{{- end -}}
