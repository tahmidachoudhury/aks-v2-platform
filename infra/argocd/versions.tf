terraform {
  required_version = ">= 1.8.0"

  required_providers {
    # 5.x here, while persistent and platform stay on 4.81. In 4.x the
    # azurerm_kubernetes_cluster data source panics on a cluster with Node
    # Auto Provisioning, reading the NAP pools' nil OS type
    # (github.com/hashicorp/terraform-provider-azurerm/issues/33604). 5.x has
    # the fix. This root only reads with azurerm, so the major version gap
    # touches nothing it creates.
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 5.15"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.3"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.4"
    }
  }
}
