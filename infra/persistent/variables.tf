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
