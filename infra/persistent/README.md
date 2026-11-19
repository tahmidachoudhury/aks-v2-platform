# persistent

**TL;DR:** Holds what must survive the nightly teardown: the DNS zone, Key Vault, ACR and the GitHub Actions identities. Never destroy this root.

## What it holds

- A resource group, `rg-aks-v2-persistent`, in UK South. `prevent_destroy` is set on it, so no plan can delete it by accident.
- The container registry (P3-10), through [`../modules/acr`](../modules/acr/README.md). Basic SKU, Entra ID access only.
- The Key Vault and its four app secrets (P3-13), through [`../modules/keyvault`](../modules/keyvault/README.md). Secret values never reach state.
- The public DNS zone `azure.order-fulfilment.tahmidchoudhury.uk` (P3-15), through [`../modules/dns`](../modules/dns/README.md). Its delegation from the parent zone is a manual step described there.
- The GitHub Actions identities `id-gh-infra`, `id-gh-infra-plan` and `id-gh-app` (P3-21), through [`../modules/github-oidc`](../modules/github-oidc/README.md). They sign in with OIDC and have no client secret. Their client IDs are in GitHub repository secrets, so they must not change. They sit in a second group, `rg-aks-v2-identity`, also with `prevent_destroy`. CI has no write role on that group, so it can't add federated credentials to its own identities.

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

`acr_name`, `key_vault_name`, `state_storage_account_name` and `operator_object_ids` are required. The first two are globally unique names. Copy `storage_account_name` from `infra/backend.hcl` for the third; a name that matches no account fails the plan with a clear error. `operator_object_ids` lists the people who apply by hand: find yours with `az ad signed-in-user show --query id -o tsv`. Copy [`terraform.tfvars.example`](terraform.tfvars.example) to `terraform.tfvars`, which is gitignored, and set them. `location`, `resource_group_name`, `identity_resource_group_name`, `environment`, `owner`, `dns_zone_name`, `github_repository` and `state_resource_group_name` have defaults.

Key Vault Secrets Officer goes to the operators and to `id-gh-infra`, a fixed list. It doesn't follow whoever runs the apply, so a person and CI applying in turn don't swap each other's access.

Run the first apply yourself, not in CI. CI can't change its own role assignments, and the identities it signs in as are created here. After the apply, set the GitHub secrets listed in the [github-oidc README](../modules/github-oidc/README.md#github-secrets).
