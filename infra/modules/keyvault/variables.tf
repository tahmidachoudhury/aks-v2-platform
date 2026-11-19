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

variable "secrets_officers" {
  description = "Principals that get Key Vault Secrets Officer: every person and CI identity that applies. A map of static key to object ID, because a CI identity's ID is unknown until it is created."
  type        = map(string)
}

variable "allowed_ip_ranges" {
  description = "Public IPv4 addresses or CIDR ranges allowed through the vault firewall: everyone who applies from outside the VNet. At least one, or no one outside the VNet can write the secrets."
  type        = list(string)

  validation {
    condition     = length(var.allowed_ip_ranges) > 0 && alltrue([for r in var.allowed_ip_ranges : can(cidrhost(strcontains(r, "/") ? r : "${r}/32", 0))])
    error_message = "allowed_ip_ranges needs at least one entry, each an IPv4 address or CIDR range."
  }
}
