terraform {
  required_version = ">= 1.9, < 2.0"

  backend "azurerm" {}

  required_providers {
    azapi = {
      source  = "Azure/azapi"
      version = "~> 2.13"
    }
  }
}

provider "azapi" {}

variable "location" {
  description = "Azure region with ACA Sandboxes support."
  type        = string
  nullable    = false
}

variable "name_prefix" {
  description = "Deployment name used for the resource group, registry, vault and sandbox group."
  type        = string
  nullable    = false

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{2,11}$", var.name_prefix))
    error_message = "name_prefix must be 3-12 lowercase letters or digits, starting with a letter."
  }
}

variable "ci_principal_id" {
  description = "Object ID (not client ID) of the image publishing identity; granted AcrPush and sandbox data-plane access."
  type        = string
  default     = null
}

variable "data_plane_operators" {
  description = "Additional operators granted sandbox data-plane access and Key Vault Secrets Officer."
  type = map(object({
    principal_id   = string
    principal_type = string
  }))
  default = {}
}

variable "sandbox_vnet_connection" {
  description = "Optional existing subnet delegated to Microsoft.App/environments for sandbox private network access."
  type        = object({ subnet_id = string })
  default     = null
}

variable "tags" {
  type    = map(string)
  default = {}
}

data "azapi_client_config" "current" {}

resource "azapi_resource" "rg" {
  type      = "Microsoft.Resources/resourceGroups@2024-11-01"
  name      = "rg-${var.name_prefix}"
  location  = var.location
  parent_id = "/subscriptions/${data.azapi_client_config.current.subscription_id}"
  tags      = var.tags
  body      = {}
}

module "sandbox_runners" {
  source = "git::https://github.com/kewalaka/terraform-azurerm-ptn-cicd-sandbox-runners.git?ref=75d50d09c6d227edccd31e505c48e0d1394f5bec"

  location         = var.location
  name             = "sbxg-${var.name_prefix}"
  parent_id        = azapi_resource.rg.id
  enable_telemetry = false
  vnet_connection  = var.sandbox_vnet_connection
  tags             = var.tags

  data_plane_operators = merge(var.data_plane_operators, var.ci_principal_id == null ? {} : {
    ci = {
      principal_id   = var.ci_principal_id
      principal_type = "ServicePrincipal"
    }
    }, {
    controller = {
      principal_id   = azapi_resource.controller_identity.output.properties.principalId
      principal_type = "ServicePrincipal"
    }
  })
}

output "resource_group_name" {
  value = azapi_resource.rg.name
}

output "sandbox_group_name" {
  value = module.sandbox_runners.name
}

output "sandbox_group_id" {
  value = module.sandbox_runners.resource_id
}

output "management_endpoint" {
  value = module.sandbox_runners.management_endpoint
}
