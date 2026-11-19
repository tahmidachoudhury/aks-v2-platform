output "id" {
  description = "ID of the vault. Scope for role assignments such as Key Vault Secrets User."
  value       = azurerm_key_vault.this.id
}

output "name" {
  description = "Name of the vault."
  value       = azurerm_key_vault.this.name
}

output "vault_uri" {
  description = "Data plane URI, such as https://<name>.vault.azure.net/. ESO's SecretStore points here."
  value       = azurerm_key_vault.this.vault_uri
}
