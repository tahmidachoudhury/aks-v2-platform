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

  # Every secret the services read, from app/services and the service
  # catalogue. Bump a number to rotate that secret. See the module README.
  generated_secrets = {
    postgres-password       = 1
    jwt-secret              = 1
    shipping-webhook-secret = 1
    redis-password          = 1
  }
}

module "dns" {
  source = "../modules/dns"

  name                = var.dns_zone_name
  resource_group_name = azurerm_resource_group.persistent.name
  tags                = local.tags
}

# The state account lives in bootstrap, which CI never runs. Looked up by
# name with azurerm_resources, which returns its ID without the account keys
# that the azurerm_storage_account data source would put in state.
data "azurerm_resources" "state_account" {
  name                = var.state_storage_account_name
  resource_group_name = var.state_resource_group_name
  type                = "Microsoft.Storage/storageAccounts"
}

module "github_oidc" {
  source = "../modules/github-oidc"

  github_repository   = var.github_repository
  resource_group_name = azurerm_resource_group.persistent.name
  location            = azurerm_resource_group.persistent.location
  tags                = local.tags

  # The apply jobs from P8-01, behind reviewers set up in P8-07.
  infra_environments = ["infra"]

  # Platform's group is missing on purpose: that root destroys it every
  # evening, which would take any role assignment on it too. See the module
  # README.
  infra_resource_group_ids = {
    persistent = azurerm_resource_group.persistent.id
  }

  # Every role the roots assign, now or in a planned card. Add a role here
  # before a root starts assigning it, or the CI apply fails with 403.
  infra_assignable_roles = [
    "AcrPull",                                     # kubelet, P3-11
    "Azure Kubernetes Service RBAC Cluster Admin", # aks module
    "Azure Service Bus Data Receiver",             # worker, P3-16
    "Azure Service Bus Data Sender",               # publishers, P3-16
    "DNS Zone Contributor",                        # ExternalDNS, P3-18
    "Key Vault Secrets Officer",                   # keyvault module
    "Key Vault Secrets User",                      # External Secrets, P3-18
    "Network Contributor",                         # aks module, node subnet
  ]

  state_storage_account_id = one(data.azurerm_resources.state_account.resources).id
  key_vault_id             = module.keyvault.id
  acr_id                   = module.acr.id
}
