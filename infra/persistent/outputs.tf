output "resource_group_name" {
  description = "Name of this root's resource group."
  value       = azurerm_resource_group.persistent.name
}

output "resource_group_id" {
  description = "ID of this root's resource group."
  value       = azurerm_resource_group.persistent.id
}

output "acr_id" {
  description = "ID of the container registry."
  value       = module.acr.id
}

output "acr_name" {
  description = "Name of the container registry."
  value       = module.acr.name
}

output "acr_login_server" {
  description = "Registry host for image references."
  value       = module.acr.login_server
}
