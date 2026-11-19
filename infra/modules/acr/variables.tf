variable "name" {
  description = "Registry name. Globally unique: 5 to 50 letters and digits."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z0-9]{5,50}$", var.name))
    error_message = "name must be 5 to 50 letters and digits, with no hyphens."
  }
}

variable "resource_group_name" {
  description = "Resource group for the registry."
  type        = string
}

variable "location" {
  description = "Azure region for the registry."
  type        = string
}

variable "tags" {
  description = "Tags for the registry."
  type        = map(string)
}
