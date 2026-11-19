variable "name" {
  description = "Name of the Log Analytics workspace. Unique within the resource group."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group for the workspace."
  type        = string
}

variable "location" {
  description = "Azure region for the workspace."
  type        = string
}

variable "tags" {
  description = "Tags for the workspace."
  type        = map(string)
}

variable "daily_quota_gb" {
  description = "Daily ingestion cap in GB. Ingestion stops for the rest of the day when it is reached."
  type        = number
}

variable "aks_cluster_id" {
  description = "ID of the AKS cluster whose control plane logs go to the workspace."
  type        = string
}
