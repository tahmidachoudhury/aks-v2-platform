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

output "ingress_ip_address" {
  description = "Traefik's public IP, where ExternalDNS points the A records. Fixed for the life of the project."
  value       = azurerm_public_ip.ingress.ip_address
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

output "github_infra_client_id" {
  description = "Client ID of id-gh-infra. Repository secret AZURE_CLIENT_ID_INFRA."
  value       = module.github_oidc.infra_client_id
}

output "github_infra_principal_id" {
  description = "Principal ID of id-gh-infra."
  value       = module.github_oidc.infra_principal_id
}

output "github_infra_plan_client_id" {
  description = "Client ID of id-gh-infra-plan. Repository secret AZURE_CLIENT_ID_INFRA_PLAN."
  value       = module.github_oidc.infra_plan_client_id
}

output "github_infra_plan_principal_id" {
  description = "Principal ID of id-gh-infra-plan."
  value       = module.github_oidc.infra_plan_principal_id
}

output "github_app_client_id" {
  description = "Client ID of id-gh-app. Repository secret AZURE_CLIENT_ID_APP."
  value       = module.github_oidc.app_client_id
}

output "github_app_principal_id" {
  description = "Principal ID of id-gh-app."
  value       = module.github_oidc.app_principal_id
}

output "tenant_id" {
  description = "Entra ID tenant. Repository secret AZURE_TENANT_ID. An output only: never commit it."
  value       = module.github_oidc.tenant_id
}

output "workload_identity_client_ids" {
  description = "Client ID of each workload identity, by ServiceAccount name, for the azure.workload.identity/client-id annotations. Not committed yet: see modules/workload-identity/README.md."
  value       = module.workload_identity.client_ids
}
