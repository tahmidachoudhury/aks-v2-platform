output "resource_group_name" {
  description = "Name of this root's resource group."
  value       = azurerm_resource_group.platform.name
}

output "resource_group_id" {
  description = "ID of this root's resource group."
  value       = azurerm_resource_group.platform.id
}

output "vnet_name" {
  description = "Name of the platform VNet, for private DNS zone links."
  value       = module.network.vnet_name
}

output "vnet_id" {
  description = "ID of the platform VNet."
  value       = module.network.vnet_id
}

output "subnet_ids" {
  description = "Subnet IDs by function."
  value       = module.network.subnet_ids
}

output "private_dns_zone_ids" {
  description = "Private DNS zone IDs by short name (key_vault, blob), for the private endpoints in P3-14."
  value       = module.network.private_dns_zone_ids
}

output "aks_cluster_name" {
  description = "Name of the AKS cluster."
  value       = module.aks.name
}

output "aks_cluster_id" {
  description = "ID of the AKS cluster."
  value       = module.aks.id
}

output "oidc_issuer_url" {
  description = "Cluster OIDC issuer URL, for workload identity federated credentials."
  value       = module.aks.oidc_issuer_url
}

output "kubelet_identity_object_id" {
  description = "Object ID of the kubelet identity, for the AcrPull grant (P3-11)."
  value       = module.aks.kubelet_identity_object_id
}

output "aks_node_resource_group" {
  description = "Resource group AKS manages for nodes, disks and the load balancer."
  value       = module.aks.node_resource_group
}

output "kubeconfig_command" {
  description = "Fetches a kubeconfig that signs in with Entra ID through kubelogin. Holds no secret."
  value       = "az aks get-credentials --resource-group ${azurerm_resource_group.platform.name} --name ${module.aks.name} --overwrite-existing && kubelogin convert-kubeconfig -l azurecli"
}

output "servicebus_namespace_id" {
  description = "ID of the Service Bus namespace, the scope for the data plane roles in P3-17."
  value       = module.servicebus.id
}

output "servicebus_namespace_name" {
  description = "Name of the Service Bus namespace."
  value       = module.servicebus.name
}

output "servicebus_fqdn" {
  description = "Fully qualified Service Bus namespace, for SERVICEBUS_NAMESPACE in the manifests."
  value       = module.servicebus.fqdn
}

output "servicebus_queue_name" {
  description = "Service Bus queue name, for SERVICEBUS_QUEUE in the manifests."
  value       = module.servicebus.queue_name
}

output "egress_ip_addresses" {
  description = "Public IPs the cluster's egress leaves from. New every morning."
  value       = module.aks.outbound_ip_addresses
}

output "log_analytics_workspace_id" {
  description = "Resource ID of the Log Analytics workspace."
  value       = module.loganalytics.id
}

output "log_analytics_workspace_guid" {
  description = "Workspace GUID, for az monitor log-analytics query --workspace."
  value       = module.loganalytics.workspace_id
}
