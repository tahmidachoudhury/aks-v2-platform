# Design and the reason for each setting: see README.md.

resource "azurerm_kubernetes_cluster" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  location            = var.location
  dns_prefix          = var.name
  node_resource_group = var.node_resource_group
  kubernetes_version  = var.kubernetes_version
  sku_tier            = "Free"

  default_node_pool {
    name           = "system"
    vm_size        = var.system_node_vm_size
    node_count     = 3
    zones          = ["1", "2", "3"]
    vnet_subnet_id = var.node_subnet_id
    os_sku         = "AzureLinux"

    # azurerm 4.x changes size, disk and zones by rotating through a
    # temporary pool rather than replacing the cluster.
    temporary_name_for_rotation = "systemtmp"

    # No temp disk on v6 sizes, so no ephemeral OS disk. A 64 GB managed
    # disk bills as a P6 instead of the P10 (128 GB) AKS would pick.
    os_disk_type    = "Managed"
    os_disk_size_gb = 64

    # Taints the pool CriticalAddonsOnly=true:NoSchedule. User workloads go
    # to Node Auto Provisioning nodes.
    only_critical_addons_enabled = true

    # One extra node during an upgrade: 2 vCPUs on top of the system pool's 6.
    upgrade_settings {
      max_surge = "1"
    }

    tags = var.tags
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin      = "azure"
    network_plugin_mode = "overlay"
    network_data_plane  = "cilium"
    network_policy      = "cilium"
    load_balancer_sku   = "standard"

    # Egress through the outbound rules of the cluster's Standard Load
    # Balancer, on one managed public IP. The node subnet has no default
    # outbound access, so this is the only way out. No NAT Gateway: see the
    # Egress section of README.md.
    outbound_type = "loadBalancer"

    load_balancer_profile {
      managed_outbound_ip_count = 1
    }

    pod_cidr       = var.pod_cidr
    service_cidr   = var.service_cidr
    dns_service_ip = var.dns_service_ip
  }

  # Entra ID sign-in, authorised by Azure RBAC role assignments. No local
  # admin kubeconfig.
  azure_active_directory_role_based_access_control {
    tenant_id          = var.tenant_id
    azure_rbac_enabled = true
  }
  local_account_disabled = true

  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  # Node Auto Provisioning (Karpenter) for user workloads. Needs a managed
  # identity, a Standard Load Balancer in a BYO VNet, and Azure CNI, with
  # Overlay and Cilium recommended. "None" skips the default NodePools: ours
  # live in k8s/node-pools and ArgoCD applies them.
  node_provisioning_profile {
    mode               = "Auto"
    default_node_pools = "None"
  }

  # Azure Disk CSI and the snapshot controller back the Postgres and Redis
  # volumes and their VolumeSnapshots. Both are on by default; set here so
  # the intent is visible. Nothing uses Azure Files or Blob, so those
  # drivers are off. No key_vault_secrets_provider block: secrets come from
  # External Secrets Operator, see docs/architecture-decisions.md.
  storage_profile {
    disk_driver_enabled         = true
    snapshot_controller_enabled = true
    file_driver_enabled         = false
    blob_driver_enabled         = false
  }

  tags = var.tags
}

# BYO subnet: the cluster identity joins node NICs and load balancer frontends
# to it. A system-assigned identity only exists once the cluster does, so this
# assignment follows the cluster.
resource "azurerm_role_assignment" "cluster_network_contributor" {
  scope                = var.node_subnet_id
  role_definition_name = "Network Contributor"
  principal_id         = azurerm_kubernetes_cluster.this.identity[0].principal_id
  principal_type       = "ServicePrincipal"
}

resource "azurerm_role_assignment" "cluster_admin" {
  for_each = toset(var.cluster_admin_object_ids)

  scope                = azurerm_kubernetes_cluster.this.id
  role_definition_name = "Azure Kubernetes Service RBAC Cluster Admin"
  principal_id         = each.value
}
