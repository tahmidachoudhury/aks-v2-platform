# keda

**TL;DR:** KEDA 2.21.1 is installed with Helm into `platform`. It scales the worker on the `order-events` queue's active message count, 5 messages per replica, from 1 to 5 replicas ([Autoscaling](../../docs/architecture-decisions.md#autoscaling)). The operator reads the queue as its own workload identity, `id-keda`, which holds Azure Service Bus Data Owner on that queue only. The client ID goes in with `--set-string` at install time, so none is committed. `make keda` runs each morning before `make apps`, since the worker's ScaledObject needs KEDA's CRDs. ArgoCD takes over in P7. Filled by P6-09 (#70).

The ScaledObject and its TriggerAuthentication live with the worker, in [`k8s/apps/worker/base/scaledobject.yaml`](../apps/worker/base/scaledobject.yaml).

## How it signs in

| Piece | Value | Set by |
| --- | --- | --- |
| Identity | `id-keda`, in `rg-aks-v2-persistent` | `infra/persistent/main.tf` |
| Federated subject | `system:serviceaccount:platform:keda-operator`, the chart's default ServiceAccount | `infra/platform/main.tf` |
| Role | Azure Service Bus Data Owner on the queue `order-events` | `infra/platform/main.tf` |
| Pod label and client ID annotation | `podIdentity.azureWorkload.enabled: true` in `values.yaml`, the client ID with `--set-string` | The chart |
| Tenant | Injected by the AKS webhook | AKS |
| Trigger authentication | `podIdentity.provider: azure-workload` | `worker-servicebus` in `apps` |

Only the operator pod gets the label. The metrics server asks the operator for the numbers and never calls Azure.

The scaler needs `Manage` rights to read the queue's message count: "Manage access is required for KEDA to be able to get metrics from Service Bus" ([KEDA's Service Bus scaler docs](https://keda.sh/docs/latest/scalers/azure-service-bus/)). Data Owner is the only built-in Service Bus data role with them, and [Microsoft's KEDA workload identity guide](https://learn.microsoft.com/en-us/azure/aks/keda-workload-identity) assigns it. It also lets the holder send, receive and change the queue, so it is scoped to the one queue, not the namespace. If the scaler logs a 401 at queue scope, widen the assignment to the namespace in `infra/platform/main.tf`.

The worker's own identity stays a Receiver. KEDA doesn't use it.

### OpenTofu changes, applied by hand

`id-keda` and its role need a persistent apply, then a platform apply, by Tahmid:

1. **Persistent** creates `id-keda`. It also adds Azure Service Bus Data Owner to `infra_assignable_roles`, which rewrites the condition on CI's RBAC Administrator assignment. The plan shows `infra_rbac_admin["persistent"] must be replaced`, with Data Owner's GUID the only change inside. This changes CI's own access, so apply it by hand ([CI and the ABAC condition](../../infra/modules/workload-identity/README.md#ci-and-the-abac-condition)).
2. **Platform** adds the federated credential for `keda-operator` and the Data Owner assignment on the queue.

Until both are applied, `make keda` stops with "No keda client ID in the persistent outputs".

## Settings

| Value | Setting | Why |
| --- | --- | --- |
| `podIdentity.azureWorkload.enabled` | `true` | Labels the operator's pods and annotates `keda-operator` with the client ID |
| `webhooks.enabled` | `false` | The webhooks only catch ScaledObject mistakes (`failurePolicy: Ignore`) and there is one ScaledObject. AKS's admissions enforcer rewrites every webhook's `namespaceSelector`, which breaks a second `helm upgrade` (as [cert-manager's values](../cert-manager/values.yaml) note), and this chart can't declare the selector. It also saves a pod |
| CPU limits | `null` | The chart sets 1 CPU by default. No `platform` component has a CPU limit |

The trigger uses KEDA's defaults: it counts active messages only (not dead-lettered or scheduled ones), polls every 30 seconds and scales down after 300 seconds without activity. `minReplicaCount: 1` keeps one worker up, so the first order after a quiet spell needs no cold start.

KEDA 2.21 is tested against Kubernetes 1.34 to 1.36 ([cluster compatibility](https://keda.sh/docs/latest/operate/cluster/)), and the cluster runs 1.37. 2.21.1 is the newest release. Check the operator's logs after the first install, and move to 2.22 when it ships.

## Pod Security and sizing

The chart runs non-root, drops every capability, blocks privilege escalation, and uses the `RuntimeDefault` seccomp profile and a read-only root filesystem, which `restricted` needs.

| Pod | CPU request | Memory request | Memory limit |
| --- | --- | --- | --- |
| Operator | 20m | 96Mi | 256Mi |
| Metrics server | 20m | 64Mi | 256Mi |

The `platform` quota arithmetic with KEDA and Reloader is in [apps](../apps/README.md#platform-quota). The [namespaces](../namespaces/README.md#capacity) capacity table kept about 1 CPU outside these namespaces for ArgoCD and KEDA. KEDA now runs inside `platform` at 40m, so that reserve is ArgoCD's.

## Install each morning

`make cluster` runs `make keda` after `make reloader` and before `make data`. To run it alone:

```bash
make keda
```

It reads the `keda` client ID from the persistent output, checks it is a GUID, then installs the chart. The [Makefile](../../Makefile) pins the chart in `KEDA_VERSION`. To upgrade, read the release notes for CRD changes, then change `KEDA_VERSION` and the version in the header of `values.yaml`.

## Proving it

P6-09 (#70): KEDA scales the worker on queue depth. This needs a live cluster after `make cluster`. The load test in [loadtest](../loadtest/README.md) creates the orders.

```bash
CTX=aks-v2-platform
K="kubectl --context $CTX"
$K -n platform get pods -l app.kubernetes.io/name=keda-operator
$K -n apps get scaledobject worker
$K -n apps get hpa keda-hpa-worker

# Hold the worker at zero so a backlog builds, then let KEDA see it.
$K -n apps annotate scaledobject worker autoscaling.keda.sh/paused-replicas=0 --overwrite
make loadtest DURATION=2m
$K -n apps annotate scaledobject worker autoscaling.keda.sh/paused-replicas-
$K -n apps get deploy worker -w
$K -n platform logs deploy/keda-operator --since=10m | grep -iE 'error|401|403' || echo "no auth errors"
```

Expect the ScaledObject `READY True` and `ACTIVE True` once messages wait, and `keda-hpa-worker` with `MAXPODS 5`. With about 360 messages waiting, the worker goes to 5 replicas within a minute or two of the pause lifting. It drains the queue and returns to 1 replica after the 300-second cooldown. `no auth errors` shows `id-keda` read the queue.

## Validate

```bash
helm template keda keda --repo https://kedacore.github.io/charts \
  --version 2.21.1 --namespace platform -f k8s/keda/values.yaml \
  --set-string podIdentity.azureWorkload.clientId=00000000-0000-0000-0000-000000000000 \
  | kubeconform -strict -ignore-missing-schemas -summary
```

kubeconform has no schema for the six CRDs the chart installs, so it skips them. The ScaledObject and TriggerAuthentication are checked against the CRD catalog in [apps](../apps/README.md#validate).
