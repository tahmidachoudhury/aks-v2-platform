{{/*
The head of a child Application: its name, the Argo CD namespace and its
sync wave. The root applies the children wave by wave and waits for each
wave to turn Healthy (argo-cd-values.yaml in infra/argocd).
Takes a dict: root (the chart's context), name, wave.
*/}}
{{- define "apps.head" -}}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ .name }}
  namespace: {{ .root.Release.Namespace }}
  annotations:
    argocd.argoproj.io/sync-wave: {{ .wave | quote }}
{{- end }}

{{/* This repository at the root's revision, as the start of a source. */}}
{{- define "apps.repo" -}}
repoURL: {{ .Values.repoURL }}
targetRevision: {{ .Values.targetRevision }}
{{- end }}

{{/*
The tail every child shares: the cluster, the namespace for resources that
name none, and the sync policy.
- Automated sync with selfHeal everywhere, and prune everywhere but data.
- Server-side apply: ESO's, KEDA's and cert-manager's CRDs are too large
  for the last-applied annotation, and from Argo CD 3.6 it also selects the
  server-side diff, which leaves out fields other managers own, such as
  the AKS admissions enforcer's additions to webhook selectors.
- SkipDryRunOnMissingResource where a custom resource's CRD comes from
  another Application, so the dry run doesn't fail before it exists.
- Retries without a limit, every 3 minutes at most, picking up a new
  revision as it lands (refresh). Auto-sync never tries a failed revision
  again by itself, so a child that gave up would hold its wave, and every
  wave after it, until the next commit. migrations is the exception: it
  passes retryLimit 2, since rerunning a failed SQL file only marks the
  version dirty again (k8s/migrations/README.md).
No resources-finalizer: deleting an Application leaves its resources, so
the evening destroy never waits on a cascade (README.md).
Takes a dict: namespace, and optionally noPrune, skipDryRun, retryLimit.
*/}}
{{- define "apps.tail" -}}
destination:
  server: https://kubernetes.default.svc
  namespace: {{ .namespace }}
syncPolicy:
  automated:
    prune: {{ not .noPrune }}
    selfHeal: true
  syncOptions:
    - ServerSideApply=true
    {{- if .skipDryRun }}
    - SkipDryRunOnMissingResource=true
    {{- end }}
  retry:
    limit: {{ hasKey . "retryLimit" | ternary .retryLimit -1 }}
    refresh: true
    backoff:
      duration: 10s
      factor: 2
      maxDuration: 3m
{{- end }}
