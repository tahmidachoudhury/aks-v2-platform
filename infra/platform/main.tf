# azurerm has no provider-level default tags, so every resource in this root
# sets tags = local.tags. Keep the keys the same in every root so Cost
# Management can group and filter by them.
data "azurerm_client_config" "current" {}

# The CI infra identity from persistent, read by name like the other
# persistent resources. See ../README.md.
data "azurerm_user_assigned_identity" "github_infra" {
  name                = var.github_infra_identity_name
  resource_group_name = var.identity_resource_group_name
}

# The registry from persistent, read by name the same way.
data "azurerm_container_registry" "acr" {
  name                = var.acr_name
  resource_group_name = var.persistent_resource_group_name
}

# The private endpoint targets (P3-14). The vault is in persistent. The state
# account is in bootstrap, looked up with azurerm_resources as persistent
# does, so its account keys never land in this root's state.
data "azurerm_key_vault" "this" {
  name                = var.key_vault_name
  resource_group_name = var.persistent_resource_group_name
}

data "azurerm_resources" "state_account" {
  name                = var.state_storage_account_name
  resource_group_name = var.state_resource_group_name
  type                = "Microsoft.Storage/storageAccounts"

  lifecycle {
    postcondition {
      condition     = length(self.resources) == 1
      error_message = "No storage account named ${var.state_storage_account_name} in ${var.state_resource_group_name}. Set state_storage_account_name to storage_account_name from infra/backend.hcl."
    }
  }
}

locals {
  tags = {
    project    = "aks-v2-platform"
    managed-by = "opentofu"
    root       = "platform"
    env        = var.environment
    owner      = var.owner
  }
}

resource "azurerm_resource_group" "platform" {
  name     = var.resource_group_name
  location = var.location
  tags     = local.tags
}

module "network" {
  source = "../modules/network"

  name                     = var.vnet_name
  resource_group_name      = azurerm_resource_group.platform.name
  location                 = azurerm_resource_group.platform.location
  tags                     = local.tags
  address_space            = var.vnet_address_space
  aks_nodes_prefix         = var.aks_nodes_prefix
  private_endpoints_prefix = var.private_endpoints_prefix
  pod_cidr                 = var.pod_cidr

  # Only zones that will hold records. ACR Basic and Service Bus Standard
  # have no private endpoints, so they get no zone: see the network README.
  private_dns_zones = {
    key_vault = "privatelink.vaultcore.azure.net"
    blob      = "privatelink.blob.core.windows.net"
  }
}

# Private endpoints for Key Vault and the state account's blob service, keyed
# like the DNS zones above. Each takes an IP in snet-private-endpoints and
# writes its A record into its zone, so names resolve privately inside the
# VNet. They go with the VNet every evening, which leaves both resources on
# their public path overnight. What that path allows is in the README.
locals {
  private_endpoints = {
    key_vault = {
      name        = "pe-key-vault"
      resource_id = data.azurerm_key_vault.this.id
      subresource = "vault"
    }
    blob = {
      name        = "pe-state-blob"
      resource_id = data.azurerm_resources.state_account.resources[0].id
      subresource = "blob"
    }
  }
}

resource "azurerm_private_endpoint" "this" {
  for_each = local.private_endpoints

  name                = each.value.name
  resource_group_name = azurerm_resource_group.platform.name
  location            = azurerm_resource_group.platform.location
  subnet_id           = module.network.subnet_ids.private_endpoints
  tags                = local.tags

  # Approved on create, which needs the approval permission on the target:
  # the operators have it on both. CI does not on the state account, see the
  # README.
  private_service_connection {
    name                           = each.value.name
    private_connection_resource_id = each.value.resource_id
    subresource_names              = [each.value.subresource]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "default"
    private_dns_zone_ids = [module.network.private_dns_zone_ids[each.key]]
  }
}

module "aks" {
  source = "../modules/aks"

  name                = var.cluster_name
  resource_group_name = azurerm_resource_group.platform.name
  location            = azurerm_resource_group.platform.location
  tags                = local.tags
  kubernetes_version  = var.kubernetes_version
  node_resource_group = var.node_resource_group_name
  node_subnet_id      = module.network.subnet_ids.aks_nodes
  system_node_vm_size = var.system_node_vm_size
  pod_cidr            = var.pod_cidr
  service_cidr        = var.service_cidr
  dns_service_ip      = var.dns_service_ip

  tenant_id = data.azurerm_client_config.current.tenant_id

  # A fixed list, not whoever runs the apply, so a person and CI applying
  # in turn don't swap each other's access.
  cluster_admin_object_ids = distinct(concat(
    var.operator_object_ids,
    [data.azurerm_user_assigned_identity.github_infra.principal_id],
  ))

  # Nodes must not boot into the subnet before its NSG and the Overlay rule
  # are attached. The subnet ID alone doesn't make the cluster wait for them.
  depends_on = [module.network]
}

# The kubelet pulls images as its own identity, so pods need no
# imagePullSecret. AKS creates that identity with the cluster and deletes it
# with the cluster, so the grant lives here and goes with it every evening.
# In persistent it would outlive its principal and pile up as an orphaned
# assignment, one a day. AcrPull only: no push, no delete.
resource "azurerm_role_assignment" "kubelet_acr_pull" {
  scope                = data.azurerm_container_registry.acr.id
  role_definition_name = "AcrPull"
  principal_id         = module.aks.kubelet_identity_object_id

  # Set explicitly so CI's ABAC condition, which only lets it grant roles to
  # service principals, can see the type (see modules/github-oidc).
  principal_type = "ServicePrincipal"
}

module "servicebus" {
  source = "../modules/servicebus"

  name                = var.servicebus_namespace_name
  resource_group_name = azurerm_resource_group.platform.name
  location            = azurerm_resource_group.platform.location
  tags                = local.tags
  queue_name          = var.servicebus_queue_name

  # Only the cluster's egress IP gets through the firewall. It changes every
  # morning, so the namespace is created after the cluster and reads it then.
  allowed_ip_addresses = module.aks.outbound_ip_addresses
}

module "loganalytics" {
  source = "../modules/loganalytics"

  name                = var.log_analytics_workspace_name
  resource_group_name = azurerm_resource_group.platform.name
  location            = azurerm_resource_group.platform.location
  tags                = local.tags
  daily_quota_gb      = var.log_analytics_daily_quota_gb
  aks_cluster_id      = module.aks.id
}

# Workload identity federation (P3-16, P3-18) and the Service Bus roles
# (P3-17). The identities are in persistent, so their client IDs survive the
# night. Their federated credentials name this cluster's OIDC issuer, which
# is new every morning, so they live here. Role matrix and naming:
# ../modules/workload-identity/README.md.

locals {
  # The ServiceAccount each identity trusts, keyed by the name persistent
  # gives the identity (id-<key>). The namespaces are a convention until
  # P4-07 creates them: apps for the services, platform for the components.
  # Every key must also be in the names list of module.workload_identity in
  # infra/persistent/main.tf, or the identity lookup fails with "not found".
  workload_identities = {
    order-service    = { namespace = "apps", service_account = "order-service" }
    payment-service  = { namespace = "apps", service_account = "payment-service" }
    shipping-service = { namespace = "apps", service_account = "shipping-service" }
    worker           = { namespace = "apps", service_account = "worker" }
    external-dns     = { namespace = "platform", service_account = "external-dns" }
    external-secrets = { namespace = "platform", service_account = "external-secrets" }
  }

  # The three publishers send and the worker receives, all on one queue.
  servicebus_roles = {
    order-service    = "Azure Service Bus Data Sender"
    payment-service  = "Azure Service Bus Data Sender"
    shipping-service = "Azure Service Bus Data Sender"
    worker           = "Azure Service Bus Data Receiver"
  }
}

data "azurerm_user_assigned_identity" "workload" {
  for_each = local.workload_identities

  name                = "id-${each.key}"
  resource_group_name = var.workload_identity_resource_group_name
}

# One credential per identity, so Azure's limit on concurrent credential
# writes to one identity never bites. Destroyed with the cluster each
# evening, so yesterday's issuer is never left trusted.
resource "azurerm_federated_identity_credential" "workload" {
  for_each = local.workload_identities

  name                      = module.aks.name
  user_assigned_identity_id = data.azurerm_user_assigned_identity.workload[each.key].id
  issuer                    = module.aks.oidc_issuer_url
  audience                  = ["api://AzureADTokenExchange"]
  subject                   = "system:serviceaccount:${each.value.namespace}:${each.value.service_account}"
}

# Scoped to the queue, not the namespace: it is the only entity, and a role
# on it also covers its dead-letter queue.
resource "azurerm_role_assignment" "servicebus" {
  for_each = local.servicebus_roles

  scope                = module.servicebus.queue_id
  role_definition_name = each.value
  principal_id         = data.azurerm_user_assigned_identity.workload[each.key].principal_id
  principal_type       = "ServicePrincipal"
}
