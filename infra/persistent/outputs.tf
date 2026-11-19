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

output "key_vault_id" {
  description = "ID of the Key Vault."
  value       = module.keyvault.id
}

output "key_vault_name" {
  description = "Name of the Key Vault."
  value       = module.keyvault.name
}

output "key_vault_uri" {
  description = "Data plane URI of the Key Vault."
  value       = module.keyvault.vault_uri
}

output "dns_zone_id" {
  description = "ID of the public DNS zone."
  value       = module.dns.id
}

output "dns_zone_name" {
  description = "Name of the public DNS zone."
  value       = module.dns.name
}

output "dns_zone_name_servers" {
  description = "Azure name servers for the zone. Add them as NS records in the parent zone."
  value       = module.dns.name_servers
}
