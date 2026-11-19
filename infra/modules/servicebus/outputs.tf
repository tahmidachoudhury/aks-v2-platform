output "id" {
  description = "ID of the namespace, the scope for Service Bus data plane role assignments (P3-17)."
  value       = azurerm_servicebus_namespace.this.id
}

output "name" {
  description = "Name of the namespace."
  value       = azurerm_servicebus_namespace.this.name
}

output "fqdn" {
  description = "Fully qualified namespace, the value of SERVICEBUS_NAMESPACE in the manifests."
  value       = "${azurerm_servicebus_namespace.this.name}.servicebus.windows.net"
}

output "queue_name" {
  description = "Name of the queue, the value of SERVICEBUS_QUEUE in the manifests."
  value       = azurerm_servicebus_queue.this.name
}
