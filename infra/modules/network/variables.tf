variable "name" {
  description = "Name of the VNet."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group for the VNet, subnets and NSGs."
  type        = string
}

variable "location" {
  description = "Azure region for the VNet and NSGs."
  type        = string
}

variable "tags" {
  description = "Tags for every taggable resource in the module."
  type        = map(string)
}

variable "address_space" {
  description = "VNet address space. Must not overlap pod_cidr or the AKS service CIDR."
  type        = string
}

variable "aks_nodes_prefix" {
  description = "CIDR of the AKS node subnet. One IP per node."
  type        = string
}

variable "private_endpoints_prefix" {
  description = "CIDR of the private endpoint subnet."
  type        = string
}

variable "pod_cidr" {
  description = "Azure CNI Overlay pod CIDR. Outside the VNet. The node NSG allows traffic from it."
  type        = string
}

variable "private_dns_zones" {
  description = "Private DNS zones to create and link to the VNet, keyed by a short name such as key_vault. Values are zone names such as privatelink.vaultcore.azure.net."
  type        = map(string)
  default     = {}
}
