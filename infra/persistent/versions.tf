terraform {
  # 1.11 is the first release with ephemeral resources and write-only
  # attributes. The Key Vault module needs both to keep secret values out of
  # state and plan files.
  required_version = ">= 1.11.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.81"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }
}
