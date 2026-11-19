# Design, categories and cost: see README.md.

resource "azurerm_log_analytics_workspace" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  location            = var.location
  sku                 = "PerGB2018"
  retention_in_days   = 30

  # Ingestion stops for the rest of the day once this many GB arrive. It
  # caps the bill if a category turns out noisier than expected.
  daily_quota_gb = var.daily_quota_gb

  # Entra ID only for queries and ingestion. No agent here uses the shared
  # workspace keys.
  local_authentication_enabled = false

  tags = var.tags
}

# AKS control plane logs. Container Insights stays off: New Relic collects
# container logs and metrics.
resource "azurerm_monitor_diagnostic_setting" "aks" {
  name                       = "aks-control-plane"
  target_resource_id         = var.aks_cluster_id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  # Resource-specific tables (AKSAuditAdmin, AKSControlPlane) rather than
  # AzureDiagnostics: smaller rows and simpler queries.
  log_analytics_destination_type = "Dedicated"

  # Changes to the cluster: kube-audit without get and list.
  enabled_log {
    category = "kube-audit-admin"
  }

  # Entra ID sign-in and Azure RBAC decisions for kubectl.
  enabled_log {
    category = "guard"
  }

  # Node Auto Provisioning decisions: why a node was or wasn't created.
  enabled_log {
    category = "karpenter-events"
  }
}
