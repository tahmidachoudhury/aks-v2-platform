# aks

**TL;DR:** One AKS cluster on Kubernetes 1.37, Free tier, with three `Standard_D2nls_v6` system nodes, one in each zone. It uses Azure CNI Overlay with Cilium, Entra ID sign-in with Azure RBAC, no local accounts, and the OIDC issuer with Workload Identity. Node Auto Provisioning adds user nodes, and egress leaves through the load balancer's outbound rules. The system nodes cost about £60 a month on the 9am to 5pm weekday schedule.

The `platform` root calls this module, so the cluster is destroyed every evening and created every morning. Filled by P3-07 (#33). AKS creates its own node resource group, `rg-aks-v2-platform-nodes`. This module doesn't manage that group, and it goes when the cluster goes.

## Version

`kubernetes_version` holds a minor version, `1.37`. AKS picks the latest patch of that minor at create time, so each morning's cluster gets the newest patch with no code change. 1.37 has been GA on AKS since the 2026-10-25 release. 1.36 is the default for new clusters, not the latest.

No automatic upgrade channel is set. The cluster lives for eight hours, and the next cluster uses the newest patch and node image anyway.

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

AKS refuses system pools on sizes with fewer than 2 vCPUs or less than 4 GiB ([AKS quotas and SKUs](https://learn.microsoft.com/en-us/azure/aks/quotas-skus-regions)), so `Standard_D2nls_v6` meets the floor. The [system node pools](https://learn.microsoft.com/en-us/azure/aks/use-system-pools) page says "at least 4 vCPUs", so the two pages disagree. If AKS rejects the size at create time, change `system_node_vm_size` in platform to `Standard_D2ns_v6` (2 vCPUs, 8 GiB, £0.1308 an hour) or a 4 vCPU size.

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

## Egress

Decided in P3-05 (#31). Also recorded in [architecture decisions](../../../docs/architecture-decisions.md#egress).

**Decision:** `outbound_type = "loadBalancer"` with one managed outbound public IP. No NAT Gateway and no firewall.

Both subnets set `default_outbound_access_enabled = false`, so a node gets no implicit internet access from Azure. The outbound rules of the cluster's Standard Load Balancer are the only way out. AKS creates that load balancer, its outbound rule and the public IP in the node resource group.

### Options

Prices are US dollars at UK South retail rates, checked on 19 November 2026. A full month is 730 hours. The schedule is 176 hours. Internet data transfer is billed the same under all three, so it is left out.

| Option | Cost, full month | Cost, on the schedule | SNAT | Notes |
| --- | --- | --- | --- | --- |
| Load balancer outbound rules (chosen) | About $4 extra: one public IP at $0.005 an hour. Data processed at $0.005 per GB | About $1 extra | Ports split up front per node. See below | Traefik (P4-03) needs the same Standard Load Balancer anyway. Its rule charge, $0.025 an hour for the first five rules, covers the outbound rule too |
| NAT Gateway | About $33 for the gateway at $0.045 an hour, plus $3.65 for its public IP, plus $0.045 per GB | About $9 | 64,512 ports per public IP, handed out on demand to any VM in the subnet. Up to 16 IPs | Better SNAT behaviour under load. The brief asks us to avoid it. The load balancer still exists for Traefik |
| User-defined routing through Azure Firewall | Basic: about $288 at $0.395 an hour, plus $0.065 per GB. Standard: about $913 at $1.25 an hour | Basic: about $70 | The firewall's public IPs do the SNAT | Filters egress by FQDN, which nothing else here can do. Needs a firewall subnet, a route table and more public IPs. Basic alone uses most of the £100 budget |

### SNAT ports

- One managed outbound IP gives 64,000 SNAT ports.
- `outbound_ports_allocated` is left at its default of 0. Azure then sets the ports per node from the backend pool size: 1,024 per node for up to 50 nodes, 512 for 51 to 100, and fewer as the pool grows.
- This cluster tops out at about 7 nodes: 3 system, 1 surge, 2 on-demand and 1 spot from Node Auto Provisioning. That is about 7,000 of the 64,000 ports.
- 1,024 ports allow 1,024 concurrent connections from one node to the same destination IP and port. Few connections use SNAT, since services reach Postgres and Redis inside the cluster, and most Azure services over private endpoints. Service Bus Standard has no private endpoint, so its AMQP connections do use SNAT, a handful per pod.
- If the load balancer's SNAT metrics show exhaustion, set `outbound_ports_allocated` explicitly. Ports per node times the most nodes the cluster can reach, surge included, must stay under 64,000 per outbound IP. 8,000 ports across 8 nodes fits one IP.

Idle outbound flows close after 30 minutes, the default `idle_timeout_in_minutes`.

### Changing it later

Node Auto Provisioning doesn't allow a change of outbound type after the cluster is created. The cluster is rebuilt every morning, so a change takes effect at the next create.

## Node Auto Provisioning

Added in P3-08 (#34). NAP is the AKS-managed Karpenter. It creates user nodes when pods are pending and removes them when they are not needed.

```hcl
node_provisioning_profile {
  mode               = "Auto"
  default_node_pools = "None"
}
```

- `mode = "Auto"` turns NAP on. It is the azurerm 4.81 argument for the AKS `nodeProvisioningProfile.mode` field.
- `default_node_pools = "None"` stops AKS creating its `default` and `system-surge` NodePools. Ours are in [`k8s/node-pools`](../../../k8s/node-pools/README.md), with the quota limits these defaults don't have. ArgoCD applies them from P7.

NAP needs the following, and this cluster meets each one:

- A managed identity, not a service principal. This cluster uses a system-assigned identity.
- Azure CNI in Overlay or node subnet mode. Not kubenet, and not Calico network policy. Microsoft recommends Overlay with the Cilium data plane, which this cluster uses.
- A Standard Load Balancer when the cluster sits in its own VNet.
- Read and join rights on the node subnet for the cluster identity. Network Contributor on `snet-aks-nodes` covers both.
- Linux only, IPv4 only.

A NAP cluster can't be stopped, which doesn't matter because this one is destroyed instead. Its outbound type can't change after create.

To prove NAP works, use a live cluster: apply the NodePools, scale a test deployment, and watch a node appear. The steps are in the [node-pools README](../../../k8s/node-pools/README.md#acceptance-test).

## Add-ons

Added in P3-09 (#35). The `storage_profile` block sets each CSI driver on or off explicitly, so the plan shows the intent.

| Add-on | Setting | Why |
| --- | --- | --- |
| Azure Disk CSI driver | `disk_driver_enabled = true` | Backs the Premium SSD volumes for Postgres and Redis. On by default in AKS. Set here so it shows |
| Snapshot controller | `snapshot_controller_enabled = true` | Serves VolumeSnapshots, for the Postgres backup and restore test. On by default |
| Azure Files CSI driver | `file_driver_enabled = false` | Nothing here uses Azure Files. Off saves a DaemonSet pod on every node |
| Azure Blob CSI driver | `blob_driver_enabled = false` | Nothing here mounts Blob storage. Off by default |
| Key Vault secrets provider | Not enabled | P0-10 chose External Secrets Operator, not the Secrets Store CSI Driver. See [Secrets delivery](../../../docs/architecture-decisions.md#secrets-delivery) |

The StorageClass and VolumeSnapshotClass are Kubernetes manifests for a later card.

To check the add-ons on a live cluster:

```bash
kubectl get csidrivers
kubectl get pods -n kube-system -l app=csi-azuredisk-node -o wide
kubectl get crd volumesnapshots.snapshot.storage.k8s.io volumesnapshotclasses.snapshot.storage.k8s.io
kubectl get pods -n kube-system | grep -i snapshot
```

The acceptance test is a `csi-azuredisk-node` pod on every node, and `disk.csi.azure.com` in the CSI driver list. There should be no `file.csi.azure.com`, and no `secrets-store` pods. AKS may run the snapshot controller in the managed control plane, so the last command may show nothing. The VolumeSnapshot CRDs show that it is installed.

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

In US dollars that is about $80: $0.134 an hour per node and $12.35 a month per disk. The Free tier has no uptime SLA. The load balancer and its egress IP add about $5 on the schedule, as costed under [Egress](#egress).
