{{/*
Общие helper'ы чарта.

Комментарии в шаблонах намеренно краткие: развёрнутые обоснования решений
(почему у minio args, а не command; почему у проб RabbitMQ таймауты в 15 с;
почему у БД возвращены капабилити) живут в одноимённых файлах k8s/ — чарт их
упаковывает, а не переписывает.
*/}}

{{/* Имя чарта; nameOverride позволяет переопределить его в метках. */}}
{{- define "gophprofile.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
fullname — префикс для объектов, чьё имя должно быть уникальным в namespace
(сейчас это только Job миграций). Если имя релиза уже содержит имя чарта
(обычный случай: release=gophprofile, chart=gophprofile), дублировать его не
нужно — иначе получилось бы gophprofile-gophprofile-migrate.
*/}}
{{- define "gophprofile.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
namespace — куда ставить объекты. Приоритет у global.namespace, но по умолчанию
он пуст, и тогда берётся namespace релиза (-n / --create-namespace).
*/}}
{{- define "gophprofile.namespace" -}}
{{- default .Release.Namespace .Values.global.namespace -}}
{{- end -}}

{{/* Общие метки: что это за чарт, какая версия, кто управляет объектом. */}}
{{- define "gophprofile.labels" -}}
app.kubernetes.io/name: {{ include "gophprofile.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/*
selectorLabels — идентичность компонента: app=<component>.

Вызывается как include "gophprofile.selectorLabels" (dict "component" "server").
Используется именно эта метка, а не app.kubernetes.io/name: на неё опираются
селекторы Service и правила NetworkPolicy, которые в k8s/61-networkpolicy.yaml
записаны как app: server, app: postgres и т. д. Один общий app.kubernetes.io/name
на все поды сломал бы и то, и другое.
*/}}
{{- define "gophprofile.selectorLabels" -}}
app: {{ required "component is required" .component }}
{{- end -}}
