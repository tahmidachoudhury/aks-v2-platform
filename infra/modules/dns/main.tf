resource "azurerm_dns_zone" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  tags                = var.tags

  # A new zone gets new name servers, which breaks the delegation in the
  # parent zone. Remove this line on purpose, and only at final teardown.
  lifecycle {
    prevent_destroy = true
  }
}
