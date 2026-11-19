# Basic, with no private endpoint. See README.md and the Container registry
# decision in docs/architecture-decisions.md.
resource "azurerm_container_registry" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  location            = var.location
  sku                 = "Basic"

  # Entra ID only: the kubelet pulls with AcrPull (P3-11) and CI pushes with
  # AcrPush (P3-21). No shared admin password, no unauthenticated pulls.
  admin_enabled          = false
  anonymous_pull_enabled = false

  tags = var.tags

  # Deleting the registry deletes every image. Remove this line on purpose,
  # and only at final teardown.
  lifecycle {
    prevent_destroy = true
  }
}
