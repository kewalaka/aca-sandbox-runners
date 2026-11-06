variable "deploy_controller" {
  description = "Deploy the scale-set controller on Azure Container Apps. Needs the controller image in the registry and the GitHub credential in the vault."
  type        = bool
  default     = false
}

variable "github_config_url" {
  description = "GitHub repository URL whose jobs this scale set runs."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^https://[^/]+/[^/]+/[^/]+/?$", var.github_config_url))
    error_message = "github_config_url must be an HTTPS repository URL."
  }
}

variable "controller_environment_id" {
  description = "Optional existing ACA managed environment; otherwise create an environment for the controller."
  type        = string
  default     = null
}

variable "controller_subnet_id" {
  description = "Optional existing infrastructure subnet for the controller's new ACA environment."
  type        = string
  default     = null

  validation {
    condition     = var.controller_environment_id == null || var.controller_subnet_id == null
    error_message = "Set controller_environment_id or controller_subnet_id, not both."
  }
}

variable "github_app_client_id" {
  description = "Client ID of the GitHub App the controller authenticates as (private key: vault secret github-app-private-key). Empty: fall back to a PAT in vault secret github-token."
  type        = string
  default     = ""
}

variable "github_app_installation_id" {
  type    = number
  default = 0
}

variable "controller_image_tag" {
  type    = string
  default = "latest"
}

variable "controller_env" {
  description = "Extra controller environment variables (WARM_POOL, MAX_RUNNERS, KEEP_ON_FAILURE, ...)."
  type        = map(string)
  default     = {}
}

locals {
  github_app = var.github_app_client_id != ""

  controller_env = merge({
    GITHUB_CONFIG_URL       = var.github_config_url
    SANDBOX_ENDPOINT        = module.sandbox_runners.management_endpoint
    SANDBOX_SUBSCRIPTION_ID = data.azapi_client_config.current.subscription_id
    SANDBOX_RESOURCE_GROUP  = azapi_resource.rg.name
    SANDBOX_GROUP           = module.sandbox_runners.name
    SANDBOX_REGION          = var.location
    }, local.github_app ? {
    GITHUB_APP_CLIENT_ID       = var.github_app_client_id
    GITHUB_APP_INSTALLATION_ID = tostring(var.github_app_installation_id)
  } : {}, var.controller_env)

  # Vault secret -> container app secret -> env var holding it.
  controller_secret = local.github_app ? {
    name = "github-app-private-key", env = "GITHUB_APP_PRIVATE_KEY"
    } : {
    name = "github-token", env = "GITHUB_TOKEN"
  }
}

# The identity is created unconditionally so the module can grant it the data-plane role before the app exists.
resource "azapi_resource" "controller_identity" {
  type                      = "Microsoft.ManagedIdentity/userAssignedIdentities@2024-11-30"
  name                      = "id-${var.name_prefix}-controller"
  location                  = var.location
  parent_id                 = azapi_resource.rg.id
  response_export_values    = ["properties.principalId", "properties.clientId"]
  schema_validation_enabled = false
  tags                      = var.tags
  body                      = {}
}

resource "azapi_resource" "controller_env" {
  count     = var.deploy_controller && var.controller_environment_id == null ? 1 : 0
  type      = "Microsoft.App/managedEnvironments@2026-01-01"
  name      = "cae-${var.name_prefix}"
  location  = var.location
  parent_id = azapi_resource.rg.id
  tags      = var.tags
  body = {
    properties = merge({
      workloadProfiles = [{ name = "Consumption", workloadProfileType = "Consumption" }]
      }, var.controller_subnet_id == null ? {} : {
      vnetConfiguration = { infrastructureSubnetId = var.controller_subnet_id }
    })
  }
}

# One replica on purpose: a scale set allows a single message session, and there is no ingress.
resource "azapi_resource" "controller" {
  count     = var.deploy_controller ? 1 : 0
  type      = "Microsoft.App/containerApps@2026-01-01"
  name      = "ca-${var.name_prefix}-controller"
  location  = var.location
  parent_id = azapi_resource.rg.id
  tags      = var.tags

  identity {
    type         = "UserAssigned"
    identity_ids = [azapi_resource.controller_identity.id]
  }

  body = {
    properties = {
      environmentId       = var.controller_environment_id != null ? var.controller_environment_id : azapi_resource.controller_env[0].id
      workloadProfileName = "Consumption"
      configuration = {
        activeRevisionsMode = "Single"
        registries = [{
          server   = azapi_resource.registry.output.properties.loginServer
          identity = azapi_resource.controller_identity.id
        }]
        secrets = [{
          name        = local.controller_secret.name
          keyVaultUrl = "${azapi_resource.vault.output.properties.vaultUri}secrets/${local.controller_secret.name}"
          identity    = azapi_resource.controller_identity.id
        }]
      }
      template = {
        scale = { minReplicas = 1, maxReplicas = 1 }
        containers = [{
          name      = "controller"
          image     = "${azapi_resource.registry.output.properties.loginServer}/controller:${var.controller_image_tag}"
          resources = { cpu = 0.25, memory = "0.5Gi" }
          env = concat(
            [for k, v in local.controller_env : { name = k, value = v }],
            [
              { name = "AZURE_CLIENT_ID", value = azapi_resource.controller_identity.output.properties.clientId },
              { name = local.controller_secret.env, secretRef = local.controller_secret.name },
            ]
          )
        }]
      }
    }
  }

  # Role assignments must exist before the app first pulls the image / reads the secret.
  depends_on = [azapi_resource.acr_role, azapi_resource.kv_role]

  lifecycle {
    precondition {
      condition     = !local.github_app || var.github_app_installation_id != 0
      error_message = "github_app_installation_id is required with github_app_client_id."
    }
  }
}

output "controller_identity_principal_id" {
  value = azapi_resource.controller_identity.output.properties.principalId
}
