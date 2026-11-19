variable "subscription_id" {
  description = "Subscription to deploy into. Set in terraform.tfvars, so the target never falls back to ARM_SUBSCRIPTION_ID or the az CLI default."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", var.subscription_id))
    error_message = "subscription_id must be a subscription GUID in lowercase."
  }
}

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
  description = "GitHub repository whose workflows sign in to Azure, written as it appears in the OIDC subject. The repo uses immutable subjects, so that is owner@owner_id/name@repo_id."
  type        = string
  default     = "tahmidachoudhury@127567208/aks-v2-platform@1382383802"
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

variable "identity_resource_group_name" {
  description = "Resource group for the GitHub Actions identities. CI has no write role on it."
  type        = string
  default     = "rg-aks-v2-identity"
}

variable "operator_object_ids" {
  description = "Entra object IDs of the people who run applies by hand. They get Key Vault Secrets Officer. Set in terraform.tfvars, never committed."
  type        = list(string)
}
