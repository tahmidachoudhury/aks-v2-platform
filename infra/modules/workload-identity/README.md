# workload-identity

**TL;DR:** One user-assigned managed identity per workload that signs in to Azure: the four services that use `azidentity`, ExternalDNS and External Secrets. The `persistent` root calls this module, so the client IDs never change. The federated credentials and Service Bus roles live in `platform`, since they name the cluster's OIDC issuer and the queue, both new every morning. No identity holds Contributor, Owner or a client secret. Filled by P3-16 (#42), P3-17 (#43) and P3-18 (#44).

## What it creates

One `azurerm_user_assigned_identity` per name in `names`, called `id-<name>`, in `rg-aks-v2-persistent`. Each has `prevent_destroy`, since a recreated identity gets a new client ID and the `azure.workload.identity/client-id` annotation on its ServiceAccount would still hold the old one.

The rest of the wiring sits in the roots, next to the resources it points at:

| What | Root | Why there |
| --- | --- | --- |
| The identities | `persistent` | Client IDs must survive the nightly destroy |
| Key Vault and DNS roles | `persistent` | The vault and the zone are in persistent |
| Federated credentials | `platform` | They name the cluster's OIDC issuer, new with each cluster |
| Service Bus roles | `platform` | The namespace and queue are destroyed every evening, and their role assignments with them |

Platform reads the identities by name with `data "azurerm_user_assigned_identity"`, so apply persistent first. Until the identities exist, a platform plan fails with "User Assigned Identity ... was not found".

## Identities

The services are the four in `app/services/` that import `azidentity` and read `SERVICEBUS_NAMESPACE`, per the [service catalogue](../../../docs/service-catalogue.md). inventory-service publishes nothing (#13), and the other four services touch no Azure API.

| Identity | Namespace | ServiceAccount | Federated subject |
| --- | --- | --- | --- |
| `id-order-service` | `apps` | `order-service` | `system:serviceaccount:apps:order-service` |
| `id-payment-service` | `apps` | `payment-service` | `system:serviceaccount:apps:payment-service` |
| `id-shipping-service` | `apps` | `shipping-service` | `system:serviceaccount:apps:shipping-service` |
| `id-worker` | `apps` | `worker` | `system:serviceaccount:apps:worker` |
| `id-external-dns` | `platform` | `external-dns` | `system:serviceaccount:platform:external-dns` |
| `id-external-secrets` | `platform` | `external-secrets` | `system:serviceaccount:platform:external-secrets` |

Every credential uses the cluster's issuer, `module.aks.oidc_issuer_url`, and the audience `api://AzureADTokenExchange`. Each is named after the cluster, `aks-v2-platform`.

### Naming

Nothing in the repo fixed these names yet, so this is a convention:

- A service's ServiceAccount takes the name of its folder in `app/services/`, as its manifests will (see [`k8s/README.md`](../../../k8s/README.md)).
- The services run in `apps` and the components in `platform`, the namespaces P4-07 (#54) lists.
- ExternalDNS and External Secrets keep their Helm charts' default ServiceAccount names, `external-dns` and `external-secrets`.

If P4-07 or a chart install picks other names, change `local.workload_identities` in `platform/main.tf`. The subjects live only in platform, so a rename recreates nothing in persistent and leaves the client IDs alone.

### cert-manager

No identity yet. P4-04 (#51) hasn't chosen between HTTP-01 through Traefik, which needs no Azure access, and DNS-01 through Azure DNS. If it picks DNS-01, add `cert-manager` to `names` in persistent, a `platform`-namespace binding in platform, and a DNS Zone Contributor assignment on the zone like ExternalDNS's.

## Role matrix

| Identity | Role | Scope | Root |
| --- | --- | --- | --- |
| `id-order-service` | Azure Service Bus Data Sender | Queue `order-events` | `platform` |
| `id-payment-service` | Azure Service Bus Data Sender | Queue `order-events` | `platform` |
| `id-shipping-service` | Azure Service Bus Data Sender | Queue `order-events` | `platform` |
| `id-worker` | Azure Service Bus Data Receiver | Queue `order-events` | `platform` |
| `id-external-secrets` | Key Vault Secrets User | The Key Vault, `key_vault_name` | `persistent` |
| `id-external-dns` | DNS Zone Contributor | Zone `azure.order-fulfilment.tahmidchoudhury.uk` | `persistent` |
| `id-external-dns` | Reader | Group `rg-aks-v2-persistent` | `persistent` |

Why each one:

- Queue, not namespace: `order-events` is the only entity in the namespace, and a role on the queue also covers its dead-letter queue, so the worker can dead-letter without anything wider.
- Sender or Receiver, never Owner: publishers can't receive and the worker can't send. Service Bus Data Owner would also let them manage entities.
- No Key Vault role for the services: P4-06 (#53) installs one ClusterSecretStore, so ESO is the only workload that reads Key Vault. The services get secrets as Kubernetes Secrets. A per-namespace SecretStore would need a Key Vault role per service, and the [Secrets delivery](../../../docs/architecture-decisions.md#secrets-delivery) ADR didn't ask for that.
- Secrets User on the vault, not per secret: ESO serves every secret in the vault, so a per-secret grant would list all four and add nothing. Secrets User reads secret values and changes nothing. It can't use or read keys.
- Reader for ExternalDNS: it lists the zones in its resource group before it writes, and a role on the zone doesn't cover that list call. The [ExternalDNS Azure guide](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/azure/) asks for Reader on the group. Reader sees resource settings in `rg-aks-v2-persistent`, never secret values or blobs, and changes nothing. A custom role with only `Microsoft.Network/dnszones/read` would be tighter, but a role definition to hide settings that hold no secret isn't worth it.

Every assignment sets `principal_type = "ServicePrincipal"`.

## Where the identities live

In `rg-aks-v2-persistent`, not in `rg-aks-v2-identity` with the GitHub identities.

The [github-oidc README](../github-oidc/README.md#why-the-identities-have-their-own-group) keeps the GitHub identities out of CI's reach, so CI can't add a federated credential to its own identity and skip the reviewers. The same test gives the opposite answer here:

- These identities are not CI's. A credential added to one gets CI only that identity's roles.
- CI can already grant itself every role they hold in persistent: Key Vault Secrets User, DNS Zone Contributor and Reader are on its ABAC list. Service Bus Data Sender and Receiver are on it too, for when CI applies platform.
- Once CI applies platform, it must write these federated credentials every morning. In the identity group it couldn't.

## CI and the ABAC condition

`id-gh-infra` may only assign the roles in `infra_assignable_roles` in `persistent/main.tf`, and only to service principals or the operators. Managed identities are service principals. The list already had the two Service Bus roles, DNS Zone Contributor and Key Vault Secrets User. This change adds Reader, for ExternalDNS.

Adding a role rewrites the condition on CI's RBAC Administrator assignment, and azurerm replaces a role assignment rather than updating it. So the persistent plan shows `infra_rbac_admin["persistent"] must be replaced`. The only change inside is Reader's GUID. Apply it by hand, as with every change to CI's own access.

The Service Bus roles are in `rg-aks-v2-platform`, where CI has no RBAC Administrator role yet. This is the [known gap](../github-oidc/README.md#known-gap-the-platform-resource-group): platform is applied by hand until it closes.

## Federated credentials and the daily rebuild

- Issuer change: azurerm updates `issuer`, `subject` and `audience` in place. Only `name` and the identity force a new credential. This rarely matters, since `tofu destroy` deletes the credentials each evening with the cluster and the morning apply creates them with the new issuer.
- Stale credentials: destroy removes them, so yesterday's issuer is never left trusted. If a cluster is deleted outside OpenTofu, the next apply still updates each credential in place.
- Limits: an identity takes up to 20 federated credentials, and each has one. Azure refuses concurrent credential writes on one identity, so one credential each means the parallel apply never writes two to one identity at once.

## Client IDs

A client ID isn't a credential. It only works with a token the cluster's issuer signs for the matching ServiceAccount. It is still an identifier, and this repo is public. The GitHub identities' client IDs are kept in repository secrets, not Git, so these follow the same rule for now: persistent outputs them, and nothing commits them.

```bash
tofu -chdir=infra/persistent output workload_identity_client_ids
```

The ServiceAccount annotations need them (P6-05, #66, and the P4 component installs). Those cards decide how they get there, committed in the overlay or injected at sync. The [infra README](../../README.md#what-goes-in-persistent) assumes they will be committed.

## Proving it

Nothing here is verifiable until applied. After the persistent and platform applies:

```bash
# The identities and their client IDs
tofu -chdir=infra/persistent output workload_identity_client_ids

# Each credential's subject and issuer, against the cluster's issuer
az identity federated-credential list -g rg-aks-v2-persistent --identity-name id-worker \
  --query "[].{subject:subject, issuer:issuer}" -o table
tofu -chdir=infra/platform output -raw oidc_issuer_url
```

Signing in without a secret needs the ServiceAccounts and pods, so it waits for the component installs (P4-05, P4-06), the service manifests (P6-05) and ArgoCD (P7).

## Inputs

| Name | Notes |
| --- | --- |
| `names` | Workload names, matching their ServiceAccounts. Each becomes `id-<name>` |
| `resource_group_name` | |
| `location` | |
| `tags` | |

## Outputs

`client_ids` and `principal_ids`, both maps keyed by workload name.
