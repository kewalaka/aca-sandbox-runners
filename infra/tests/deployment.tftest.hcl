mock_provider "azapi" {
  mock_data "azapi_client_config" {
    defaults = {
      subscription_id = "00000000-0000-0000-0000-000000000001"
      tenant_id       = "00000000-0000-0000-0000-000000000002"
    }
  }
}

override_resource {
  target = azapi_resource.rg
  values = { id = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/rg-example" }
}

override_resource {
  target = azapi_resource.controller_identity
  values = {
    id = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/rg-example/providers/Microsoft.ManagedIdentity/userAssignedIdentities/controller"
    output = {
      properties = {
        principalId = "00000000-0000-0000-0000-000000000003"
        clientId    = "00000000-0000-0000-0000-000000000004"
      }
    }
  }
}

override_resource {
  target = azapi_resource.vault
  values = {
    id     = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/rg-example/providers/Microsoft.KeyVault/vaults/vault"
    output = { properties = { vaultUri = "https://vault.example.test/" } }
  }
}

override_resource {
  target = azapi_resource.registry
  values = {
    id     = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/rg-example/providers/Microsoft.ContainerRegistry/registries/registry"
    output = { properties = { loginServer = "registry.example.test" } }
  }
}

override_module {
  target = module.sandbox_runners
  outputs = {
    name                = "sbxg-example"
    resource_id         = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/rg-example/providers/Microsoft.App/sandboxGroups/sbxg-example"
    management_endpoint = "https://sandbox.example.test"
  }
}

variables {
  location          = "example"
  name_prefix       = "example"
  github_config_url = "https://github.com/example/workloads"
}

run "bootstrap" {
  command = apply

  assert {
    condition     = length(azapi_resource.controller) == 0 && length(azapi_resource.controller_env) == 0
    error_message = "Bootstrap must not start the controller before credentials and images exist."
  }

  assert {
    condition     = length(azapi_resource.acr_role) == 1
    error_message = "An omitted publishing identity must not add a hardcoded CI role."
  }
}

run "existing_environment" {
  command = apply

  variables {
    deploy_controller          = true
    ci_principal_id            = "00000000-0000-0000-0000-000000000005"
    controller_environment_id  = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network/providers/Microsoft.App/managedEnvironments/existing"
    github_app_client_id       = "example-client"
    github_app_installation_id = 1
  }

  assert {
    condition     = length(azapi_resource.controller_env) == 0 && azapi_resource.controller[0].body.properties.environmentId == var.controller_environment_id
    error_message = "An existing environment must be reused, not recreated."
  }

  assert {
    condition     = local.controller_env.GITHUB_CONFIG_URL == var.github_config_url && local.controller_env.SANDBOX_ENDPOINT == "https://sandbox.example.test"
    error_message = "The controller must use the configured GitHub URL and module endpoint."
  }

  assert {
    condition     = azapi_resource.controller[0].body.properties.template.scale == { minReplicas = 1, maxReplicas = 1 }
    error_message = "The scale-set controller must remain a single replica."
  }

  assert {
    condition     = azapi_resource.acr_role["ci_push"].body.properties.principalId == var.ci_principal_id && local.controller_secret.env == "GITHUB_APP_PRIVATE_KEY"
    error_message = "CI permissions and App credentials must use explicit deployment inputs."
  }
}

run "controller_subnet" {
  command = apply

  variables {
    deploy_controller    = true
    controller_subnet_id = "/subscriptions/00000000-0000-0000-0000-000000000001/resourceGroups/network/providers/Microsoft.Network/virtualNetworks/existing/subnets/controller"
  }

  assert {
    condition     = azapi_resource.controller_env[0].body.properties.vnetConfiguration.infrastructureSubnetId == var.controller_subnet_id
    error_message = "A new controller environment must use the supplied subnet."
  }

  assert {
    condition     = local.controller_secret.env == "GITHUB_TOKEN"
    error_message = "PAT authentication must remain available when no GitHub App is configured."
  }
}

run "reject_ambiguous_network" {
  command = plan

  variables {
    controller_environment_id = "existing"
    controller_subnet_id      = "subnet"
  }

  expect_failures = [var.controller_subnet_id]
}

run "reject_missing_app_installation" {
  command = plan

  variables {
    deploy_controller    = true
    github_app_client_id = "example-client"
  }

  expect_failures = [azapi_resource.controller]
}
