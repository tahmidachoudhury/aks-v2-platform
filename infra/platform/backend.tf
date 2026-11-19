# Partial config. The shared settings come from infra/backend.hcl:
#   tofu init -backend-config=../backend.hcl
terraform {
  backend "azurerm" {
    key = "platform.tfstate"
  }
}
