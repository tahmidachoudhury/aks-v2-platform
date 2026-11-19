output "id" {
  description = "ID of the registry. Scope for the AcrPull and AcrPush role assignments."
  value       = azurerm_container_registry.this.id
}

output "name" {
  description = "Name of the registry."
  value       = azurerm_container_registry.this.name
}

output "login_server" {
  description = "Registry host for image references, such as <name>.azurecr.io."
  value       = azurerm_container_registry.this.login_server
}
