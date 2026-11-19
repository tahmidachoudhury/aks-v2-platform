variable "location" {
  description = "Azure region for this root's resources."
  type        = string
  default     = "uksouth"
}

variable "resource_group_name" {
  description = "Resource group for the daily stack. Destroyed every evening."
  type        = string
  default     = "rg-aks-v2-platform"
}

variable "environment" {
  description = "Value of the env tag."
  type        = string
  default     = "dev"
}

variable "owner" {
  description = "Value of the owner tag: who to ask about these resources and their cost."
  type        = string
  default     = "tahmidachoudhury"
}

# Address plan: see ../modules/network/README.md.

variable "vnet_name" {
  description = "Name of the VNet."
  type        = string
  default     = "vnet-aks-v2-platform"
}

variable "vnet_address_space" {
  description = "VNet address space."
  type        = string
  default     = "10.20.0.0/16"
}

variable "aks_nodes_prefix" {
  description = "AKS node subnet. One IP per node."
  type        = string
  default     = "10.20.0.0/22"
}

variable "private_endpoints_prefix" {
  description = "Private endpoint subnet."
  type        = string
  default     = "10.20.4.0/24"
}

variable "pod_cidr" {
  description = "Azure CNI Overlay pod CIDR. Must not overlap the VNet or the service CIDR."
  type        = string
  default     = "10.244.0.0/16"
}

variable "service_cidr" {
  description = "Kubernetes service CIDR. Must not overlap the VNet or the pod CIDR."
  type        = string
  default     = "10.0.0.0/16"
}

variable "dns_service_ip" {
  description = "ClusterIP of CoreDNS. Must sit inside service_cidr."
  type        = string
  default     = "10.0.0.10"
}

# AKS: see ../modules/aks/README.md.

variable "cluster_name" {
  description = "Name of the AKS cluster."
  type        = string
  default     = "aks-v2-platform"
}

variable "node_resource_group_name" {
  description = "Resource group AKS creates for nodes, disks and the load balancer. Goes when the cluster goes."
  type        = string
  default     = "rg-aks-v2-platform-nodes"
}

variable "kubernetes_version" {
  description = "Kubernetes minor version. AKS picks its latest patch at create time, so each morning's cluster gets the newest patch."
  type        = string
  default     = "1.37"
}

variable "system_node_vm_size" {
  description = "System pool VM size. Must be offered to this subscription in zones 1, 2 and 3 of the region."
  type        = string
  default     = "Standard_D2nls_v6"
}

variable "extra_cluster_admin_object_ids" {
  description = "Entra object IDs that get AKS RBAC Cluster Admin as well as whoever runs the apply."
  type        = list(string)
  default     = []
}

# Service Bus: see ../modules/servicebus/README.md.

variable "servicebus_namespace_name" {
  description = "Service Bus namespace name. Globally unique, and kept the same every day so the FQDN in the manifests holds. Check with az servicebus namespace exists."
  type        = string

  validation {
    condition = (
      can(regex("^[a-zA-Z][a-zA-Z0-9-]{4,48}[a-zA-Z0-9]$", var.servicebus_namespace_name))
      && !can(regex("(-sb|-mgmt)$", var.servicebus_namespace_name))
    )
    error_message = "servicebus_namespace_name must be 6 to 50 letters, digits and hyphens, start with a letter, end with a letter or digit, and not end in -sb or -mgmt."
  }
}

variable "servicebus_queue_name" {
  description = "Queue every service sends to and the worker receives from. Matches SERVICEBUS_QUEUE in the app."
  type        = string
  default     = "order-events"
}
