# azurerm has no provider-level default tags, so every resource in this root
# sets tags = local.tags. Keep the keys the same in every root so Cost
# Management can group and filter by them.
data "azurerm_client_config" "current" {}

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

  # Whoever runs the apply can use kubectl straight away, plus any fixed
  # admins, so a second operator or CI applying does not remove the first.
  cluster_admin_object_ids = distinct(concat(
    [data.azurerm_client_config.current.object_id],
    var.extra_cluster_admin_object_ids,
  ))

  # Nodes must not boot into the subnet before its NSG and the Overlay rule
  # are attached. The subnet ID alone doesn't make the cluster wait for them.
  depends_on = [module.network]
}
