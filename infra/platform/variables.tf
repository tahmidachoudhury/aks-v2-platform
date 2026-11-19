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
  description = "Azure CNI Overlay pod CIDR. Must not overlap the VNet or the service CIDR. P3-07 passes it to AKS."
  type        = string
  default     = "10.244.0.0/16"
}
