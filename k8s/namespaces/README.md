# namespaces

**TL;DR:** Three namespaces, `apps`, `data` and `platform`, all enforce the `restricted` Pod Security Standard, so each rejects a privileged pod. Each has a ResourceQuota and a LimitRange. The quotas request 2.5 CPUs and 8 GiB in total. That is sized against the `general` NodePool alone, after the AKS DaemonSets and about 1 CPU for ArgoCD, because most of these pods must not run on spot. KEDA and Reloader run inside `platform`, at 50m between them. The general pool's CPU is the binding limit: P6 keeps each service pod's CPU request at or under 40m, or lowers the HPA maxima. `make namespaces` applies them each morning after the node pools and before any Helm install. ArgoCD takes this over in P7.

Filled by P4-07 (#54).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `namespace-apps.yaml` | `Namespace` | `apps` | The nine services |
| `namespace-data.yaml` | `Namespace` | `data` | Postgres and Redis |
| `namespace-platform.yaml` | `Namespace` | `platform` | Traefik, cert-manager, ExternalDNS and External Secrets |
| `quota-<namespace>.yaml` | `ResourceQuota`, `LimitRange` | The namespace's name | Caps the namespace and gives containers default requests |
| `networkpolicy-<namespace>.yaml` | `NetworkPolicy` | `default-deny`, `allow-dns` | `apps` and `data` only. Blocks all traffic in and out, then lets every pod reach CoreDNS |

## Who owns the namespaces

Only this folder creates the three namespaces. Run Helm installs into `platform` without `--create-namespace`. Don't set `CreateNamespace=true` on ArgoCD Applications that target these namespaces. Otherwise a chart could create the namespace first without the Pod Security labels.

The workload identity federated credentials trust these ServiceAccounts, so the names are fixed (see [workload-identity](../../infra/modules/workload-identity/README.md#identities)):

| Namespace | ServiceAccount | Created by |
| --- | --- | --- |
| `apps` | `order-service`, `payment-service`, `shipping-service`, `worker` | The service manifests (P6-05) |
| `platform` | `external-dns` | The ExternalDNS chart, by its default name |
| `platform` | `external-secrets` | The External Secrets chart, by its default name |
| `platform` | `keda-operator` | The KEDA chart, by its default name |

This folder creates no ServiceAccounts, since a chart refuses to install over a ServiceAccount it didn't create.

## Pod Security

Each namespace sets all three [Pod Security Admission](https://kubernetes.io/docs/concepts/security/pod-security-admission/) modes to `restricted`:

| Label | Effect |
| --- | --- |
| `pod-security.kubernetes.io/enforce: restricted` | The API server rejects a pod that breaks the standard |
| `pod-security.kubernetes.io/audit: restricted` | Breaches are noted in the audit log |
| `pod-security.kubernetes.io/warn: restricted` | `kubectl` and Helm print a warning. This also covers Deployments and other workload objects, whose pods enforce would otherwise reject silently |

No version is pinned, so each label follows the cluster's Kubernetes version.

No namespace drops to `baseline`. A pod must:

- Run as a non-root user: `runAsNonRoot: true`, and a numeric `runAsUser` if the image's user is a name.
- Set `seccompProfile.type: RuntimeDefault`.
- Set `allowPrivilegeEscalation: false` and drop all capabilities. Only `NET_BIND_SERVICE` may be added back.
- Not be privileged, and use no host namespaces, host ports or `hostPath` volumes.

Per namespace:

- `apps`: the services are Go binaries on ports above 1024, so they need nothing special. P6 sets the security context on each Deployment.
- `data`: the official `postgres` and `redis` images start as root and drop to user 999. The StatefulSets start them as 999 directly with `runAsUser: 999`, `runAsGroup: 999` and `fsGroup: 999`, so the disk is writable. Postgres then skips its `chown` step. Point `PGDATA` at a subdirectory of the mount, since the disk's root holds `lost+found` and `initdb` needs an empty directory.
- `platform`: current Traefik, cert-manager, ExternalDNS and External Secrets charts set a `restricted` security context by default. Traefik listens on 8000 and 8443 inside the pod, so it needs no extra capability. Check each chart before installing it, as below.

To check a chart before you install it, render it and dry-run it against the namespace:

```bash
helm template <release> <chart> -n platform -f <values> \
  | kubectl apply --dry-run=server -n platform -f -
```

Pods that break `restricted` fail with `violates PodSecurity "restricted:latest"`. Workload objects only warn. If a chart can't comply, set its security context values. Lower a namespace to `baseline` only if that fails, and record why here.

## NetworkPolicies

`apps` and `data` start from default deny, added by P6-08 (#69). `default-deny` selects every pod and lists both directions with no rules, so nothing gets in or out. `allow-dns` lets every pod reach CoreDNS (`k8s-app: kube-dns` in `kube-system`) on 53, UDP and TCP. Every other path is an allow rule next to the workload that needs it: each service's base in [apps](../apps/README.md#networkpolicies), Postgres and Redis in [data](../data/README.md#networkpolicies), the snapshot CronJob in [backup](../backup/README.md) and the migration Job in [migrations](../migrations/README.md).

`platform` has no policy yet, so its components talk freely. That is a follow-up.

They live here, not with the workloads, because they belong to the namespace: `make namespaces` applies them each morning before anything runs in it. Cilium enforces them.

## Quotas

### Capacity

User workloads run on Node Auto Provisioning nodes, since the system pool is tainted `CriticalAddonsOnly`. See [node-pools](../node-pools/README.md#sizes-and-limits).

Only the `general` NodePool counts here. `spot` takes only pods that tolerate eviction, which means the worker at most. Postgres, Redis, the scheduler, api-gateway and every `platform` component must stay off it. Spot's one 2 vCPU node, about 1,700m for pods, is extra room for the worker and isn't counted.

| `general` NodePool | CPU |
| --- | --- |
| `limits.cpu` | 4 vCPUs |
| For pods, after the AKS DaemonSets (about 180m a node) | 3,440m on two 2 vCPU nodes, the only shape since P6-04 |
| Kept for ArgoCD (P7), outside these namespaces | About 1,000m |
| Left for `apps`, `data` and `platform` | About 2,440m |

The quotas total 2,500m. If all three namespaces use their full CPU quota and nothing runs on spot, demand is about 60m more than the two nodes hold. The quota admits the last pod or two, but they stay `Pending`. `general` offers no 4 vCPU size, which would have fitted, because the five multi-replica services in `apps` need two nodes ([node-pools](../node-pools/README.md#sizes-and-limits)). A worker on spot takes its requests off `general`. If ArgoCD requests more than 1 CPU, the shortfall grows by the difference.

Memory depends on the size Karpenter picks. `Standard_D2s_v6` has 8 GiB per 2 vCPUs. With two of those, `general` holds about 11 GiB for pods, against 8 GiB of quota requests plus ArgoCD's. `Standard_D2nls_v6` has half that memory and is the only size in zone 1. With one of each, pods go `Pending` on memory before CPU.

### Sizes

| Namespace | `requests.cpu` | `requests.memory` | `limits.memory` | Pods | PVCs | Storage | LoadBalancers |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `apps` | 1250m | 4Gi | 10Gi | 40 | 0 | n/a | 0 |
| `data` | 750m | 2Gi | 6Gi | 10 | 4 | 64Gi | 0 |
| `platform` | 500m | 2Gi | 4Gi | 20 | 0 | n/a | 1 |
| Total | 2500m | 8Gi | 20Gi | | | | |

### CPU budget

The binding limit is the `general` pool's CPU. The quotas only split it between the namespaces. Raising a quota without adding capacity only moves where pods go `Pending`.

- `apps`: P6 sets each service pod's CPU request at 40m or less. The [Autoscaling](../../docs/architecture-decisions.md#autoscaling) maximums add up to 26 pods, and 26 x 40m is 1,040m. That leaves 210m for one service's rolling update at a time and cert-manager's HTTP-01 solver pod (10m). The LimitRange default request is 40m to match. HPAs scale at 70% of the request, so at 40m a pod scales out at 28m.
- If P6 needs larger requests, it lowers the HPA maximums so the sum of `maxReplicas` x request stays at or under about 1,050m. At 60m, for example, 17 pods fit, not 26. The quota refuses pods past its limit and the HPA reports `FailedCreate`, so maximums above the budget never take effect.
- `data`: Postgres requests up to 250m and Redis up to 100m. The restore drill's second Postgres then fits: 600m of 750m.
- `platform`: the charts request 230m: Traefik 2 x 50m, cert-manager 3 x 10m, External Secrets 4 x 10m, ExternalDNS 10m, Reloader 10m, and KEDA's operator and metrics server 2 x 20m. That is 13 of 20 pods. cert-manager's startupapicheck Job adds 50m and a pod while it runs. The full table is in [apps](../apps/README.md#platform-quota).

### Memory and other limits

- `apps` `limits.memory` is 10Gi. The 26 pods at the HPA maximums plus a 25% rolling surge (9 more), at the 256Mi default limit, come to 8,960Mi.
- `data` `limits.memory` is 6Gi, so two Postgres containers and Redis all fit at the 2Gi per-container maximum during the restore drill.
- Memory requests decide where pods fit. Limits sit above them so pods can burst.
- There is no `limits.cpu`. A CPU quota forces a CPU limit on every container, and a CPU limit throttles a pod even when the node is idle.
- Only `data` has PVCs, since only Postgres and Redis keep state. Only Traefik has a LoadBalancer Service, since a second one would add a public IP.

A quota on `requests.memory` and `limits.memory` rejects any pod that leaves either out. The LimitRange fills them in.

### LimitRange

| Namespace | Default CPU request | Default memory request | Default memory limit | Max memory limit |
| --- | --- | --- | --- | --- |
| `apps` | 40m | 64Mi | 256Mi | 1Gi |
| `data` | 100m | 128Mi | 512Mi | 2Gi |
| `platform` | 50m | 64Mi | 256Mi | 1Gi |

The defaults apply only to a container that sets nothing. If a container sets a memory request above the default limit, it must also set its own limit. Otherwise the API server rejects it. Set both.

## Morning apply

After the platform apply and `kubectl apply -k k8s/node-pools`, and before any Helm install into `platform`:

```bash
kubectl apply -k k8s/namespaces
```

## Acceptance test

This needs a live cluster. After the apply, every test is a server-side dry run, so it creates nothing and needs no node. Run from the repo root.

```bash
kubectl apply -k k8s/namespaces
kubectl get ns apps data platform --show-labels
kubectl get resourcequota,limitrange -n apps
kubectl get resourcequota,limitrange -n data
kubectl get resourcequota,limitrange -n platform
```

P4-07 (#54): a privileged pod is rejected in each namespace.

```bash
for ns in apps data platform; do
  kubectl run priv-test -n "$ns" --image=registry.k8s.io/pause:3.10 --dry-run=server \
    --overrides='{"spec":{"containers":[{"name":"priv-test","image":"registry.k8s.io/pause:3.10","securityContext":{"privileged":true}}]}}'
done
```

Each should fail with `pods "priv-test" is forbidden: violates PodSecurity "restricted:latest": privileged (...)`, followed by the other rules it breaks.

A pod that meets `restricted` is admitted, and the LimitRange fills in its resources:

```bash
kubectl run ok-test -n apps --image=registry.k8s.io/pause:3.10 --dry-run=server -o jsonpath='{.spec.containers[0].resources}{"\n"}' \
  --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65535,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"ok-test","image":"registry.k8s.io/pause:3.10","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'
```

Expect `{"limits":{"memory":"256Mi"},"requests":{"cpu":"40m","memory":"64Mi"}}`.

The quota blocks a PVC in `apps`:

```bash
kubectl create -n apps --dry-run=server -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: quota-test
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
EOF
```

Expect a Forbidden error with `exceeded quota: apps` and `limited: persistentvolumeclaims=0`.

## Validate

```bash
kustomize build k8s/namespaces | kubeconform -strict -summary
```
