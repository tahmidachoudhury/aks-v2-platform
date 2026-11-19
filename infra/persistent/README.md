# persistent

**TL;DR:** Holds what must survive the nightly teardown: the DNS zone, Key Vault and ACR. Never destroy this root.

## What it holds

- A resource group, `rg-aks-v2-persistent`, in UK South. `prevent_destroy` is set on it, so no plan can delete it by accident.
- Later: the public DNS zone (P3-15), the Key Vault (P3-13) and the container registry (P3-10), through the modules in [`../modules`](../modules).

State lives in the `persistent.tfstate` blob in the state account.

## What belongs here

Put a resource here if recreating it every day would break something. The zone would get new name servers, the vault name stays reserved after a delete, and the registry would lose its images.

Any managed identity whose client ID is referenced from Git belongs here too. This covers ExternalDNS, External Secrets and the per-service workload identities from P3-16 and P3-18, whose client IDs appear in manifests that ArgoCD applies. A recreated identity gets a new client ID, but Git still holds the old one. Their federated credentials depend on the cluster's OIDC issuer, which changes with each new cluster, so those stay in `platform`.

## Lifecycle

Apply once, after `bootstrap`, and again when this root changes. Never run `tofu destroy` here. To tear the project down for good, first remove `prevent_destroy` on purpose.

## Init, plan and apply

Needs `infra/backend.hcl`, generated from the bootstrap outputs. See [`../bootstrap`](../bootstrap/README.md).

```bash
cd infra/persistent
export ARM_SUBSCRIPTION_ID=<subscription id>
tofu init -backend-config=../backend.hcl
tofu plan -out=persistent.tfplan
tofu apply persistent.tfplan
```

No variables are required. `location`, `resource_group_name`, `environment` and `owner` have defaults. To override them, use a gitignored `terraform.tfvars`.
