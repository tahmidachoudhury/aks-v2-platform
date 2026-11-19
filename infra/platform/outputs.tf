output "resource_group_name" {
  description = "Name of this root's resource group."
  value       = azurerm_resource_group.platform.name
}

output "resource_group_id" {
  description = "ID of this root's resource group."
  value       = azurerm_resource_group.platform.id
}

output "vnet_name" {
  description = "Name of the platform VNet, for private DNS zone links."
  value       = module.network.vnet_name
}

output "vnet_id" {
  description = "ID of the platform VNet."
  value       = module.network.vnet_id
}

output "subnet_ids" {
  description = "Subnet IDs by function."
  value       = module.network.subnet_ids
}
