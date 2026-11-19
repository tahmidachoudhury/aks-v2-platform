output "client_ids" {
  description = "Client ID of each identity, by workload name. The azure.workload.identity/client-id annotation on its ServiceAccount."
  value       = { for k, v in azurerm_user_assigned_identity.this : k => v.client_id }
}

output "principal_ids" {
  description = "Principal ID of each identity, by workload name, for role assignments."
  value       = { for k, v in azurerm_user_assigned_identity.this : k => v.principal_id }
}
