variable "name" {
  description = "Vault name. Globally unique: 3 to 24 letters, digits and hyphens, starting with a letter."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z][a-zA-Z0-9-]{1,22}[a-zA-Z0-9]$", var.name)) && !strcontains(var.name, "--")
    error_message = "name must be 3 to 24 letters, digits and hyphens, start with a letter, end with a letter or digit, and have no double hyphens."
  }
}

variable "resource_group_name" {
  description = "Resource group for the vault."
  type        = string
}

variable "location" {
  description = "Azure region for the vault."
  type        = string
}

variable "tags" {
  description = "Tags for the vault and its secrets."
  type        = map(string)
}

variable "generated_secrets" {
  description = "Secrets to generate, as name => version. Bump a version to write a new random value for that secret."
  type        = map(number)

  validation {
    condition     = alltrue([for name in keys(var.generated_secrets) : can(regex("^[a-z0-9]+(-[a-z0-9]+)*$", name))])
    error_message = "Secret names must be kebab-case: lowercase letters and digits joined by single hyphens."
  }
}

variable "secrets_officer_object_ids" {
  description = "Entra object IDs that get Key Vault Secrets Officer as well as whoever runs the apply."
  type        = list(string)
  default     = []
}
