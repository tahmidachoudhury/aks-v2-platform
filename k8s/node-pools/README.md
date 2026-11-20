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
| `general` | On-demand | `Standard_D2s_v6`, `Standard_D4s_v6`, `Standard_D2nls_v6`, `Standard_D4nls_v6` | 4 | Two 2 vCPU nodes or one 4 vCPU node |
| `spot` | Spot | `Standard_D2s_v6`, `Standard_D2nls_v6` | 3 | One 2 vCPU node |

All the sizes are D family, which has quota. The subscription can't run D2s_v6 or D4s_v6 in zone 1, so NAP uses the nls sizes there. The DSv4 family is blocked in the region, so it isn't listed. Karpenter picks the cheapest size that fits the pending pods, usually `Standard_D2s_v6`.

A system pool upgrade needs 2 spare standard vCPUs for its surge node. With `general` at its limit there are none. See the [quota notes](../../infra/modules/aks/README.md#quota).

## Zones

Both NodePools allow `uksouth-1`, `uksouth-2` and `uksouth-3`. Karpenter only spreads nodes across zones when the pods ask for it, so each Deployment that needs zone spread must set a `topologySpreadConstraints` on `topology.kubernetes.io/zone`.

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
