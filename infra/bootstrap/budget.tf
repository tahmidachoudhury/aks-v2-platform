# A second budget for this project. The subscription already has a budget
# managed elsewhere; this one sits alongside it and does not touch it.
data "azurerm_subscription" "current" {}

resource "azurerm_consumption_budget_subscription" "project" {
  name            = "budget-aks-v2-platform"
  subscription_id = data.azurerm_subscription.current.id
  amount          = var.budget_amount
  time_grain      = "Monthly"

  # A fixed date, not timestamp(), so plans stay clean. Azure sets the end
  # date ten years out when none is given.
  time_period {
    start_date = var.budget_start_date
  }

  dynamic "notification" {
    for_each = [50, 80, 100]

    content {
      enabled        = true
      threshold      = notification.value
      threshold_type = "Actual"
      operator       = "GreaterThanOrEqualTo"
      contact_emails = var.budget_contact_emails
    }
  }

  notification {
    enabled        = true
    threshold      = 100
    threshold_type = "Forecasted"
    operator       = "GreaterThanOrEqualTo"
    contact_emails = var.budget_contact_emails
  }
}
