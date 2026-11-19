variable "location" {
  description = "Azure region for this root's resources."
  type        = string
  default     = "uksouth"
}

variable "resource_group_name" {
  description = "Resource group for the resources that survive the nightly destroy."
  type        = string
  default     = "rg-aks-v2-persistent"
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

variable "acr_name" {
  description = "Container registry name. Globally unique: 5 to 50 letters and digits. Check with az acr check-name."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9]{5,50}$", var.acr_name))
    error_message = "acr_name must be 5 to 50 letters and digits, with no hyphens."
  }
}

variable "key_vault_name" {
  description = "Key Vault name. Globally unique: 3 to 24 letters, digits and hyphens. Check with az keyvault check-name."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z][a-zA-Z0-9-]{1,22}[a-zA-Z0-9]$", var.key_vault_name)) && !strcontains(var.key_vault_name, "--")
    error_message = "key_vault_name must be 3 to 24 letters, digits and hyphens, start with a letter, end with a letter or digit, and have no double hyphens."
  }
}
