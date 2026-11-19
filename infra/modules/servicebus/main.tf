# Design and the reason for each setting: see README.md.

resource "azurerm_servicebus_namespace" "this" {
  name                = var.name
  resource_group_name = var.resource_group_name
  location            = var.location
  sku                 = "Standard"

  # Entra ID only. No SAS keys or connection strings: pods use Workload
  # Identity, and the data plane roles come in P3-17.
  local_auth_enabled = false

  # Standard has no private endpoint, so the public endpoint stays on and
  # the IP firewall limits it to the addresses passed in.
  public_network_access_enabled = true

  network_rule_set {
    default_action                = "Deny"
    public_network_access_enabled = true
    ip_rules                      = var.allowed_ip_addresses
    trusted_services_allowed      = false
  }

  tags = var.tags
}

# Settings match the local emulator (app/servicebus-emulator/Config.json), so
# a message behaves the same in compose and on AKS.
resource "azurerm_servicebus_queue" "this" {
  name         = var.queue_name
  namespace_id = azurerm_servicebus_namespace.this.id

  # The third failed delivery moves a message to the dead-letter queue.
  max_delivery_count = 3

  # A message nobody completes within an hour expires into the DLQ rather
  # than vanishing.
  default_message_ttl                  = "PT1H"
  dead_lettering_on_message_expiration = true

  # The worker's handlers take milliseconds. A minute is the Azure default
  # and gives a stuck handler time before Service Bus redelivers.
  lock_duration = "PT1M"
}
