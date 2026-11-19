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

  # Restricted, not disabled (P3-14). The cluster reaches the vault through
  # the private endpoint in platform, which bypasses this firewall. That
  # endpoint goes every evening with the VNet, and the operators write
  # secrets from outside Azure, so the public endpoint stays up for the
  # listed addresses only. No trusted-service bypass: nothing here needs it.
  # See README.md.
  public_network_access_enabled = true

  network_acls {
    default_action = "Deny"
    bypass         = "None"
    ip_rules       = var.allowed_ip_ranges
  }

  tags = var.tags

  # Purge protection already stops a hard delete. This also stops a plan from
  # replacing the vault. Remove this line on purpose, and only at final
  # teardown.
  lifecycle {
    prevent_destroy = true
  }
}

# Owner on the subscription does not grant access to secret values. Whoever
# applies writes the secrets below, so every principal that applies needs
# this role. The list is fixed, not the caller, so the assignments stay the
# same whether a person or CI runs the apply.
resource "azurerm_role_assignment" "secrets_officer" {
  for_each = var.secrets_officers

  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets Officer"
  principal_id         = each.value
}

# Role assignments take a minute or two to reach the Key Vault data plane.
# Without this wait, the first secret write returns 403.
resource "time_sleep" "rbac_propagation" {
  create_duration = "90s"

  triggers = {
    officers = join(",", sort([for a in azurerm_role_assignment.secrets_officer : a.principal_id]))
  }
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

  depends_on = [time_sleep.rbac_propagation]
}
