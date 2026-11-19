# Secret values never reach state or the plan. See README.md.

data "azurerm_client_config" "current" {}

resource "azurerm_key_vault" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  location            = var.location
  tenant_id           = data.azurerm_client_config.current.tenant_id
  sku_name            = "standard"

  # Azure RBAC, not access policies. Grants are role assignments, scoped to
  # the vault or to one secret.
  rbac_authorization_enabled = true

  # 7 days is the minimum. With purge protection on, a deleted vault cannot
  # be purged early, so its name stays reserved for the full 7 days.
  soft_delete_retention_days = 7
  purge_protection_enabled   = true

  # Open for now. P3-14 adds the private endpoint and the laptop and CI path,
  # then closes this.
  public_network_access_enabled = true

  tags = var.tags

  # Purge protection already stops a hard delete. This also stops a plan from
  # replacing the vault. Remove this line on purpose, and only at final
  # teardown.
  lifecycle {
    prevent_destroy = true
  }
}

# Owner on the subscription does not grant access to secret values. The
# caller writes the secrets below, so it needs this role on the vault.
resource "azurerm_role_assignment" "caller_secrets_officer" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets Officer"
  principal_id         = data.azurerm_client_config.current.object_id
}

# A new random value on every run. It is ephemeral, so it is never stored.
# The secret below sends it to Azure only when value_wo_version changes.
# Alphanumeric, so the value is safe inside a URL such as DATABASE_URL.
ephemeral "random_password" "this" {
  for_each = var.generated_secrets

  length  = 48
  special = false
}

resource "azurerm_key_vault_secret" "generated" {
  for_each = var.generated_secrets

  name             = each.key
  key_vault_id     = azurerm_key_vault.this.id
  value_wo         = ephemeral.random_password.this[each.key].result
  value_wo_version = each.value
  content_type     = "generated"
  tags             = var.tags

  # Role assignments take a few minutes to reach the data plane. See the
  # README if the first apply fails with a 403.
  depends_on = [azurerm_role_assignment.caller_secrets_officer]
}
