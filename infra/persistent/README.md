# persistent

**TL;DR:** Holds what must survive the nightly teardown: the DNS zone, Key Vault and ACR. Never destroy this root.

## What it holds

- A resource group, `rg-aks-v2-persistent`, in UK South. `prevent_destroy` is set on it, so no plan can delete it by accident.
- The container registry (P3-10), through [`../modules/acr`](../modules/acr/README.md). Basic SKU, Entra ID access only.
- The Key Vault and its four app secrets (P3-13), through [`../modules/keyvault`](../modules/keyvault/README.md). Secret values never reach state.
- Later: the public DNS zone (P3-15), through the module in [`../modules`](../modules).

State lives in the `persistent.tfstate` blob in the state account.

## What belongs here

Put a resource here if recreating it every day would break something. The zone would get new name servers, the vault name stays reserved after a delete, and the registry would lose its images.

Any managed identity whose client ID is referenced from Git belongs here too. This covers ExternalDNS, External Secrets and the per-service workload identities from P3-16 and P3-18, whose client IDs appear in manifests that ArgoCD applies. A recreated identity gets a new client ID, but Git still holds the old one. Their federated credentials depend on the cluster's OIDC issuer, which changes with each new cluster, so those stay in `platform`.

## Lifecycle

Apply once, after `bootstrap`, and again when this root changes. Never run `tofu destroy` here. To tear the project down for good, first remove `prevent_destroy` on purpose.

## Init, plan and apply

Needs OpenTofu 1.11 or later, for the ephemeral and write-only values in the Key Vault module. Needs `infra/backend.hcl`, generated from the bootstrap outputs. See [`../bootstrap`](../bootstrap/README.md).

```bash
cd infra/persistent
export ARM_SUBSCRIPTION_ID=<subscription id>
tofu init -backend-config=../backend.hcl
tofu plan -out=persistent.tfplan
tofu apply persistent.tfplan
```

`acr_name` and `key_vault_name` are required, because both names are globally unique. Copy [`terraform.tfvars.example`](terraform.tfvars.example) to `terraform.tfvars`, which is gitignored, and set them. `location`, `resource_group_name`, `environment` and `owner` have defaults.
