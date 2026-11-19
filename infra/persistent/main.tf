# azurerm has no provider-level default tags, so every resource in this root
# sets tags = local.tags. Keep the keys the same in every root so Cost
# Management can group and filter by them.
locals {
  tags = {
    project    = "aks-v2-platform"
    managed-by = "opentofu"
    root       = "persistent"
    env        = var.environment
    owner      = var.owner
  }
}

resource "azurerm_resource_group" "persistent" {
  name     = var.resource_group_name
  location = var.location
  tags     = local.tags

  # Holds the DNS zone, Key Vault and ACR, which must survive the nightly
  # destroy of the platform root. Remove this line on purpose, and only at
  # final teardown.
  lifecycle {
    prevent_destroy = true
  }
}

module "acr" {
  source = "../modules/acr"

  name                = var.acr_name
  resource_group_name = azurerm_resource_group.persistent.name
  location            = azurerm_resource_group.persistent.location
  tags                = local.tags
}

module "keyvault" {
  source = "../modules/keyvault"

  name                = var.key_vault_name
  resource_group_name = azurerm_resource_group.persistent.name
  location            = azurerm_resource_group.persistent.location
  tags                = local.tags

  # The vault firewall lets in only these public addresses. The cluster uses
  # the private endpoint in platform instead. See the module README.
  allowed_ip_ranges = var.key_vault_allowed_ip_ranges

  # Every secret the services read, from app/services and the service
  # catalogue. Bump a number to rotate that secret. See the module README.
  generated_secrets = {
    postgres-password       = 1
    jwt-secret              = 1
    shipping-webhook-secret = 1
    redis-password          = 1
  }

  # Everyone who applies: the operators by hand and CI's infra identity.
  # Operators are keyed by their own object ID; the CI key is static because
  # its ID is unknown until the identity exists.
  secrets_officers = merge(
    { for id in var.operator_object_ids : id => id },
    { github-infra = module.github_oidc.infra_principal_id },
  )
}

module "dns" {
  source = "../modules/dns"

  name                = var.dns_zone_name
  resource_group_name = azurerm_resource_group.persistent.name
  tags                = local.tags
}

# The GitHub Actions identities get their own group. CI has no write role
# on it, so it can't add federated credentials to its own identities.
resource "azurerm_resource_group" "identity" {
  name     = var.identity_resource_group_name
  location = var.location
  tags     = local.tags

  # Deleting it deletes the identities, and new ones get new client IDs.
  # Remove this line on purpose, and only at final teardown.
  lifecycle {
    prevent_destroy = true
  }
}

# The state account lives in bootstrap, which CI never runs. Looked up by
# name with azurerm_resources, which returns its ID without the account keys
# that the azurerm_storage_account data source would put in state.
data "azurerm_resources" "state_account" {
  name                = var.state_storage_account_name
  resource_group_name = var.state_resource_group_name
  type                = "Microsoft.Storage/storageAccounts"

  lifecycle {
    postcondition {
      condition     = length(self.resources) == 1
      error_message = "No storage account named ${var.state_storage_account_name} in ${var.state_resource_group_name}. Set state_storage_account_name to storage_account_name from infra/backend.hcl."
    }
  }
}

module "github_oidc" {
  source = "../modules/github-oidc"

  github_repository   = var.github_repository
  resource_group_name = azurerm_resource_group.identity.name
  location            = azurerm_resource_group.identity.location
  tags                = local.tags

  # The only users the infra identity may grant roles to.
  operator_object_ids = var.operator_object_ids

  # The apply jobs from P8-01, behind reviewers set up in P8-07.
  infra_environments = ["infra"]

  # The identity group is left out on purpose: see above. So is platform's
  # group, which that root destroys every evening, taking any role
  # assignment on it too. See the module README.
  infra_resource_group_ids = {
    persistent = azurerm_resource_group.persistent.id
  }

  # Every role the roots assign, now or in a planned card. Add a role here
  # before a root starts assigning it, or the CI apply fails with 403.
  infra_assignable_roles = [
    "AcrPull",                                     # kubelet, P3-11
    "Azure Kubernetes Service RBAC Cluster Admin", # aks module
    "Azure Service Bus Data Receiver",             # worker, P3-17
    "Azure Service Bus Data Sender",               # publishers, P3-17
    "DNS Zone Contributor",                        # ExternalDNS, P3-18
    "Key Vault Secrets Officer",                   # keyvault module
    "Key Vault Secrets User",                      # External Secrets, P3-18
    "Network Contributor",                         # aks module, node subnet
    "Reader",                                      # ExternalDNS, P3-18
  ]

  state_storage_account_id = data.azurerm_resources.state_account.resources[0].id
  acr_id                   = module.acr.id
}

# Workload identities (P3-16, P3-18). Their client IDs go into ServiceAccount
# annotations, so they live here and never change. Their federated
# credentials and Service Bus roles are in platform. Role matrix and naming:
# ../modules/workload-identity/README.md.
#
# They sit in the persistent group, not the identity group. CI must be able
# to write their federated credentials once it applies platform, and they
# hold no role CI couldn't already grant itself here.
module "workload_identity" {
  source = "../modules/workload-identity"

  # Named as their Kubernetes ServiceAccounts: the four services that use
  # azidentity in app/services, then the platform components. cert-manager
  # is left out until P4-04 picks DNS-01 over HTTP-01. Keep this list in
  # step with local.workload_identities in infra/platform/main.tf: a name
  # here without an entry there gets no federated credential.
  names = [
    "order-service",
    "payment-service",
    "shipping-service",
    "worker",
    "external-dns",
    "external-secrets",
  ]

  resource_group_name = azurerm_resource_group.persistent.name
  location            = azurerm_resource_group.persistent.location
  tags                = local.tags
}

# One ClusterSecretStore (P4-06), so ESO is the only workload that reads Key
# Vault. The services get their secrets as Kubernetes Secrets and need no
# Key Vault role.
resource "azurerm_role_assignment" "external_secrets_key_vault" {
  scope                = module.keyvault.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = module.workload_identity.principal_ids["external-secrets"]
  principal_type       = "ServicePrincipal"
}

# ExternalDNS changes records in the zone and nothing else.
resource "azurerm_role_assignment" "external_dns_zone" {
  scope                = module.dns.id
  role_definition_name = "DNS Zone Contributor"
  principal_id         = module.workload_identity.principal_ids["external-dns"]
  principal_type       = "ServicePrincipal"
}

# ExternalDNS also lists the zones in the zone's group. A role on the zone
# doesn't cover a list call on the group, so the ExternalDNS Azure guide
# asks for Reader there. Reader sees settings, never secrets, and changes
# nothing.
resource "azurerm_role_assignment" "external_dns_reader" {
  scope                = azurerm_resource_group.persistent.id
  role_definition_name = "Reader"
  principal_id         = module.workload_identity.principal_ids["external-dns"]
  principal_type       = "ServicePrincipal"
}
