# Private registry for the controller and runner images. No admin user: everything pulls with Entra identities.
locals {
  acr_pull_role_id = "/subscriptions/${data.azapi_client_config.current.subscription_id}/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"
  acr_push_role_id = "/subscriptions/${data.azapi_client_config.current.subscription_id}/providers/Microsoft.Authorization/roleDefinitions/8311e382-0749-4cb8-b61a-304f252e45ec"
}

resource "azapi_resource" "registry" {
  type                   = "Microsoft.ContainerRegistry/registries@2025-11-01"
  name                   = "acr${var.name_prefix}${substr(data.azapi_client_config.current.subscription_id, 0, 8)}"
  location               = var.location
  parent_id              = azapi_resource.rg.id
  response_export_values = ["properties.loginServer"]
  tags                   = var.tags
  body = {
    sku = { name = "Basic" }
    properties = {
      adminUserEnabled     = false
      anonymousPullEnabled = false
    }
  }
}

resource "azapi_resource" "acr_role" {
  for_each = merge({
    controller_pull = { principal = azapi_resource.controller_identity.output.properties.principalId, role = local.acr_pull_role_id, type = "ServicePrincipal" }
    }, var.ci_principal_id == null ? {} : {
    ci_push = { principal = var.ci_principal_id, role = local.acr_push_role_id, type = "ServicePrincipal" }
  })

  type      = "Microsoft.Authorization/roleAssignments@2022-04-01"
  name      = uuidv5("url", "${azapi_resource.registry.id}/${each.key}")
  parent_id = azapi_resource.registry.id
  body = {
    properties = {
      roleDefinitionId = each.value.role
      principalId      = each.value.principal
      principalType    = each.value.type
    }
  }
}

output "registry_login_server" {
  value = azapi_resource.registry.output.properties.loginServer
}
