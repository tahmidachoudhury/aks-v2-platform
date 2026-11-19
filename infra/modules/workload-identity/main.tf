# One user-assigned identity per workload that signs in to Azure. Only the
# identities live here: the federated credentials and the Service Bus roles
# are in platform, because they name the cluster's issuer and the namespace
# that platform rebuilds every day. See README.md.
resource "azurerm_user_assigned_identity" "this" {
  for_each = toset(var.names)

  name                = "id-${each.value}"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = var.tags

  # A recreated identity gets a new client ID, and the ServiceAccount
  # annotations in Git would still hold the old one.
  lifecycle {
    prevent_destroy = true
  }
}
