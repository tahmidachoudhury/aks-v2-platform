# github-oidc

**TL;DR:** Three user-assigned managed identities that GitHub Actions signs in as with OIDC. `id-gh-infra` applies OpenTofu, `id-gh-infra-plan` runs pull request plans with read access only, and `id-gh-app` pushes images to ACR. No client secret exists for any of them. Filled by P3-21 (#47) and called from the `persistent` root, because their client IDs live in GitHub variables and must not change.

## Why managed identities

A user-assigned managed identity takes federated credentials like an app registration, but it is an Azure resource. The azurerm provider creates it, so no azuread provider or Entra ID directory permissions are needed. It can't hold a client secret, which meets the card's "no client secret anywhere" by design.

## Identities

All three trust GitHub's issuer, `https://token.actions.githubusercontent.com`, with audience `api://AzureADTokenExchange`. The subject says which workflow runs may sign in.

| Identity | Subjects | Roles |
| --- | --- | --- |
| `id-gh-infra` | `repo:<repo>:ref:refs/heads/main`, `repo:<repo>:environment:infra` | Contributor and conditional RBAC Administrator on `rg-aks-v2-persistent`. Reader on the subscription. Storage Blob Data Contributor on the state account |
| `id-gh-infra-plan` | `repo:<repo>:pull_request` | Reader on the subscription. Storage Blob Data Reader on the state account. Key Vault Secrets User on the vault |
| `id-gh-app` | `repo:<repo>:ref:refs/heads/main` | AcrPush on the registry |

`<repo>` is `tahmidachoudhury/aks-v2-platform`, from the persistent root's `github_repository` variable.

A job that names a GitHub environment gets the environment subject, not the branch one. The apply jobs in P8-01 run in the `infra` environment, which P8-07 protects with required reviewers. A `workflow_dispatch` or `push` run on main with no environment gets the main subject.

### Why plans get their own identity

The card asked for one infra identity that trusts main, pull requests and environments. A `pull_request` credential on that identity would let any branch with a pull request sign in with Contributor and run an apply with no reviewer. That defeats the environment gate from P8-07. So pull requests get `id-gh-infra-plan`, which can read but not change anything.

Forked pull requests get no OIDC token from GitHub, so only people with write access to the repo can use this credential.

The plan identity has two limits:

- It can't take a lease on the state blob, so pull request plans run with `-lock=false`. A plan never writes state, so this is safe.
- A refresh of a Key Vault secret reads its value, so a plan of `persistent` fails without Key Vault Secrets User. A pull request can therefore read the four app secrets. This is the same exposure as the state file, and only repo writers can trigger it.

## Roles

### Reader on the subscription

Platform reads the egress public IPs from `rg-aks-v2-platform-nodes`, which AKS creates with each cluster. Nothing outside the cluster can grant a role on that group ahead of time, so both infra identities read at subscription scope. Reader sees resource settings, not secrets, keys or blob data.

### RBAC Administrator with an ABAC condition

The roots make role assignments: the aks module grants Network Contributor and AKS RBAC Cluster Admin, the keyvault module grants Key Vault Secrets Officer, and later cards grant AcrPull and data roles. So the infra identity must write role assignments.

"Role Based Access Control Administrator" can grant any role. A condition, `condition_version = "2.0"`, limits the roles it may assign or remove to the list in `infra_assignable_roles`. azurerm supports both arguments on `azurerm_role_assignment`. The condition follows the "constrain roles" pattern from [delegate Azure role assignment management](https://learn.microsoft.com/en-us/azure/role-based-access-control/delegate-role-assignments-portal). `azurerm_role_definition` looks up the role names, so the list in the persistent root reads as names.

Contributor, Owner, User Access Administrator and RBAC Administrator are not in the list, so CI can't give itself or anyone else more control. As a result, CI can't apply changes to this module's own role assignments. Apply those by hand.

Add a role to `infra_assignable_roles` before a root starts assigning it. Otherwise the CI apply fails with a 403.

### State account

The backend reads, writes and leases blobs as the caller, so `id-gh-infra` gets Storage Blob Data Contributor on the state account. The account sits in `rg-aks-v2-tfstate`, made by `bootstrap`. CI never runs `bootstrap`, so neither identity gets anything else in that group.

## Known gap: the platform resource group

`id-gh-infra` has Contributor and RBAC Administrator on `rg-aks-v2-persistent` only. The card asked for `rg-aks-v2-platform` too, but the platform root creates that group every morning and deletes it every evening. A role assignment on it would be deleted with it, and would fail to create while the group doesn't exist.

Until that changes, CI can plan platform but not apply it. The fix is to move `rg-aks-v2-platform` into the persistent root and have platform read it with a `data` source, like the Key Vault. Then add it to `infra_resource_group_ids` in the persistent root. This changes the state of two roots, so it needs its own card.

## Federated credentials

Azure refuses concurrent federated credential writes on one identity. The main and environment credentials on `id-gh-infra` are chained with `depends_on`. With more than one environment in `infra_environments`, the environment credentials still run together, so apply with `-parallelism=1`.

A user-assigned identity takes up to 20 federated credentials.

## Inputs

| Name | Notes |
| --- | --- |
| `github_repository` | `owner/name` |
| `resource_group_name` | Where the identities live |
| `location` | |
| `tags` | |
| `infra_environments` | GitHub environments for the apply jobs |
| `infra_resource_group_ids` | Map of static label to resource group ID |
| `infra_assignable_roles` | Built-in role names the ABAC condition allows |
| `state_storage_account_id` | |
| `key_vault_id` | |
| `acr_id` | |

## Outputs

`infra_client_id`, `infra_principal_id`, `infra_plan_client_id`, `infra_plan_principal_id`, `app_client_id`, `app_principal_id` and `tenant_id`.

## GitHub repository variables

None of these values is a secret. A client ID only works with a token GitHub signs for this repo. Store them as repository variables, not secrets, so they stay visible in the settings page and in logs. Never commit them, as the repo is public.

| Variable | Source |
| --- | --- |
| `AZURE_CLIENT_ID_INFRA` | `tofu output -raw github_infra_client_id` |
| `AZURE_CLIENT_ID_INFRA_PLAN` | `tofu output -raw github_infra_plan_client_id` |
| `AZURE_CLIENT_ID_APP` | `tofu output -raw github_app_client_id` |
| `AZURE_TENANT_ID` | `tofu output -raw tenant_id` |
| `AZURE_SUBSCRIPTION_ID` | The subscription the roots deploy to |

After applying persistent, from `infra/persistent`:

```bash
R=tahmidachoudhury/aks-v2-platform
gh variable set AZURE_CLIENT_ID_INFRA      -R "$R" --body "$(tofu output -raw github_infra_client_id)"
gh variable set AZURE_CLIENT_ID_INFRA_PLAN -R "$R" --body "$(tofu output -raw github_infra_plan_client_id)"
gh variable set AZURE_CLIENT_ID_APP        -R "$R" --body "$(tofu output -raw github_app_client_id)"
gh variable set AZURE_TENANT_ID            -R "$R" --body "$(tofu output -raw tenant_id)"
gh variable set AZURE_SUBSCRIPTION_ID      -R "$R" --body "<subscription id>"
```

The values go straight from the output to GitHub and never print.

## Acceptance test

Run [`azure-login-check`](../../../.github/workflows/azure-login-check.yml) from the Actions tab, on main. It signs in as `id-gh-infra` with `azure/login` and OIDC, prints the subscription name and lists the resource groups. A run from any other branch fails to sign in, which shows the subject check works.
