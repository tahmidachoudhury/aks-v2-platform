output "resource_group_name" {
  description = "Resource group of the state storage account."
  value       = azurerm_resource_group.state.name
}

output "storage_account_name" {
  description = "State storage account name, for the backend block of later roots."
  value       = azurerm_storage_account.state.name
}

output "container_name" {
  description = "State container name, for the backend block of later roots."
  value       = azurerm_storage_container.state.name
}

output "backend_config" {
  description = "Partial backend config shared by every later root. Write it to infra/backend.hcl."
  value       = <<-EOT
    resource_group_name  = "${azurerm_resource_group.state.name}"
    storage_account_name = "${azurerm_storage_account.state.name}"
    container_name       = "${azurerm_storage_container.state.name}"
    use_azuread_auth     = true
  EOT
}
