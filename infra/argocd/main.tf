# Argo CD, and the root Application that points it at argocd/apps. Everything
# else in the cluster comes from Git through that root. See README.md.

data "azurerm_kubernetes_cluster" "this" {
  name                = var.cluster_name
  resource_group_name = var.platform_resource_group_name
}

# The values that stay out of the public repo. Each is read by name here, as
# platform reads persistent (../README.md), and handed to the root
# Application as Helm values. They live in this root's state and in the
# cluster, never in Git.

data "azurerm_container_registry" "acr" {
  name                = var.acr_name
  resource_group_name = var.persistent_resource_group_name
}

data "azurerm_key_vault" "this" {
  name                = var.key_vault_name
  resource_group_name = var.persistent_resource_group_name
}

data "azurerm_dns_zone" "this" {
  name                = var.dns_zone_name
  resource_group_name = var.persistent_resource_group_name
}

data "azurerm_servicebus_namespace" "this" {
  name                = var.servicebus_namespace_name
  resource_group_name = var.platform_resource_group_name
}

data "azurerm_servicebus_queue" "this" {
  name         = var.servicebus_queue_name
  namespace_id = data.azurerm_servicebus_namespace.this.id
}

# Keep in step with the names in infra/persistent/main.tf.
data "azurerm_user_assigned_identity" "workload" {
  for_each = toset([
    "order-service",
    "payment-service",
    "shipping-service",
    "worker",
    "external-dns",
    "external-secrets",
    "keda",
  ])

  name                = "id-${each.key}"
  resource_group_name = var.persistent_resource_group_name
}

# Restricted, like apps, data and platform. The chart's pods all run as
# non-root with no capabilities and the RuntimeDefault seccomp profile.
resource "kubernetes_namespace_v1" "argocd" {
  metadata {
    name = "argocd"
    labels = {
      "pod-security.kubernetes.io/enforce" = "restricted"
      "pod-security.kubernetes.io/audit"   = "restricted"
      "pod-security.kubernetes.io/warn"    = "restricted"
    }
  }
}

resource "helm_release" "argocd" {
  name       = "argocd"
  repository = "https://argoproj.github.io/argo-helm"
  chart      = "argo-cd"
  version    = "10.17.1"
  namespace  = kubernetes_namespace_v1.argocd.metadata[0].name

  values = [
    file("${path.module}/argo-cd-values.yaml"),
    yamlencode({
      configs = {
        repositories = {
          aks-v2-platform = { name = "aks-v2-platform", type = "git", url = var.repo_url }
        }
      }
    }),
  ]

  wait    = true
  timeout = 600
}
