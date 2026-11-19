# platform

**TL;DR:** Holds the daily stack: network, AKS, Service Bus and the rest. Apply it each morning and destroy it each evening.

## What it holds

- A resource group, `rg-aks-v2-platform`, in UK South.
- The VNet `vnet-aks-v2-platform` with a node subnet and a private endpoint subnet, each with an NSG, through [`../modules/network`](../modules/network/README.md). The address plan, including the pod and service CIDRs for AKS, is in that README.
- Private DNS zones for Key Vault and blob, linked to the VNet, also through the network module. ACR and Service Bus get none, since they have no private endpoints. The [network README](../modules/network/README.md#private-dns-zones) says why.
- The AKS cluster `aks-v2-platform`, through [`../modules/aks`](../modules/aks/README.md). Its node resource group is `rg-aks-v2-platform-nodes`.
- The Service Bus namespace and the `order-events` queue, through [`../modules/servicebus`](../modules/servicebus/README.md). Its firewall allows only the cluster's egress IP, so the namespace is created after the cluster.
- The Log Analytics workspace `log-aks-v2-platform`, with a 0.5 GB daily cap and the AKS control plane diagnostic setting, through [`../modules/loganalytics`](../modules/loganalytics/README.md). No Container Insights: New Relic does that job.
- Later: private endpoints, federated credentials and the other cluster-bound resources, through the modules in [`../modules`](../modules).

AKS creates its own node resource group. This root doesn't manage it, and it goes when the cluster goes.

After an apply, run `eval "$(tofu output -raw kubeconfig_command)"` to fetch an Entra ID kubeconfig, then `kubectl apply -k ../../k8s/node-pools` so user workloads have somewhere to run.

State lives in the `platform.tfstate` blob in the state account.

## Lifecycle

This root is destroyed every evening and applied every morning, so nothing in it can need to survive the night. Anything that does belongs in [`../persistent`](../persistent/README.md). Apply this root before `argocd`, and destroy `argocd` before this root.

## Init, plan and apply

You need `infra/backend.hcl`, generated from the bootstrap outputs. See [`../bootstrap`](../bootstrap/README.md).

```bash
cd infra/platform
export ARM_SUBSCRIPTION_ID=<subscription id>
tofu init -backend-config=../backend.hcl
tofu plan -out=platform.tfplan
tofu apply platform.tfplan
```

In the evening, after `argocd` is destroyed:

```bash
tofu destroy
```

Two variables are required. `servicebus_namespace_name` must be globally unique. `operator_object_ids` lists the people who apply by hand, who get AKS RBAC Cluster Admin. Find your ID with `az ad signed-in-user show --query id -o tsv`. Copy [`terraform.tfvars.example`](terraform.tfvars.example) to `terraform.tfvars`, which is gitignored, and set them there. Every other variable has a default, including the address ranges, and can be overridden in the same file.

The CI infra identity, `id-gh-infra`, gets AKS RBAC Cluster Admin too. Platform reads it by name from `rg-aks-v2-identity` in persistent, so apply persistent first. The admin list is fixed and doesn't follow whoever runs the apply, so a person and CI applying in turn don't swap each other's access.
