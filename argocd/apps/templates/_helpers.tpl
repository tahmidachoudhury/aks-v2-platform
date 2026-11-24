{{/*
The head of a child Application: its name and the Argo CD namespace.
Takes a dict: root (the chart's context), name, wave.
*/}}
{{- define "apps.head" -}}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ .name }}
  namespace: {{ .root.Release.Namespace }}
{{- end }}

{{/* This repository at the root's revision, as the start of a source. */}}
{{- define "apps.repo" -}}
repoURL: {{ .Values.repoURL }}
targetRevision: {{ .Values.targetRevision }}
{{- end }}

{{/*
The tail every child shares: the cluster, the namespace for resources that
name none, and the sync policy.
- Server-side apply: ESO's, KEDA's and cert-manager's CRDs are too large
  for the last-applied annotation, and from Argo CD 3.6 it also selects the
  server-side diff, which leaves out what mutating webhooks add.
No resources-finalizer: deleting an Application leaves its resources, so
the evening destroy never waits on a cascade (README.md).
Takes a dict: namespace.
*/}}
{{- define "apps.tail" -}}
destination:
  server: https://kubernetes.default.svc
  namespace: {{ .namespace }}
syncPolicy:
  syncOptions:
    - ServerSideApply=true
{{- end }}
