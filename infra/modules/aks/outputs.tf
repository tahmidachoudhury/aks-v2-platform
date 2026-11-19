output "name" {
  description = "Name of the cluster."
  value       = azurerm_kubernetes_cluster.this.name
}

output "id" {
  description = "ID of the cluster."
  value       = azurerm_kubernetes_cluster.this.id
}

output "oidc_issuer_url" {
  description = "OIDC issuer URL, for workload identity federated credentials."
  value       = azurerm_kubernetes_cluster.this.oidc_issuer_url
}

output "kubelet_identity_object_id" {
  description = "Object ID of the kubelet identity, for AcrPull (P3-11)."
  value       = azurerm_kubernetes_cluster.this.kubelet_identity[0].object_id
}

output "node_resource_group" {
  description = "Resource group AKS manages for nodes, disks and the load balancer."
  value       = azurerm_kubernetes_cluster.this.node_resource_group
}
