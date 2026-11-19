# platform

**TL;DR:** Holds the daily stack: network, AKS, Service Bus and the rest. Apply it each morning and destroy it each evening.

## What it holds

- A resource group, `rg-aks-v2-platform`, in UK South.
- Later: the VNet and subnets, private DNS zones, AKS, Service Bus, private endpoints, Log Analytics, federated credentials and the other cluster-bound resources, through the modules in [`../modules`](../modules).

AKS creates its own node resource group. This root doesn't manage it, and it goes when the cluster goes.

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

No variables are required. `location`, `resource_group_name`, `environment` and `owner` have defaults. To override them, use a gitignored `terraform.tfvars`.
