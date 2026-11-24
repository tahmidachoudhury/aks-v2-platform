provider "azurerm" {
  features {}

  # Never register resource providers from a plan or apply, as in platform.
  resource_provider_registrations = "none"

  subscription_id = var.subscription_id
}

# The cluster has local accounts disabled, so the providers sign in to the
# API server with Entra ID, as kubectl does: kubelogin exchanges the az CLI
# login for a token. The server ID is the AKS Entra ID server application,
# the same for every AKS cluster. The data source gives the address and CA,
# no credential.
locals {
  kube_host = data.azurerm_kubernetes_cluster.this.kube_config[0].host
  kube_ca   = base64decode(data.azurerm_kubernetes_cluster.this.kube_config[0].cluster_ca_certificate)
  kube_exec = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "kubelogin"
    args        = ["get-token", "--login", "azurecli", "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630"]
  }
}

provider "kubernetes" {
  host                   = local.kube_host
  cluster_ca_certificate = local.kube_ca

  exec {
    api_version = local.kube_exec.api_version
    command     = local.kube_exec.command
    args        = local.kube_exec.args
  }
}

# A Helm repository config and cache of our own, in the repo's gitignored
# .helm/, so a stale entry in the operator's own Helm config can't fail the
# install. abspath, so it is the same path from make and from plain tofu.
provider "helm" {
  repository_config_path = "${abspath(path.module)}/../../.helm/repositories.yaml"
  repository_cache       = "${abspath(path.module)}/../../.helm/cache"

  kubernetes = {
    host                   = local.kube_host
    cluster_ca_certificate = local.kube_ca
    exec                   = local.kube_exec
  }
}
