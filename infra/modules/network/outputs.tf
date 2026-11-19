output "vnet_id" {
  description = "ID of the VNet."
  value       = azurerm_virtual_network.this.id
}

output "vnet_name" {
  description = "Name of the VNet."
  value       = azurerm_virtual_network.this.name
}

output "subnet_ids" {
  description = "Subnet IDs by function: aks_nodes and private_endpoints."
  value = {
    aks_nodes         = azurerm_subnet.aks_nodes.id
    private_endpoints = azurerm_subnet.private_endpoints.id
  }
}

output "private_dns_zone_ids" {
  description = "Private DNS zone IDs, keyed like var.private_dns_zones, for private endpoint DNS zone groups."
  value       = { for k, z in azurerm_private_dns_zone.this : k => z.id }
}
