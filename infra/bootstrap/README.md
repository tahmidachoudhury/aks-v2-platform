# bootstrap

**TL;DR:** Creates the storage account that holds remote state for every other root. Apply it once, by hand, before anything else.

## What it creates

- A resource group for state in UK South.
- A StorageV2 account, Standard LRS. HTTPS only, TLS 1.2 minimum, no public blob access, shared keys off. Blob versioning is on, and deleted blobs and containers can be restored for 14 days.
- A private container, `tfstate`. Each root writes its own blob in it.
- A Storage Blob Data Contributor role assignment on the account for whoever runs the apply.

Shared keys are off, so everything reaches the blobs with Entra ID. Owner on the subscription doesn't grant blob data access, so the root grants the role to the caller. The later roots' backend sets `use_azuread_auth = true`.

## Why its own state is local

The state account can't hold the state of the root that creates it. This root keeps a local `terraform.tfstate`, which is gitignored. If the file is lost, import the resources again or recreate them. You can move it into the new account later with `tofu init -migrate-state`, but nothing needs that.

## Before you apply

- Sign in with `az login` as a user who can create role assignments (Owner or User Access Administrator on the subscription).
- Register `Microsoft.Storage` once by hand if the subscription doesn't have it. The provider never registers resource providers.
- Pick a storage account name. It must be globally unique, 3 to 24 lowercase letters and digits.

## Apply

```bash
cd infra/bootstrap
cp terraform.tfvars.example terraform.tfvars   # set storage_account_name
export ARM_SUBSCRIPTION_ID=<subscription id>
tofu init
tofu plan -out=bootstrap.tfplan
tofu apply bootstrap.tfplan
```

Then write the shared backend config for the other roots:

```bash
tofu output -raw backend_config > ../backend.hcl
```

`infra/backend.hcl` is gitignored. [`../backend.hcl.example`](../backend.hcl.example) shows its shape. A new role assignment can take a few minutes, so a 403 from the backend straight after the apply usually clears on a retry.

## Check the acceptance criteria

Use a throwaway root with only a backend block, in a shell that still has `ARM_SUBSCRIPTION_ID` set:

```bash
mkdir -p /tmp/state-check && cd /tmp/state-check
cat > main.tf <<'EOF'
terraform {
  backend "azurerm" {
    key = "state-check.tfstate"
  }
}
EOF
tofu init -backend-config=<repo>/infra/backend.hcl
```

`tofu init` should report that it configured the `azurerm` backend. Then run two plans at once, in two terminals or like this:

```bash
tofu plan -lock-timeout=0s & sleep 1; tofu plan -lock-timeout=0s; wait
```

One plan holds the blob lease. The other fails with `Error acquiring the state lock` and names the lease holder. Delete the `state-check.tfstate` blob afterwards.
