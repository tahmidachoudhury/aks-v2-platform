variable "names" {
  description = "Workloads that sign in to Azure, named as their Kubernetes ServiceAccount. Each gets an identity called id-<name>."
  type        = list(string)
}

variable "resource_group_name" {
  description = "Resource group for the identities."
  type        = string
}

variable "location" {
  description = "Azure region for the identities."
  type        = string
}

variable "tags" {
  description = "Tags for the identities."
  type        = map(string)
}
