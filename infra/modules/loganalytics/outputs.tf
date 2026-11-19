output "id" {
  description = "ID of the workspace, for further diagnostic settings."
  value       = azurerm_log_analytics_workspace.this.id
}

output "name" {
  description = "Name of the workspace."
  value       = azurerm_log_analytics_workspace.this.name
}

output "workspace_id" {
  description = "Workspace GUID, for az monitor log-analytics query."
  value       = azurerm_log_analytics_workspace.this.workspace_id
}
