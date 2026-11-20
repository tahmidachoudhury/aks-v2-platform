# github-oidc

**TL;DR:** Three user-assigned managed identities that GitHub Actions signs in as with OIDC. `id-gh-infra` applies OpenTofu, only from jobs in the `infra` environment. `id-gh-infra-plan` runs pull request plans with read access only. `id-gh-app` pushes images to ACR. None has a client secret. Filled by P3-21 (#47) and called from the `persistent` root, which puts them in `rg-aks-v2-identity`.

## Why managed identities

A user-assigned managed identity takes federated credentials like an app registration, but it is an Azure resource. The azurerm provider creates it, so no azuread provider or Entra ID directory permissions are needed. It can't hold a client secret, which meets the card's "no client secret anywhere" by design.

Each identity has `prevent_destroy`. A recreated identity gets a new client ID, and the GitHub secrets would still hold the old one.

## Identities

All three trust the issuer `https://token.actions.githubusercontent.com` with audience `api://AzureADTokenExchange`. The subject says which workflow runs may sign in.

| Identity | Subjects | Roles |
| --- | --- | --- |
| `id-gh-infra` | `repo:<repo>:environment:infra` | Contributor and conditional RBAC Administrator on `rg-aks-v2-persistent`. Reader on the subscription. Storage Blob Data Contributor on the state account |
| `id-gh-infra-plan` | `repo:<repo>:pull_request` | Reader on the subscription. Storage Blob Data Reader on the state account |
| `id-gh-app` | `repo:<repo>:ref:refs/heads/main` | AcrPush on the registry |

`<repo>` comes from the persistent root's `github_repository` variable: `tahmidachoudhury@127567208/aks-v2-platform@1382383802`. The repo has immutable OIDC subjects on, so GitHub puts the owner and repository IDs into the subject. A deleted and recreated repo with the same name gets a new ID, so it can't sign in as these identities. Check the current form with `gh api repos/<owner>/<name>/actions/oidc/customization/sub`, which returns it as `sub_claim_prefix`.

A job that names a GitHub environment gets the environment subject, not the branch one. `id-gh-infra` trusts the `infra` environment only. Every plan or apply job that signs in as it must name that environment, so it waits for the reviewers P8-07 sets up. No branch push or manual run can skip them.

`id-gh-app` trusts main with no environment. It can only push images, so a review gate on every build would cost more than it protects.

### Why the identities have their own group

CI's Contributor role covers `rg-aks-v2-persistent`. If the identities sat there, CI could add a federated credential to `id-gh-infra` that trusts any branch, and skip the reviewers. `rg-aks-v2-identity` is not in `infra_resource_group_ids`, so CI can only read it. The group has `prevent_destroy` too.

### Why plans get their own identity

A `pull_request` credential on `id-gh-infra` would let any branch with a pull request sign in with Contributor and apply with no reviewer. So pull requests use `id-gh-infra-plan`, which can read settings and state but change nothing.

Forked pull requests get no OIDC token from GitHub, so only people with write access to the repo can use this credential.

Plans as this identity have limits:

- It can't take a lease on the state blob, so pull request plans run with `-lock=false`. A plan never writes state, so this is safe.
- It has no Key Vault data role, so it can't read app secrets. Refreshing a Key Vault secret reads its value, so a pull request plan of `persistent` must use `-refresh=false`.
- Reader can't call `listClusterUserCredential`, which azurerm uses to read the cluster. While a cluster exists, a pull request plan of `platform` must also use `-refresh=false`, or it fails.
- With `-refresh=false`, the plan compares the code with state, not with Azure. It shows what the change does, not drift. The apply job in the `infra` environment refreshes in full, so drift shows there before anything changes.

## Roles

### Reader on the subscription

Platform reads the egress public IPs from `rg-aks-v2-platform-nodes`, which AKS creates with each cluster. Nothing outside the cluster can grant a role on that group ahead of time, so both infra identities read at subscription scope. Reader sees resource settings, not secrets, keys or blob data.

### RBAC Administrator with an ABAC condition

The roots make role assignments, so the infra identity must write them. The aks module grants Network Contributor and AKS RBAC Cluster Admin. The keyvault module grants Key Vault Secrets Officer. Later cards grant AcrPull and data roles.

"Role Based Access Control Administrator" can grant any role to anyone. A condition, `condition_version = "2.0"`, limits both sides of each assignment it writes or deletes:

- The role must be in `infra_assignable_roles`. The persistent root looks the names up with `azurerm_role_definition`.
- The principal must be a service principal, which covers managed identities, or one of `operator_object_ids`. No other user and no group.

azurerm supports both arguments on `azurerm_role_assignment`. The condition follows the "constrain roles" and "constrain roles and principal types" patterns in [Azure role assignment conditions](https://learn.microsoft.com/en-us/azure/role-based-access-control/delegate-role-assignments-examples).

What this prevents:

- CI can't assign Contributor, Owner, User Access Administrator, RBAC Administrator or any role not in the list.
- CI can't grant any role to a user or group outside the operators, such as a guest account.
- CI can't change its own federated credentials or role assignments. Those live in the identity group or need roles outside the list. Apply this module's changes by hand.

What it still allows:

- CI can grant any listed role to any service principal in the tenant, itself included. For example, it can give itself Key Vault Secrets User and read the app secrets, or DNS Zone Contributor on the zone. The list holds data roles that real workloads need, so this is the price of letting CI wire them.
- This only applies within the groups in `infra_resource_group_ids`: `rg-aks-v2-persistent`, and `rg-aks-v2-ingress`, which holds only Traefik's public IP (P4-03). CI needs Contributor there to refresh and change the IP on a persistent apply, and RBAC Administrator to grant the cluster identity Network Contributor there once it applies platform. Contributor also lets CI delete the IP. `prevent_destroy` stops an OpenTofu apply doing that, but not a direct API call.
- The workload identities (`infra/modules/workload-identity`) sit in `rg-aks-v2-persistent`, so CI's Contributor role can add federated credentials to them. A reviewed apply could add one to `id-external-secrets` that trusts `pull_request`, and every pull request job could then read the app secrets, even though `id-gh-infra-plan` has no Key Vault role. That's no more than the first point already allows, but it's a second route. They sit there because CI has to write their credentials every morning once it applies platform.

Add a role to `infra_assignable_roles` before a root assigns it, or the CI apply fails with a 403. Add a person to `operator_object_ids` before CI grants them anything.

### State account

The backend reads, writes and leases blobs as the caller, so `id-gh-infra` gets Storage Blob Data Contributor on the state account. The account sits in `rg-aks-v2-tfstate`, made by `bootstrap`. CI never runs `bootstrap`, so neither identity gets anything else in that group.

## Known gap: the platform resource group

`id-gh-infra` has Contributor and RBAC Administrator on `rg-aks-v2-persistent` and `rg-aks-v2-ingress` only. Both are persistent groups. The card asked for `rg-aks-v2-platform` too, but the platform root creates that group every morning and deletes it every evening. A role assignment on it would be deleted with it, and would fail to create while the group doesn't exist.

Until that changes, CI can't apply platform. It can plan it, with `-refresh=false` while a cluster exists. The fix is to move `rg-aks-v2-platform` into the persistent root, have platform read it with a `data` source like the Key Vault, then add it to `infra_resource_group_ids`. This changes the state of two roots, so it needs its own card.

## Federated credentials

Azure refuses concurrent federated credential writes on one identity. Each identity has one credential today. With more than one environment in `infra_environments`, the credentials are written at the same time, so apply with `-parallelism=1`.

A user-assigned identity takes up to 20 federated credentials.

## Inputs

| Name | Notes |
| --- | --- |
| `github_repository` | As in the OIDC subject: `owner@owner_id/name@repo_id` with immutable subjects, otherwise `owner/name` |
| `resource_group_name` | Where the identities live. Keep it out of `infra_resource_group_ids` |
| `location` | |
| `tags` | |
| `operator_object_ids` | People who apply by hand. The only users CI may grant roles to. At least one |
| `infra_environments` | GitHub environments whose jobs sign in as `id-gh-infra` |
| `infra_resource_group_ids` | Map of static label to resource group ID |
| `infra_assignable_roles` | Built-in role names the ABAC condition allows |
| `state_storage_account_id` | |
| `acr_id` | |

## Outputs

`infra_client_id`, `infra_principal_id`, `infra_plan_client_id`, `infra_plan_principal_id`, `app_client_id`, `app_principal_id` and `tenant_id`.

## GitHub secrets

None of these values is a credential: a client ID only works with a token GitHub signs for this repo. They are still identifiers, and Actions logs are public on this repo. GitHub prints a step's inputs in the log and masks only secrets. So store them as repository secrets, not variables. Never commit them.

| Secret | Source |
| --- | --- |
| `AZURE_CLIENT_ID_INFRA` | `tofu output -raw github_infra_client_id` |
| `AZURE_CLIENT_ID_INFRA_PLAN` | `tofu output -raw github_infra_plan_client_id` |
| `AZURE_CLIENT_ID_APP` | `tofu output -raw github_app_client_id` |
| `AZURE_TENANT_ID` | `tofu output -raw tenant_id` |
| `AZURE_SUBSCRIPTION_ID` | The subscription the roots deploy to |

After applying persistent, from `infra/persistent`:

```bash
R=tahmidachoudhury/aks-v2-platform
gh secret set AZURE_CLIENT_ID_INFRA      -R "$R" --body "$(tofu output -raw github_infra_client_id)"
gh secret set AZURE_CLIENT_ID_INFRA_PLAN -R "$R" --body "$(tofu output -raw github_infra_plan_client_id)"
gh secret set AZURE_CLIENT_ID_APP        -R "$R" --body "$(tofu output -raw github_app_client_id)"
gh secret set AZURE_TENANT_ID            -R "$R" --body "$(tofu output -raw tenant_id)"
gh secret set AZURE_SUBSCRIPTION_ID      -R "$R" --body "<subscription id>"
```

The values go straight from the output to GitHub and never print.

## Acceptance test

If P8-07 hasn't yet, create the `infra` environment in the repo settings. Then run [`azure-login-check`](../../../.github/workflows/azure-login-check.yml) from the Actions tab. Its job names the `infra` environment, signs in as `id-gh-infra` with `azure/login` and OIDC, and prints how many resource groups it can see. It prints no names or IDs, because the logs are public.
