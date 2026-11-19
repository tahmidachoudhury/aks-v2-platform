provider "azurerm" {
  features {
    # Manage the state account through the management plane only. Without
    # this, the provider reads blob data-plane settings before the role
    # assignment below exists, and the first apply can fail with a 403.
    storage {
      data_plane_available = false
    }
  }

  # Never register resource providers from a plan or apply. Register the
  # ones this project needs by hand, once per subscription.
  resource_provider_registrations = "none"

  # subscription_id comes from ARM_SUBSCRIPTION_ID.
}

data "azurerm_client_config" "current" {}

locals {
  tags = {
    project    = "aks-v2-platform"
    managed-by = "opentofu"
    root       = "bootstrap"
  }
}

resource "azurerm_resource_group" "state" {
  name     = var.resource_group_name
  location = var.location
  tags     = local.tags
}

resource "azurerm_storage_account" "state" {
  name                     = var.storage_account_name
  resource_group_name      = azurerm_resource_group.state.name
  location                 = azurerm_resource_group.state.location
  account_kind             = "StorageV2"
  account_tier             = "Standard"
  account_replication_type = "LRS"

  https_traffic_only_enabled      = true
  min_tls_version                 = "TLS1_2"
  allow_nested_items_to_be_public = false
  shared_access_key_enabled       = false
  default_to_oauth_authentication = true

  blob_properties {
    versioning_enabled = true

    delete_retention_policy {
      days = var.soft_delete_retention_days
    }

    container_delete_retention_policy {
      days = var.soft_delete_retention_days
    }
  }

  tags = local.tags

  # This account holds every root's state. Remove this line on purpose, and
  # only at final teardown (P12-01).
  lifecycle {
    prevent_destroy = true
  }
}

# Versioning keeps a copy of every state write. Drop old versions once they
# are past the soft-delete window.
resource "azurerm_storage_management_policy" "state" {
  storage_account_id = azurerm_storage_account.state.id

  rule {
    name    = "expire-old-state-versions"
    enabled = true

    filters {
      blob_types = ["blockBlob"]
    }

    actions {
      version {
        delete_after_days_since_creation = var.state_version_retention_days
      }
    }
  }
}

# storage_account_id makes the provider create the container through the
# management plane, so it does not depend on the role assignment below.
resource "azurerm_storage_container" "state" {
  name                  = var.container_name
  storage_account_id    = azurerm_storage_account.state.id
  container_access_type = "private"
}

# Owner on the subscription does not grant blob data access. The backend
# reads and leases state blobs as the caller, so the caller needs this role.
resource "azurerm_role_assignment" "state_blob_contributor" {
  scope                = azurerm_storage_account.state.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = data.azurerm_client_config.current.object_id
}
