variable "name" {
  description = "Name of the AKS cluster. Also used as its DNS prefix."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group for the cluster resource."
  type        = string
}

variable "location" {
  description = "Azure region for the cluster."
  type        = string
}

variable "tags" {
  description = "Tags for the cluster and its system node pool."
  type        = map(string)
}

variable "kubernetes_version" {
  description = "Kubernetes minor version, such as 1.37. AKS picks the latest patch of that minor when it creates the cluster."
  type        = string
}

variable "node_resource_group" {
  description = "Name of the resource group AKS creates for nodes, disks and the load balancer. Must not exist yet."
  type        = string
}

variable "node_subnet_id" {
  description = "Subnet for every node: the system pool and Node Auto Provisioning nodes."
  type        = string
}

variable "system_node_vm_size" {
  description = "VM size of the system pool. Must be offered in zones 1, 2 and 3, with at least 2 vCPUs and 4 GiB."
  type        = string
}

variable "pod_cidr" {
  description = "Azure CNI Overlay pod CIDR. Outside the VNet."
  type        = string
}

variable "service_cidr" {
  description = "Kubernetes service CIDR. Must not overlap the VNet or the pod CIDR."
  type        = string
}

variable "dns_service_ip" {
  description = "ClusterIP of CoreDNS. Must sit inside service_cidr."
  type        = string
}

variable "cluster_admin_object_ids" {
  description = "Entra object IDs granted Azure Kubernetes Service RBAC Cluster Admin on the cluster."
  type        = list(string)
}

variable "tenant_id" {
  description = "Entra tenant that signs users in to the cluster."
  type        = string
}
