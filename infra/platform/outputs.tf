output "resource_group_name" {
  description = "Name of this root's resource group."
  value       = azurerm_resource_group.platform.name
}

output "resource_group_id" {
  description = "ID of this root's resource group."
  value       = azurerm_resource_group.platform.id
}
