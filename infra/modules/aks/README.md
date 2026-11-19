# aks

**TL;DR:** One AKS cluster on Kubernetes 1.37, Free tier, with three `Standard_D2nls_v6` system nodes, one in each zone. It uses Azure CNI Overlay with Cilium, Entra ID sign-in with Azure RBAC, no local accounts, and the OIDC issuer with Workload Identity. The system nodes cost about £60 a month on the 9am to 5pm weekday schedule.

The `platform` root calls this module, so the cluster is destroyed every evening and created every morning. Filled by P3-07 (#33). AKS creates its own node resource group, `rg-aks-v2-platform-nodes`. This module doesn't manage that group, and it goes when the cluster goes.

## Version

`kubernetes_version` holds a minor version, `1.37`. AKS picks the latest patch of that minor at create time, so each morning's cluster gets the newest patch with no code change. 1.37 has been GA on AKS since the 2026-10-25 release. 1.36 is the default for new clusters, not the latest.

No automatic upgrade channel is set. The cluster lives for eight hours, and the next cluster is built on the newest patch and node image anyway.

## System node pool

| Setting | Value | Why |
| --- | --- | --- |
| VM size | `Standard_D2nls_v6`, 2 vCPUs, 4 GiB | Cheapest size this subscription can run in all three zones. See below |
| Nodes | 3, in zones 1, 2 and 3 | One per zone, as the brief asks |
| `only_critical_addons_enabled` | `true` | Taints the nodes `CriticalAddonsOnly=true:NoSchedule`. User workloads go to Node Auto Provisioning nodes |
| OS | Azure Linux | Smaller image and lower memory use than Ubuntu, which matters on 4 GiB nodes |
| OS disk | Managed, 64 GB | v6 sizes have no temp disk, so ephemeral OS disks are not possible |
| `max_surge` | `1` | One extra node during an upgrade. See [Quota](#quota) |

### Why D2nls_v6 and not D2s_v6

`Standard_D2s_v6` is cheaper, at £0.088 an hour against £0.1007. But `az vm list-skus` shows it as `NotAvailableForSubscription` in zone 1 of UK South, so a pool across zones 1 to 3 can't use it. The DSv4 family is blocked in the whole region for this subscription. Of the sizes with 2 or 4 vCPUs, only the Dnsv6 and Dnlsv6 families are offered in all three zones.

AKS refuses system pools on sizes with fewer than 2 vCPUs or less than 4 GiB ([AKS quotas and SKUs](https://learn.microsoft.com/en-us/azure/aks/quotas-skus-regions)), so `Standard_D2nls_v6` meets the floor. The [system node pools](https://learn.microsoft.com/en-us/azure/aks/use-system-pools) page says "at least 4 vCPUs", so the two pages disagree. If AKS rejects the size at create time, use `Standard_D2ns_v6` (2 vCPUs, 8 GiB, £0.1308 an hour) or a 4 vCPU size. Either is a change to `system_node_vm_size` in platform.

The nodes carry only the critical add-ons: CoreDNS, Cilium, konnectivity, metrics-server and the CSI drivers. 4 GiB holds them.

### OS disk

AKS gives a 2 vCPU size a 128 GB P10 managed OS disk by default ([AKS storage concepts](https://learn.microsoft.com/en-us/azure/aks/concepts-storage)). This size supports premium storage, so the disk is Premium SSD LRS. Setting 64 GB makes it a P6. The disk holds no data that outlives the node.

## Quota

UK South allows the subscription 10 standard vCPUs in total, plus 3 spot vCPUs. The spot quota is separate.

| Use | vCPUs |
| --- | --- |
| System pool | 6 |
| Node Auto Provisioning, on-demand | Up to 4 (P3-08) |
| Total standard | 10 |
| Node Auto Provisioning, spot | Up to 3, from the spot quota |

An in-place upgrade of the system pool adds one 2 vCPU surge node. That fits only while Node Auto Provisioning uses 2 vCPUs or fewer. The daily rebuild is the normal upgrade path. Before an in-place upgrade, scale user workloads down until it fits, or destroy and apply the root instead.

Changing the VM size, OS disk or zones of the system pool replaces the whole cluster. Rotating through a temporary pool would need 6 more vCPUs, which the quota doesn't have.

## Networking

- `network_plugin = "azure"` with `network_plugin_mode = "overlay"`. Nodes take IPs from `snet-aks-nodes`. Pods take IPs from the pod CIDR, outside the VNet.
- `network_data_plane = "cilium"` and `network_policy = "cilium"`. Microsoft recommends Overlay with Cilium for Node Auto Provisioning. Cilium also enforces NetworkPolicy without a separate engine.
- The pod CIDR, service CIDR and DNS service IP come from platform variables: `pod_cidr`, `service_cidr` and `dns_service_ip`. The address plan is in the [network README](../network/README.md#address-plan).

The cluster joins nodes and load balancer frontends to a subnet it doesn't own, so its identity has Network Contributor on `snet-aks-nodes`. The identity is system-assigned, so the role assignment can only be made once the cluster exists. The create doesn't need it. The load balancer for a `LoadBalancer` Service and the first Node Auto Provisioning node do need it, and both come later, after ArgoCD.

## Identity and access

- Control plane identity: system-assigned. Nothing here needs a user-assigned one.
- Sign-in: Entra ID, with `azure_rbac_enabled = true`. Kubernetes authorisation comes from Azure role assignments.
- `local_account_disabled = true`. There is no admin kubeconfig with a client certificate.
- Whoever runs the apply gets "Azure Kubernetes Service RBAC Cluster Admin" on the cluster. Once a GitHub Actions identity applies the root, grant your own user too.
- `oidc_issuer_enabled` and `workload_identity_enabled` are on. Federated credentials use the issuer URL, which changes with each new cluster, so they stay in platform.

Connect with the `kubeconfig_command` output. It needs `kubelogin`:

```bash
$(tofu -chdir=infra/platform output -raw kubeconfig_command)
kubectl get nodes -L topology.kubernetes.io/zone
```

The acceptance test is three system nodes, with the zones `uksouth-1`, `uksouth-2` and `uksouth-3`.

## Outputs

| Output | Value |
| --- | --- |
| `name` | Cluster name |
| `id` | Cluster ID |
| `oidc_issuer_url` | OIDC issuer URL, for federated credentials |
| `kubelet_identity_object_id` | Kubelet identity, for AcrPull on the registry (P3-11) |
| `node_resource_group` | The resource group AKS creates for nodes, disks and the load balancer |

The platform root also outputs `kubeconfig_command`. No output holds a kubeconfig or a credential.

## Cost

On the schedule of 176 hours a month (22 weekdays of 8 hours), at Azure retail prices for UK South checked on 19 November 2026:

| Item | Rate | Monthly |
| --- | --- | --- |
| 3 x `Standard_D2nls_v6` | £0.1007 an hour each | £53.17 |
| 3 x P6 OS disks | £9.29 a month each, billed by the hour | £6.72 |
| Control plane, Free tier | £0 | £0 |
| Total | | About £60 |

In US dollars that is about $80: $0.134 an hour per node and $12.35 a month per disk. The Free tier has no uptime SLA.
