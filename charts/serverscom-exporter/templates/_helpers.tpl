{{- define "serverscom-exporter.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "serverscom-exporter.fullname" -}}
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

{{- define "serverscom-exporter.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "serverscom-exporter.labels" -}}
helm.sh/chart: {{ include "serverscom-exporter.chart" . }}
{{ include "serverscom-exporter.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "serverscom-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ include "serverscom-exporter.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "serverscom-exporter.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "serverscom-exporter.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "serverscom-exporter.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}
{{- end }}

{{/* Secret with the API token. */}}
{{- define "serverscom-exporter.tokenSecretName" -}}
{{- if .Values.apiToken.existingSecret }}
{{- .Values.apiToken.existingSecret }}
{{- else }}
{{- printf "%s-token" (include "serverscom-exporter.fullname" .) }}
{{- end }}
{{- end }}

{{/* Secret with web-config.yml. */}}
{{- define "serverscom-exporter.webConfigSecretName" -}}
{{- if .Values.webConfig.existingSecret }}
{{- .Values.webConfig.existingSecret }}
{{- else }}
{{- printf "%s-web-config" (include "serverscom-exporter.fullname" .) }}
{{- end }}
{{- end }}

{{/* Exporter flags; empty values keep the exporter defaults. */}}
{{- define "serverscom-exporter.args" -}}
{{- $c := .Values.config }}
{{- $flags := list
  (list "api.url" $c.apiUrl)
  (list "api.timeout" $c.apiTimeout)
  (list "api.page-size" $c.apiPageSize)
  (list "api.requests-per-hour" $c.requestsPerHour)
  (list "api.ratelimit-reserve" $c.ratelimitReserve)
  (list "refresh.hosts-interval" $c.refresh.hostsInterval)
  (list "refresh.configuration-interval" $c.refresh.configurationInterval)
  (list "refresh.drives-interval" $c.refresh.drivesInterval)
  (list "refresh.ports-interval" $c.refresh.portsInterval)
  (list "refresh.networks-interval" $c.refresh.networksInterval)
  (list "refresh.features-interval" $c.refresh.featuresInterval)
  (list "refresh.power-feeds-interval" $c.refresh.powerFeedsInterval)
  (list "refresh.uplinks-interval" $c.refresh.uplinksInterval)
  (list "refresh.l2-segments-interval" $c.refresh.l2SegmentsInterval)
  (list "refresh.reference-interval" $c.refresh.referenceInterval)
  (list "refresh.auto-min-interval" $c.refresh.autoMinInterval)
  (list "refresh.auto-max-interval" $c.refresh.autoMaxInterval)
  (list "cache.save-interval" $c.cacheSaveInterval)
  (list "collector.host-labels" $c.hostLabels)
  (list "web.telemetry-path" $c.metricsPath)
  (list "log.level" $c.logLevel)
  (list "log.format" $c.logFormat)
-}}
- {{ printf "--api.token-file=/etc/serverscom-exporter/token/%s" .Values.apiToken.secretKey | quote }}
- {{ printf "--web.listen-address=:%v" .Values.service.port | quote }}
- "--cache.file=/data/cache.json"
{{- range $flags }}
{{- $value := index . 1 }}
{{- if ne (toString $value) "" }}
- {{ printf "--%s=%v" (index . 0) $value | quote }}
{{- end }}
{{- end }}
{{- range $name, $enabled := $c.collectors }}
{{- if not $enabled }}
- {{ printf "--no-collector.%s" $name | quote }}
{{- end }}
{{- end }}
{{- if .Values.webConfig.enabled }}
- {{ printf "--web.config.file=/etc/serverscom-exporter/web/%s" .Values.webConfig.secretKey | quote }}
{{- end }}
{{- range .Values.extraArgs }}
- {{ tpl . $ | quote }}
{{- end }}
{{- end }}
