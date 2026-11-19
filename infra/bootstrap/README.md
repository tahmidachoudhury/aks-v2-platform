# bootstrap

**TL;DR:** Creates the storage account that holds remote state for every other root, plus the project's cost budget. Apply it once, by hand, before anything else.

## What it creates

- A resource group for state in UK South.
- A StorageV2 account, Standard LRS. HTTPS only, TLS 1.2 minimum, no public blob access, shared keys off. Blob versioning is on, and deleted blobs and containers can be restored for 14 days.
- A private container, `tfstate`. Each root writes its own blob in it. Old versions of a state blob are deleted after 30 days.
- `prevent_destroy` on the account, so no plan can delete it by accident. Remove that line on purpose at final teardown.
- A Storage Blob Data Contributor role assignment on the account for whoever runs the apply.
- A monthly subscription budget, `budget-aks-v2-platform`, 100 by default. It emails the addresses in `budget_contact_emails` at 50%, 80% and 100% of actual spend, and when forecast spend reaches 100%. It leaves any existing subscription budget alone.

Shared keys are off, so everything uses Entra ID to reach the blobs. Owner on the subscription doesn't grant blob data access, so the root grants the role to the caller. The later roots' backend sets `use_azuread_auth = true`.

## Why its own state is local

The state account can't hold the state of the root that creates it. This root keeps a local `terraform.tfstate`, which is gitignored. If you lose the file, import the resources again or recreate them. Moving it into the new account with `tofu init -migrate-state` is possible later, but not needed.

## Before you apply

- Sign in with `az login` as a user who can create role assignments (Owner or User Access Administrator on the subscription).
- If the subscription doesn't have `Microsoft.Storage` and `Microsoft.Consumption`, register them once by hand. The provider never registers resource providers.
- Pick a storage account name. It must be globally unique, 3 to 24 lowercase letters and digits.

## Apply

```bash
cd infra/bootstrap
cp terraform.tfvars.example terraform.tfvars   # set storage_account_name and budget_contact_emails
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

Use a throwaway root with only a backend block. The backend config carries the subscription, so it needs no provider block:

```bash
mkdir -p /tmp/state-check && cd /tmp/state-check
cat > main.tf <<'EOF'
terraform {
  backend "azurerm" {
    key = "state-check.tfstate"
  }
}

# Something to apply, so apply stops at its prompt and holds the lease.
resource "terraform_data" "hold" {}
EOF
tofu init -backend-config=<repo>/infra/backend.hcl
```

`tofu init` should report that it configured the `azurerm` backend. To see the lock, hold it in one terminal and try to take it from another:

```bash
# Terminal 1: apply holds the lease while it waits at the "yes" prompt. Leave it there.
tofu apply

# Terminal 2, same folder:
tofu plan -lock-timeout=0s
```

The plan fails with `Error acquiring the state lock` and names the lease holder. Answer `no` in terminal 1 to release it. Delete the `state-check.tfstate` blob afterwards.
