# Three user-assigned identities that GitHub Actions signs in as with OIDC.
# No client secret exists for any of them. See README.md.

data "azurerm_client_config" "current" {}

locals {
  subscription_scope = "/subscriptions/${data.azurerm_client_config.current.subscription_id}"

  # GitHub's token issuer and the audience Entra ID expects for it.
  issuer   = "https://token.actions.githubusercontent.com"
  audience = ["api://AzureADTokenExchange"]
  subject  = "repo:${var.github_repository}"
}

resource "azurerm_user_assigned_identity" "infra" {
  name                = "id-gh-infra"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = var.tags
}

resource "azurerm_user_assigned_identity" "infra_plan" {
  name                = "id-gh-infra-plan"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = var.tags
}

resource "azurerm_user_assigned_identity" "app" {
  name                = "id-gh-app"
  resource_group_name = var.resource_group_name
  location            = var.location
  tags                = var.tags
}

# Federated credentials. Azure rejects concurrent credential writes on one
# identity, so the infra identity's credentials are chained with depends_on.

resource "azurerm_federated_identity_credential" "infra_main" {
  name                      = "github-main"
  user_assigned_identity_id = azurerm_user_assigned_identity.infra.id
  issuer                    = local.issuer
  audience                  = local.audience
  subject                   = "${local.subject}:ref:refs/heads/main"
}

# A job that names a GitHub environment gets an environment subject, not a
# branch one. These are the apply jobs, behind the environment's reviewers.
resource "azurerm_federated_identity_credential" "infra_environment" {
  for_each = toset(var.infra_environments)

  name                      = "github-env-${each.value}"
  user_assigned_identity_id = azurerm_user_assigned_identity.infra.id
  issuer                    = local.issuer
  audience                  = local.audience
  subject                   = "${local.subject}:environment:${each.value}"

  depends_on = [azurerm_federated_identity_credential.infra_main]
}

resource "azurerm_federated_identity_credential" "infra_plan_pull_request" {
  name                      = "github-pull-request"
  user_assigned_identity_id = azurerm_user_assigned_identity.infra_plan.id
  issuer                    = local.issuer
  audience                  = local.audience
  subject                   = "${local.subject}:pull_request"
}

resource "azurerm_federated_identity_credential" "app_main" {
  name                      = "github-main"
  user_assigned_identity_id = azurerm_user_assigned_identity.app.id
  issuer                    = local.issuer
  audience                  = local.audience
  subject                   = "${local.subject}:ref:refs/heads/main"
}

# Infra identity: builds and changes resources in the listed resource groups.

resource "azurerm_role_assignment" "infra_contributor" {
  for_each = var.infra_resource_group_ids

  scope                = each.value
  role_definition_name = "Contributor"
  principal_id         = azurerm_user_assigned_identity.infra.principal_id
  principal_type       = "ServicePrincipal"
}

# The role GUIDs the ABAC condition below allows. Looked up by name so the
# list reads as role names.
data "azurerm_role_definition" "assignable" {
  for_each = toset(var.infra_assignable_roles)

  name  = each.value
  scope = local.subscription_scope
}

# role_definition_id is the full resource ID. GuidEquals wants the bare
# GUID, its last segment.
locals {
  assignable_role_guids = join(", ", sort([
    for r in data.azurerm_role_definition.assignable : basename(r.role_definition_id)
  ]))
}

# Role Based Access Control Administrator, limited by an ABAC condition to
# writing and deleting assignments of the listed roles only. Without the
# condition it could grant Owner to anyone, itself included.
resource "azurerm_role_assignment" "infra_rbac_admin" {
  for_each = var.infra_resource_group_ids

  scope                = each.value
  role_definition_name = "Role Based Access Control Administrator"
  principal_id         = azurerm_user_assigned_identity.infra.principal_id
  principal_type       = "ServicePrincipal"

  condition_version = "2.0"
  condition         = <<-EOT
    (
     (
      !(ActionMatches{'Microsoft.Authorization/roleAssignments/write'})
     )
     OR
     (
      @Request[Microsoft.Authorization/roleAssignments:RoleDefinitionId] ForAnyOfAnyValues:GuidEquals {${local.assignable_role_guids}}
     )
    )
    AND
    (
     (
      !(ActionMatches{'Microsoft.Authorization/roleAssignments/delete'})
     )
     OR
     (
      @Resource[Microsoft.Authorization/roleAssignments:RoleDefinitionId] ForAnyOfAnyValues:GuidEquals {${local.assignable_role_guids}}
     )
    )
  EOT
}

# Owner on the subscription doesn't grant blob data access. The backend
# reads, writes and leases state blobs as the caller.
resource "azurerm_role_assignment" "infra_state" {
  scope                = var.state_storage_account_id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.infra.principal_id
  principal_type       = "ServicePrincipal"
}

# Plans read resources outside the listed groups, such as the public IPs in
# the node resource group AKS creates each morning.
resource "azurerm_role_assignment" "infra_reader" {
  scope                = local.subscription_scope
  role_definition_name = "Reader"
  principal_id         = azurerm_user_assigned_identity.infra.principal_id
  principal_type       = "ServicePrincipal"
}

# Plan identity: reads only. Pull request plans run as this identity with
# -lock=false, since a lease needs write access to the state blob.

resource "azurerm_role_assignment" "infra_plan_reader" {
  scope                = local.subscription_scope
  role_definition_name = "Reader"
  principal_id         = azurerm_user_assigned_identity.infra_plan.principal_id
  principal_type       = "ServicePrincipal"
}

resource "azurerm_role_assignment" "infra_plan_state" {
  scope                = var.state_storage_account_id
  role_definition_name = "Storage Blob Data Reader"
  principal_id         = azurerm_user_assigned_identity.infra_plan.principal_id
  principal_type       = "ServicePrincipal"
}

# Refreshing a Key Vault secret reads it, value included, so a plan of the
# persistent root needs this. See README.md.
resource "azurerm_role_assignment" "infra_plan_secrets" {
  scope                = var.key_vault_id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.infra_plan.principal_id
  principal_type       = "ServicePrincipal"
}

# App identity: pushes images and nothing else.
resource "azurerm_role_assignment" "app_acr_push" {
  scope                = var.acr_id
  role_definition_name = "AcrPush"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
  principal_type       = "ServicePrincipal"
}
