output "infra_client_id" {
  description = "Client ID of id-gh-infra. Set as the AZURE_CLIENT_ID_INFRA repository variable."
  value       = azurerm_user_assigned_identity.infra.client_id
}

output "infra_principal_id" {
  description = "Principal ID of id-gh-infra, for extra role assignments."
  value       = azurerm_user_assigned_identity.infra.principal_id
}

output "infra_plan_client_id" {
  description = "Client ID of id-gh-infra-plan. Set as the AZURE_CLIENT_ID_INFRA_PLAN repository variable."
  value       = azurerm_user_assigned_identity.infra_plan.client_id
}

output "infra_plan_principal_id" {
  description = "Principal ID of id-gh-infra-plan, for extra role assignments."
  value       = azurerm_user_assigned_identity.infra_plan.principal_id
}

output "app_client_id" {
  description = "Client ID of id-gh-app. Set as the AZURE_CLIENT_ID_APP repository variable."
  value       = azurerm_user_assigned_identity.app.client_id
}

output "app_principal_id" {
  description = "Principal ID of id-gh-app, for extra role assignments."
  value       = azurerm_user_assigned_identity.app.principal_id
}

output "tenant_id" {
  description = "Entra ID tenant. Set as the AZURE_TENANT_ID repository variable."
  value       = data.azurerm_client_config.current.tenant_id
}
