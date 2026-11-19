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
