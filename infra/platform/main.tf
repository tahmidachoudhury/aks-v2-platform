# azurerm has no provider-level default tags, so every resource in this root
# sets tags = local.tags. Keep the keys the same in every root so Cost
# Management can group and filter by them.
locals {
  tags = {
    project    = "aks-v2-platform"
    managed-by = "opentofu"
    root       = "platform"
    env        = var.environment
    owner      = var.owner
  }
}

resource "azurerm_resource_group" "platform" {
  name     = var.resource_group_name
  location = var.location
  tags     = local.tags
}
