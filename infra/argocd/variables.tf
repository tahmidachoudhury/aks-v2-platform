variable "subscription_id" {
  description = "Subscription of the platform root, where it also reads the persistent resources by name. Set in terraform.tfvars. ExternalDNS gets it too, for the DNS zone."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", var.subscription_id))
    error_message = "subscription_id must be a subscription GUID in lowercase."
  }
}

# Platform resources, read by name. The same values as in infra/platform.

variable "cluster_name" {
  description = "AKS cluster from platform."
  type        = string
  default     = "aks-v2-platform"
}

variable "platform_resource_group_name" {
  description = "Resource group of the cluster and the Service Bus namespace."
  type        = string
  default     = "rg-aks-v2-platform"
}

variable "servicebus_namespace_name" {
  description = "Service Bus namespace, the same value as servicebus_namespace_name in infra/platform/terraform.tfvars."
  type        = string
}

variable "servicebus_queue_name" {
  description = "Queue the services send to and the worker receives from."
  type        = string
  default     = "order-events"
}

# Persistent resources, read by name. The same values as in infra/persistent.

variable "persistent_resource_group_name" {
  description = "Resource group in persistent that holds the registry, the Key Vault, the DNS zone and the workload identities."
  type        = string
  default     = "rg-aks-v2-persistent"
}

variable "acr_name" {
  description = "Container registry, the same value as acr_name in infra/persistent/terraform.tfvars."
  type        = string
}

variable "key_vault_name" {
  description = "Key Vault, the same value as key_vault_name in infra/persistent/terraform.tfvars."
  type        = string
}

variable "dns_zone_name" {
  description = "Public DNS zone ExternalDNS writes to, the same value as dns_zone_name in persistent."
  type        = string
  default     = "azure.order-fulfilment.tahmidchoudhury.uk"
}

# Argo CD.

variable "repo_url" {
  description = "Git repository Argo CD deploys from."
  type        = string
  default     = "https://github.com/tahmidachoudhury/aks-v2-platform.git"
}

variable "target_revision" {
  description = "Branch, tag or commit of repo_url that the root and every child Application track. Set a branch to test it on the cluster before merging."
  type        = string
  default     = "main"
}
