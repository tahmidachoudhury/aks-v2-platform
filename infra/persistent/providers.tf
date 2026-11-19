provider "azurerm" {
  features {}

  # Never register resource providers from a plan or apply. Register the
  # ones this project needs by hand, once per subscription.
  resource_provider_registrations = "none"

  # subscription_id comes from ARM_SUBSCRIPTION_ID.
}
