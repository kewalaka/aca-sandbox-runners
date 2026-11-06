# Holds the controller's GitHub credential. Terraform never sees the secret value: an operator puts it in the vault
# (scripts/create-github-app.ps1 for the App key) and the container app references it through its managed identity.
locals {
  kv_secrets_user_role_id    = "/subscriptions/${data.azapi_client_config.current.subscription_id}/providers/Microsoft.Authorization/roleDefinitions/4633458b-17de-408a-b874-0445c86b69e6"
  kv_secrets_officer_role_id = "/subscriptions/${data.azapi_client_config.current.subscription_id}/providers/Microsoft.Authorization/roleDefinitions/b86a8fe4-44ce-4948-aee5-eccb2c155cd7"
}

resource "azapi_resource" "vault" {
  type                   = "Microsoft.KeyVault/vaults@2026-02-01"
  name                   = "kv-${var.name_prefix}-${substr(data.azapi_client_config.current.subscription_id, 0, 8)}"
  location               = var.location
  parent_id              = azapi_resource.rg.id
  response_export_values = ["properties.vaultUri"]
  tags                   = var.tags
  body = {
    properties = {
      tenantId                  = data.azapi_client_config.current.tenant_id
      sku                       = { family = "A", name = "standard" }
      enableRbacAuthorization   = true
      enableSoftDelete          = true
      softDeleteRetentionInDays = 7
    }
  }
}

resource "azapi_resource" "kv_role" {
  for_each = merge(
    { controller = { principal = azapi_resource.controller_identity.output.properties.principalId, role = local.kv_secrets_user_role_id, type = "ServicePrincipal" } },
    { for k, v in var.data_plane_operators : "officer-${k}" => { principal = v.principal_id, role = local.kv_secrets_officer_role_id, type = v.principal_type } },
  )

  type      = "Microsoft.Authorization/roleAssignments@2022-04-01"
  name      = uuidv5("url", "${azapi_resource.vault.id}/${each.key}")
  parent_id = azapi_resource.vault.id
  body = {
    properties = {
      roleDefinitionId = each.value.role
      principalId      = each.value.principal
      principalType    = each.value.type
    }
  }
}

output "key_vault_name" {
  value = azapi_resource.vault.name
}
