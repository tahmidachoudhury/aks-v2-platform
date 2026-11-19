# platform

**TL;DR:** Holds the daily stack: network, AKS, Service Bus and the rest. Apply it each morning and destroy it each evening.

## What it holds

- A resource group, `rg-aks-v2-platform`, in UK South.
- The VNet `vnet-aks-v2-platform` with a node subnet and a private endpoint subnet, each with an NSG, through [`../modules/network`](../modules/network/README.md). The address plan, including the pod and service CIDRs for AKS, is in that README.
- The AKS cluster `aks-v2-platform`, through [`../modules/aks`](../modules/aks/README.md). Its node resource group is `rg-aks-v2-platform-nodes`.
- Later: private DNS zones, Service Bus, private endpoints, Log Analytics, federated credentials and the other cluster-bound resources, through the modules in [`../modules`](../modules).

AKS creates its own node resource group. This root doesn't manage it, and it goes when the cluster goes.

After an apply, `tofu output -raw kubeconfig_command` prints the command that fetches an Entra ID kubeconfig.

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

No variables are required. All of them have defaults, including the address ranges. To override them, use a gitignored `terraform.tfvars`.
