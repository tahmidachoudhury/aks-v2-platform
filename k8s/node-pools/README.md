# node-pools

**TL;DR:** The Node Auto Provisioning (NAP) NodePools for user workloads: `general` for on-demand nodes, capped at 4 vCPUs, and `spot` for eviction-tolerant workloads, capped at 3 vCPUs. Both allow zones 1 to 3 and consolidate empty or underused nodes. `make node-pools` applies them each morning after the platform apply. Without them only critical add-ons can schedule, because the system pool is tainted. ArgoCD takes this over in P7.

NAP is the AKS-managed Karpenter. OpenTofu turns it on with no default NodePools ([aks module](../../infra/modules/aks/README.md#node-auto-provisioning)), so these manifests are the only ones. Filled by P3-08 (#34).

## Resources

| File | Kind | Name | Purpose |
| --- | --- | --- | --- |
| `aksnodeclass.yaml` | `AKSNodeClass` | `general` | Azure Linux image, 64 GB managed OS disk, cost tags. No `vnetSubnetID`, so nodes join `snet-aks-nodes` |
| `nodepool-general.yaml` | `NodePool` | `general` | On-demand nodes. Takes any pod that doesn't tolerate spot |
| `nodepool-spot.yaml` | `NodePool` | `spot` | Spot nodes, tainted `kubernetes.azure.com/scalesetpriority=spot:NoSchedule` |

## Sizes and limits

The quota sets the limits. UK South allows the subscription 10 standard vCPUs and 3 spot vCPUs. The system pool takes 6 standard vCPUs.

| NodePool | Capacity | Sizes | `limits.cpu` | Most nodes |
| --- | --- | --- | --- | --- |
| `general` | On-demand | `Standard_D2s_v6`, `Standard_D2nls_v6` | 4 | Two 2 vCPU nodes |
| `spot` | Spot | `Standard_D2s_v6`, `Standard_D2nls_v6` | 3 | One 2 vCPU node |

All the sizes are D family, which has quota. The subscription can't run D2s_v6 in zone 1, so NAP uses D2nls_v6 there. The DSv4 family is blocked in the region, so it isn't listed. Karpenter picks the cheapest size that fits the pending pods. That is `Standard_D2s_v6` in zones 2 and 3, at £0.088 an hour on demand against £0.1007 for `Standard_D2nls_v6`, and £0.0155 against £0.0177 on spot (Linux retail prices for UK South, checked 25 November 2026). In zone 1 it is `Standard_D2nls_v6`, the only size there.

`general` offers no 4 vCPU size, so its limit always holds two nodes, which five services in `apps` require ([apps](../apps/README.md#disruption-and-spread)). A 4 vCPU node uses the whole limit by itself, so with one there can never be a second `general` node. With `Standard_D4s_v6` on offer, the first node in the P6 live run took the whole limit, and a second node never came up. Offering a 4 vCPU size next to the 2 vCPU ones isn't the problem in itself: Karpenter subtracts the largest size on offer from the remaining limit only while it plans NodeClaims within one scheduling pass, and counts a node at its real size once it launches.

A system pool upgrade needs 2 spare standard vCPUs for its surge node. With `general` at its limit there are none. See the [quota notes](../../infra/modules/aks/README.md#quota).

## Zones

Both NodePools allow `uksouth-1`, `uksouth-2` and `uksouth-3`. Karpenter only spreads nodes across zones when the pods require it, with a `topologySpreadConstraints` on `topology.kubernetes.io/zone` and `whenUnsatisfiable: DoNotSchedule`. Nothing does today. `general`'s limit holds two nodes but can't choose their zones, so a hard zone rule could leave pods `Pending` with no node NAP may add. The five multi-replica services in `apps` require two nodes instead, and only prefer two zones ([apps](../apps/README.md#disruption-and-spread)). A `ScheduleAnyway` preference is met on whatever nodes exist and never adds a node by itself. When NAP adds one for another reason, such as the five's node rule, Karpenter tries the preference first ([Karpenter scheduling](https://karpenter.sh/docs/concepts/scheduling/#preferences)), so the second node usually lands in another zone. Nothing guarantees it.

## Disruption

- `consolidationPolicy: WhenEmptyOrUnderutilized` with `consolidateAfter: 1m`. NAP removes empty nodes and repacks underused ones onto fewer or cheaper nodes.
- A budget of one node at a time, so consolidation never drains two nodes at once.
- `expireAfter: Never`. Nodes live no longer than the cluster, which is destroyed every evening.

## Spot

The `spot` NodePool has a higher weight than `general`, so a pod that tolerates spot goes to spot first. Azure can evict a spot node with 30 seconds' notice.

To use it, a workload adds:

```yaml
tolerations:
  - key: kubernetes.azure.com/scalesetpriority
    operator: Equal
    value: spot
    effect: NoSchedule
```

Use spot only for workloads that lose nothing on a restart, such as the worker, whose Service Bus messages return to the queue if it dies mid-message. Postgres, Redis, the scheduler and api-gateway must not run on spot.

## Acceptance test

This needs a live cluster. ArgoCD applies the manifests from P7. Before that, apply them by hand once to test, then delete them:

```bash
kubectl apply -k k8s/node-pools
kubectl create deployment nap-test --image=mcr.microsoft.com/oss/kubernetes/pause:3.6 --replicas=3
kubectl set resources deployment nap-test --requests=cpu=500m,memory=512Mi
kubectl get nodeclaims -w
kubectl get nodes -L karpenter.sh/nodepool,topology.kubernetes.io/zone,node.kubernetes.io/instance-type
```

A NodeClaim should appear within a minute or two, then a node labelled `karpenter.sh/nodepool=general`. Delete the deployment with `kubectl delete deployment nap-test`, and the node should go after `consolidateAfter`. Then run `kubectl delete -k k8s/node-pools`, so ArgoCD later takes them over cleanly.

## Validate

```bash
kustomize build k8s/node-pools | kubeconform -strict -summary \
  -schema-location default \
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
```
