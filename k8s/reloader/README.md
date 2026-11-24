# reloader

**TL;DR:** [Stakater Reloader](https://github.com/stakater/Reloader) 2.2.20 (Reloader v1.4.24) runs in `platform`, installed by Argo CD from its Helm chart. It restarts a Deployment, or the scheduler's StatefulSet, when a Secret named in its `secret.reloader.stakater.com/reload` annotation changes. Each service lists only its own Secret, so rotating one Key Vault secret restarts only the services that read it. Argo CD installs it each morning at wave 5, before the services. Filled by P6-06 (#67).

The services read their secrets from environment variables once, at startup. ESO updates the Kubernetes Secret within its one-hour `refreshInterval`, but a running pod keeps the old value until it restarts ([Secrets delivery](../../docs/architecture-decisions.md#secrets-delivery)).

## Which rotation restarts what

| Key Vault secret | Kubernetes Secret in `apps` | Refresh | Restarts |
| --- | --- | --- | --- |
| `jwt-secret` | `api-gateway` | Hourly | api-gateway |
| `shipping-webhook-secret` | `shipping-service` | Hourly | shipping-service |
| `redis-password` | `api-gateway-redis` (inside `REDIS_URL`) | On `force-sync` only | api-gateway |
| `postgres-password` | `<service>-postgres`, one per Postgres user: order, inventory, payment, notification, shipping, dashboard-api, scheduler | On `force-sync` only | All seven |

The worker has no Secret, so nothing restarts it. The Postgres and Redis copies wait for `force-sync` because the server must take the new password first. The order is in [data](../data/README.md#rotating-a-password).

## Settings

| Value | Setting | Why |
| --- | --- | --- |
| `autoReloadAll` | `false` | Only annotated workloads restart, and only for the Secrets they name |
| `ignoreConfigMaps` | `true` | Our ConfigMaps have a hash suffix, so an edit rolls the pods without Reloader |
| `reloadStrategy` | `annotations` | Reloader stamps an annotation on the pod template, not an extra environment variable. The container spec stays as Git has it, which Reloader's docs recommend for Argo CD. The annotation isn't in Git, so selfHeal leaves it |
| `readOnlyRootFileSystem` | `true` | The chart then mounts an `emptyDir` at `/tmp` |

There is no `namespaceSelector`, since the chart passes the value to Reloader wrapped in literal quotes. Reloader watches every namespace but acts only on annotated workloads, and only the services in `apps` have the annotation.

## Pod Security and sizing

The chart sets a non-root pod (user 65534) with the `RuntimeDefault` seccomp profile. `values.yaml` adds `allowPrivilegeEscalation: false` and drops every capability, which `restricted` needs. The pod requests 10m CPU and 32Mi, with a 128Mi memory limit and no CPU limit. See the [platform quota](../namespaces/README.md#sizes) arithmetic in [apps](../apps/README.md#platform-quota).

## Install each morning

The `reloader` Application installs it at wave 5 [argocd](../../argocd/README.md#how-the-cluster-comes-up). It renders chart `reloader` 2.2.20 with `values.yaml`. To upgrade, read the chart's release notes, then change its `targetRevision` in `argocd/apps/templates/platform.yaml` and the version in the header of `values.yaml`.

## Proving it

P6-06 (#67): rotating `shipping-webhook-secret` restarts shipping-service and nothing else. This needs a live cluster after `make up`. Rotation is a persistent apply, which only Tahmid runs. Bump `shipping-webhook-secret` in `generated_secrets` in `infra/persistent/main.tf`, then plan and apply persistent.

```bash
CTX=aks-v2-platform
# Pod names before, to compare after.
kubectl --context $CTX -n apps get pods -L app.kubernetes.io/name --no-headers | sort > /tmp/pods-before

# After the persistent apply, skip ESO's hourly wait.
kubectl --context $CTX -n apps annotate externalsecret shipping-service force-sync=$(date +%s) --overwrite
kubectl --context $CTX -n apps wait externalsecret/shipping-service --for=condition=Ready --timeout=2m
kubectl --context $CTX -n apps rollout status deployment/shipping-service --timeout=5m

kubectl --context $CTX -n apps get pods -L app.kubernetes.io/name --no-headers | sort > /tmp/pods-after
diff /tmp/pods-before /tmp/pods-after
kubectl --context $CTX -n platform logs deploy/reloader-reloader --since=10m | grep -i shipping-service
```

Expect the `diff` to show only `shipping-service` pods replaced, and a Reloader log line for `apps/shipping-service`.

## Validate

```bash
# The repo's own Helm repository config, as infra/argocd uses, so a stale entry
# in your own Helm config can't fail the render. Run from the repo root.
export HELM_REPOSITORY_CONFIG="$PWD/.helm/repositories.yaml" HELM_REPOSITORY_CACHE="$PWD/.helm/cache"
helm template reloader reloader --repo https://stakater.github.io/stakater-charts \
  --version 2.2.20 --namespace platform -f k8s/reloader/values.yaml \
  | kubeconform -strict -summary
```
