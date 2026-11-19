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
}

variable "key_vault_name" {
  description = "Key Vault name. Globally unique: 3 to 24 letters, digits and hyphens. Check with az keyvault check-name."
  type        = string
}

variable "dns_zone_name" {
  description = "Public DNS zone, delegated from the parent zone by hand. See modules/dns/README.md."
  type        = string
  default     = "azure.order-fulfilment.tahmidchoudhury.uk"
}

variable "github_repository" {
  description = "GitHub repository whose workflows sign in to Azure, as owner/name."
  type        = string
  default     = "tahmidachoudhury/aks-v2-platform"
}

variable "state_storage_account_name" {
  description = "State storage account from bootstrap. The same value as storage_account_name in infra/backend.hcl."
  type        = string
}

variable "state_resource_group_name" {
  description = "Resource group of the state storage account."
  type        = string
  default     = "rg-aks-v2-tfstate"
}
