output "id" {
  description = "ID of the zone. Scope for ExternalDNS's DNS Zone Contributor role."
  value       = azurerm_dns_zone.this.id
}

output "name" {
  description = "Name of the zone."
  value       = azurerm_dns_zone.this.name
}

output "name_servers" {
  description = "The zone's four Azure name servers. Add them as NS records in the parent zone."
  value       = azurerm_dns_zone.this.name_servers
}
