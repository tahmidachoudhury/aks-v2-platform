variable "github_repository" {
  description = "GitHub repository as owner/name. Forms the start of every federated credential subject."
  type        = string
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

variable "infra_environments" {
  description = "GitHub environments whose jobs may sign in as the infra identity. The apply jobs use them."
  type        = list(string)
}

variable "infra_resource_group_ids" {
  description = "Resource groups where the infra identity gets Contributor and conditional RBAC Administrator, keyed by a static label."
  type        = map(string)
}

variable "infra_assignable_roles" {
  description = "Built-in role names the infra identity may assign and remove in those resource groups."
  type        = list(string)
}

variable "state_storage_account_id" {
  description = "ID of the state storage account."
  type        = string
}

variable "acr_id" {
  description = "ID of the container registry. The app identity pushes to it."
  type        = string
}

variable "operator_object_ids" {
  description = "Entra object IDs of the people who apply by hand. The only users the infra identity may assign roles to."
  type        = list(string)

  validation {
    condition     = length(var.operator_object_ids) > 0
    error_message = "operator_object_ids needs at least one object ID."
  }
}
