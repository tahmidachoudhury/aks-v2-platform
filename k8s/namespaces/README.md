# namespaces

**TL;DR:** Three namespaces, `apps`, `data` and `platform`, all enforce the `restricted` Pod Security Standard, so each rejects a privileged pod. Each has a ResourceQuota and a LimitRange. The quotas request 4 CPUs and 8 GiB in total. That fits the roughly 5 CPUs Node Auto Provisioning can give user workloads and leaves about 1 CPU for ArgoCD and the rest. Apply by hand each morning after the node pools and before any Helm install (`kubectl apply -k k8s/namespaces`). ArgoCD takes this over in P7.

Filled by P4-07 (#54).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `namespace-apps.yaml` | `Namespace` | `apps` | The nine services |
| `namespace-data.yaml` | `Namespace` | `data` | Postgres and Redis |
| `namespace-platform.yaml` | `Namespace` | `platform` | Traefik, cert-manager, ExternalDNS and External Secrets |
| `quota-<namespace>.yaml` | `ResourceQuota`, `LimitRange` | The namespace's name | Caps the namespace and gives containers default requests |

## Who owns the namespaces

Only this folder creates the three namespaces. Run Helm installs into `platform` without `--create-namespace`. Don't set `CreateNamespace=true` on ArgoCD Applications that target these namespaces. Otherwise a chart could create the namespace first without the Pod Security labels.

The workload identity federated credentials trust these ServiceAccounts, so the names are fixed (see [workload-identity](../../infra/modules/workload-identity/README.md#identities)):

| Namespace | ServiceAccount | Created by |
| --- | --- | --- |
| `apps` | `order-service`, `payment-service`, `shipping-service`, `worker` | The service manifests (P6-05) |
| `platform` | `external-dns` | The ExternalDNS chart, by its default name |
| `platform` | `external-secrets` | The External Secrets chart, by its default name |

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

## Quotas

### Capacity

User workloads run on Node Auto Provisioning nodes, since the system pool is tainted `CriticalAddonsOnly`. See [node-pools](../node-pools/README.md#sizes-and-limits).

| NodePool | vCPUs | CPU for pods |
| --- | --- | --- |
| `general` | 4 | About 3.4 to 3.7. A 2 vCPU node gives 1,900m allocatable, and the AKS DaemonSets request about 180m of it |
| `spot` | 2, one node | About 1.7, and only for pods that tolerate spot |
| Total | 6 | About 5 |

Memory depends on the size Karpenter picks. `Standard_D2s_v6` and `Standard_D4s_v6` have 8 GiB per 2 vCPUs and are the cheapest, so Karpenter usually picks them. Three such 2 vCPU nodes give about 16 GiB for pods, twice the quotas' 8 GiB of requests. The `nls` sizes have half that memory. If they're all Karpenter can get, as in zone 1, memory runs out before CPU.

### Sizes

| Namespace | `requests.cpu` | `requests.memory` | `limits.memory` | Pods | PVCs | Storage | LoadBalancers |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `apps` | 2 | 4Gi | 8Gi | 40 | 0 | n/a | 0 |
| `data` | 1 | 2Gi | 4Gi | 10 | 4 | 64Gi | 0 |
| `platform` | 1 | 2Gi | 4Gi | 20 | 0 | n/a | 1 |
| Total | 4 | 8Gi | 16Gi | | | | |

- `apps`: the [Autoscaling](../../docs/architecture-decisions.md#autoscaling) maximums add up to 26 pods, and a rolling update adds 25%. At the default request of 50m and 64Mi each, that is under 2 CPUs and 4 GiB.
- `data`: Postgres and Redis, plus a second Postgres for the snapshot restore drill. Storage covers the 20Gi Postgres PVC, the 10Gi Redis PVC and a 20Gi restore.
- `platform`: about ten pods across four components, each needing tens of millicores.
- The 4 requested CPUs leave about 1 CPU for other namespaces, such as ArgoCD and KEDA.
- `limits.memory` is twice `requests.memory`, so pods can burst. Requests decide where pods fit. Limits only cap them.
- There is no `limits.cpu`. A CPU quota forces a CPU limit on every container, and a CPU limit throttles a pod even when the node is idle.
- Only `data` has PVCs, since only Postgres and Redis keep state. Only Traefik has a LoadBalancer Service, since a second one would add a public IP.

A quota on `requests.memory` and `limits.memory` rejects any pod that leaves either out. The LimitRange fills them in.

### LimitRange

| Namespace | Default CPU request | Default memory request | Default memory limit | Max memory limit |
| --- | --- | --- | --- | --- |
| `apps` | 50m | 64Mi | 256Mi | 1Gi |
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

Expect `{"limits":{"memory":"256Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}`.

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
