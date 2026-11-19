variable "name" {
  description = "Namespace name. Globally unique. Forms the FQDN <name>.servicebus.windows.net."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group for the namespace."
  type        = string
}

variable "location" {
  description = "Azure region for the namespace."
  type        = string
}

variable "tags" {
  description = "Tags for the namespace."
  type        = map(string)
}

variable "queue_name" {
  description = "Name of the queue every service sends to and the worker receives from."
  type        = string
}

variable "allowed_ip_addresses" {
  description = "Public IPv4 addresses or CIDRs the firewall allows. Every other address is denied."
  type        = list(string)

  validation {
    condition     = length(var.allowed_ip_addresses) > 0
    error_message = "allowed_ip_addresses must hold at least one address. The firewall denies by default and Azure rejects Deny with no rules."
  }
}
