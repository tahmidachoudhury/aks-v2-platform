variable "location" {
  description = "Azure region for the state resources."
  type        = string
  default     = "uksouth"
}

variable "resource_group_name" {
  description = "Resource group that holds the state storage account."
  type        = string
  default     = "rg-aks-v2-tfstate"
}

variable "storage_account_name" {
  description = "State storage account name. Must be globally unique: 3 to 24 lowercase letters and digits."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]{3,24}$", var.storage_account_name))
    error_message = "storage_account_name must be 3 to 24 lowercase letters and digits."
  }
}

variable "container_name" {
  description = "Blob container that holds the state files, one blob per root."
  type        = string
  default     = "tfstate"
}

variable "soft_delete_retention_days" {
  description = "Days a deleted state blob or container can be restored."
  type        = number
  default     = 14

  validation {
    condition     = var.soft_delete_retention_days >= 1 && var.soft_delete_retention_days <= 365
    error_message = "soft_delete_retention_days must be between 1 and 365."
  }
}
