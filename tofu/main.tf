terraform {
  required_version = ">= 1.6.0"

  required_providers {
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/azuread | jq -r .version
    azuread = {
      source  = "hashicorp/azuread"
      version = "~> 3.8"
    }
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/azurerm | jq -r .version
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.77"
    }
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/external | jq -r .version
    external = {
      source  = "hashicorp/external"
      version = "~> 2.4"
    }
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/tls | jq -r .version
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.3"
    }
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/time | jq -r .version
    time = {
      source  = "hashicorp/time"
      version = "~> 0.14"
    }
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/http | jq -r .version
    http = {
      source  = "hashicorp/http"
      version = "~> 3.5"
    }
  }

  backend "azurerm" {
    resource_group_name  = "azure-k8s-role-assigner-ci"
    storage_account_name = "k8sroleasstfstate"
    container_name       = "tfstate"
    key                  = "tofu.tfstate"
    subscription_id      = "5d5d98c4-3aad-4fd4-99d9-9403fd7e1635"
    use_oidc             = false
  }
}

provider "azuread" {}

provider "azurerm" {
  features {}
  subscription_id = var.subscription_id
}

data "azuread_client_config" "current" {}
data "azurerm_client_config" "current" {}

data "azuread_domains" "current" {
  only_initial = true
}

data "http" "github_meta" {
  url = "https://api.github.com/meta"

  request_headers = {
    Accept = "application/json"
  }
}

# --- Variables ---

variable "github_repository" {
  description = "GitHub repository in org/repo format for federated credential subject"
  type        = string
  default     = "dronenb/azure-k8s-role-assigner"
}

variable "subscription_id" {
  description = "Azure subscription ID (set to the new subscription after first apply)"
  type        = string
}

variable "billing_scope_id" {
  description = "Billing scope for subscription creation (find in Azure Portal: Cost Management + Billing > Properties)"
  type        = string
}

variable "location" {
  description = "Azure region for resources"
  type        = string
  default     = "eastus"
}

variable "resource_prefix" {
  description = "Prefix for resource names"
  type        = string
  default     = "azure-k8s-role-assigner"
}

# --- Subscription ---

resource "azurerm_subscription" "ci" {
  subscription_name = "${var.resource_prefix}-ci"
  billing_scope_id  = var.billing_scope_id
}

# --- Resource Group ---

resource "azurerm_resource_group" "ci" {
  name     = "${var.resource_prefix}-ci"
  location = var.location
}

# --- RSA Signing Key (stable, for minikube SA issuer) ---

resource "tls_private_key" "sa_signing" {
  algorithm = "RSA"
  rsa_bits  = 2048
}

resource "random_password" "e2e_test_user" {
  length           = 32
  special          = true
  min_lower        = 1
  min_upper        = 1
  min_numeric      = 1
  min_special      = 1
  override_special = "!@#$%^&*-_=+"
}

# --- Key Vault ---

resource "azurerm_key_vault" "ci" {
  name                       = "kv-k8sroleassigner"
  location                   = azurerm_resource_group.ci.location
  resource_group_name        = azurerm_resource_group.ci.name
  tenant_id                  = data.azuread_client_config.current.tenant_id
  sku_name                   = "standard"
  purge_protection_enabled   = false
  soft_delete_retention_days = 7
  rbac_authorization_enabled = true
}

# Current user gets Key Vault Administrator (to manage secrets during apply)
resource "azurerm_role_assignment" "kv_admin_current_user" {
  scope                = azurerm_key_vault.ci.id
  role_definition_name = "Key Vault Administrator"
  principal_id         = data.azurerm_client_config.current.object_id
}

# Store signing key in Key Vault
resource "time_sleep" "wait_for_kv_rbac" {
  depends_on      = [azurerm_role_assignment.kv_admin_current_user]
  create_duration = "60s"
}

resource "azurerm_key_vault_secret" "sa_signing_key" {
  name         = "sa-signing-key"
  value        = tls_private_key.sa_signing.private_key_pem
  key_vault_id = azurerm_key_vault.ci.id

  depends_on = [time_sleep.wait_for_kv_rbac]
}

resource "azurerm_key_vault_secret" "e2e_test_user_password" {
  name         = "e2e-test-user-password"
  value        = random_password.e2e_test_user.result
  key_vault_id = azurerm_key_vault.ci.id

  depends_on = [time_sleep.wait_for_kv_rbac]
}

# --- OIDC Storage Account (minikube SA issuer) ---

resource "azurerm_storage_account" "oidc" {
  name                     = "k8soidcassigner"
  resource_group_name      = azurerm_resource_group.ci.name
  location                 = azurerm_resource_group.ci.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  min_tls_version          = "TLS1_2"
}

resource "azurerm_storage_account_static_website" "oidc" {
  storage_account_id = azurerm_storage_account.oidc.id
  index_document     = "index.html"
}

# Upload OIDC discovery + JWKS (derived from the stable signing key)
locals {
  oidc_issuer_url = trimsuffix(azurerm_storage_account.oidc.primary_web_endpoint, "/")
  github_actions_ip_ranges = sort(distinct(concat(
    try(jsondecode(data.http.github_meta.response_body).actions, []),
    try(jsondecode(data.http.github_meta.response_body).actions_macos, [])
  )))
  github_actions_ip_range_chunks = {
    for idx, ranges in chunklist(local.github_actions_ip_ranges, 40) :
    tostring(idx) => ranges
  }

  oidc_discovery = jsonencode({
    issuer                                = local.oidc_issuer_url
    jwks_uri                              = "${local.oidc_issuer_url}/openid/v1/jwks"
    response_types_supported              = ["id_token"]
    subject_types_supported               = ["public"]
    id_token_signing_alg_values_supported = ["RS256"]
  })
}

resource "azurerm_storage_blob" "oidc_discovery" {
  name                 = ".well-known/openid-configuration"
  storage_container_id = "${azurerm_storage_account.oidc.id}/blobServices/default/containers/$web"
  type                 = "Block"
  content_type         = "application/json"
  source_content       = local.oidc_discovery

  depends_on = [azurerm_storage_account_static_website.oidc]
}

# Generate JWKS from the RSA public key via external data source
data "external" "jwks" {
  program = ["python3", "${path.module}/scripts/pem_to_jwks.py"]

  query = {
    public_key_pem = tls_private_key.sa_signing.public_key_pem
  }
}

resource "azurerm_storage_blob" "oidc_jwks" {
  name                 = "openid/v1/jwks"
  storage_container_id = "${azurerm_storage_account.oidc.id}/blobServices/default/containers/$web"
  type                 = "Block"
  content_type         = "application/json"
  source_content       = data.external.jwks.result.jwks

  depends_on = [azurerm_storage_account_static_website.oidc]
}

# --- OpenTofu State Backend ---

resource "azurerm_storage_account" "tfstate" {
  name                     = "k8sroleasstfstate"
  resource_group_name      = azurerm_resource_group.ci.name
  location                 = azurerm_resource_group.ci.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  min_tls_version          = "TLS1_2"
}

resource "azurerm_storage_container" "tfstate" {
  name               = "tfstate"
  storage_account_id = azurerm_storage_account.tfstate.id
}

# --- GitHub Actions Service Principal (WIF) ---

resource "azuread_application" "github_actions" {
  display_name     = "${var.resource_prefix}-github-actions"
  sign_in_audience = "AzureADMyOrg"
  owners           = [data.azuread_client_config.current.object_id]

  required_resource_access {
    resource_app_id = data.azuread_service_principal.microsoft_graph.client_id

    resource_access {
      id   = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "User.Read.All"])
      type = "Role"
    }

    resource_access {
      id   = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "DelegatedPermissionGrant.ReadWrite.All"])
      type = "Role"
    }
  }
}

resource "azuread_service_principal" "github_actions" {
  client_id = azuread_application.github_actions.client_id
  owners    = [data.azuread_client_config.current.object_id]
}

# Federated credential for pull requests
resource "azuread_application_federated_identity_credential" "github_pr" {
  application_id = azuread_application.github_actions.id
  display_name   = "github-pr"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = "https://token.actions.githubusercontent.com"
  subject        = "repo:${var.github_repository}:pull_request"
}

# Federated credential for the main branch (releases)
resource "azuread_application_federated_identity_credential" "github_main" {
  application_id = azuread_application.github_actions.id
  display_name   = "github-main"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = "https://token.actions.githubusercontent.com"
  subject        = "repo:${var.github_repository}:ref:refs/heads/main"
}

# GHA SP: Key Vault Secrets User (to download signing key in CI)
resource "azurerm_role_assignment" "gha_kv_secrets_user" {
  scope                = azurerm_key_vault.ci.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azuread_service_principal.github_actions.object_id
}

# GHA SP: Graph Application.ReadWrite.OwnedBy (create/delete owned apps per-run)
data "azuread_service_principal" "microsoft_graph" {
  client_id = "00000003-0000-0000-c000-000000000000"
}

resource "azuread_app_role_assignment" "gha_app_rw_ownedby" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Application.ReadWrite.OwnedBy"])
  principal_object_id = azuread_service_principal.github_actions.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

# GHA SP: Graph User.Read.All (lookup e2e test user by UPN in Terraform)
resource "azuread_app_role_assignment" "gha_user_read_all" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "User.Read.All"])
  principal_object_id = azuread_service_principal.github_actions.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

# GHA SP: Graph DelegatedPermissionGrant.ReadWrite.All (manage oauth2 permission grants)
resource "azuread_app_role_assignment" "gha_delegated_permission_grant_rw" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "DelegatedPermissionGrant.ReadWrite.All"])
  principal_object_id = azuread_service_principal.github_actions.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

# --- Test Groups (static, persist across runs) ---

resource "azuread_user" "e2e_test_user" {
  user_principal_name   = "${var.resource_prefix}-e2e-test-user@${data.azuread_domains.current.domains[0].domain_name}"
  display_name          = "${var.resource_prefix}-e2e-test-user"
  mail_nickname         = "${replace(var.resource_prefix, "-", "")}-e2e"
  password              = random_password.e2e_test_user.result
  account_enabled       = true
  force_password_change = false
}

# Trusted IP locations for GitHub Actions hosted runners.
# GitHub publishes a large CIDR list, so split into multiple named locations to avoid policy detail size limits.
resource "azuread_named_location" "github_actions_ips" {
  for_each = local.github_actions_ip_range_chunks

  display_name = "${var.resource_prefix}-github-actions-ips-${each.key}"

  ip {
    ip_ranges = each.value
    trusted   = true
  }
}

# Block e2e test user sign-ins from outside GitHub Actions hosted runner IPs.
# resource "azuread_conditional_access_policy" "e2e_test_user_gha_only" {
#   display_name = "${var.resource_prefix}-e2e-test-user-gha-only"
#   state        = "enabled"

#   conditions {
#     client_app_types = ["all"]

#     applications {
#       included_applications = ["All"]
#     }

#     users {
#       included_users = [azuread_user.e2e_test_user.object_id]
#     }

#     locations {
#       included_locations = ["All"]
#       excluded_locations = [for _, location in azuread_named_location.github_actions_ips : location.id]
#     }
#   }

#   grant_controls {
#     operator          = "OR"
#     built_in_controls = ["block"]
#   }
# }

# Group bound to a ClusterRoleBinding in e2e tests
resource "azuread_group" "test_crb" {
  display_name     = "${var.resource_prefix}-e2e-crb"
  security_enabled = true
  owners           = [data.azuread_client_config.current.object_id]
  members          = [azuread_user.e2e_test_user.object_id]
}

# Group bound to a RoleBinding in e2e tests
resource "azuread_group" "test_rb" {
  display_name     = "${var.resource_prefix}-e2e-rb"
  security_enabled = true
  owners           = [data.azuread_client_config.current.object_id]
  members          = [azuread_user.e2e_test_user.object_id]
}

# 201 filler groups to push test SP over the 200-group token overage limit
resource "azuread_group" "filler" {
  count            = 201
  display_name     = "${var.resource_prefix}-e2e-filler-${format("%03d", count.index)}"
  security_enabled = true
  owners           = [data.azuread_client_config.current.object_id]
  members          = [azuread_user.e2e_test_user.object_id]
}


# --- Outputs ---

output "github_actions_client_id" {
  description = "Client ID for GitHub Actions WIF login"
  value       = azuread_application.github_actions.client_id
}

output "github_actions_object_id" {
  description = "Object ID of the GHA service principal (for bootstrap script)"
  value       = azuread_service_principal.github_actions.object_id
}

output "tenant_id" {
  description = "Azure AD tenant ID"
  value       = data.azuread_client_config.current.tenant_id
}

output "subscription_id" {
  description = "Azure subscription ID (from the managed subscription)"
  value       = azurerm_subscription.ci.subscription_id
}

output "oidc_issuer_url" {
  description = "OIDC issuer URL (storage account static website)"
  value       = local.oidc_issuer_url
}

output "key_vault_name" {
  description = "Key Vault name containing the SA signing key"
  value       = azurerm_key_vault.ci.name
}

output "signing_key_secret_name" {
  description = "Key Vault secret name for the SA signing key"
  value       = azurerm_key_vault_secret.sa_signing_key.name
}

output "e2e_test_user_password_secret_name" {
  description = "Key Vault secret name for the static e2e test user password"
  value       = azurerm_key_vault_secret.e2e_test_user_password.name
}

output "storage_account_name" {
  description = "Storage account name for OIDC JWKS upload"
  value       = azurerm_storage_account.oidc.name
}

output "test_group_crb_id" {
  description = "Object ID of ClusterRoleBinding test group"
  value       = azuread_group.test_crb.object_id
}

output "test_group_rb_id" {
  description = "Object ID of RoleBinding test group"
  value       = azuread_group.test_rb.object_id
}

output "e2e_test_user_object_id" {
  description = "Object ID of the static e2e test user"
  value       = azuread_user.e2e_test_user.object_id
}

output "e2e_test_user_upn" {
  description = "UPN of the static e2e test user used for ROPC"
  value       = azuread_user.e2e_test_user.user_principal_name
}

output "github_actions_named_location_id" {
  description = "IDs of named locations containing GitHub Actions runner IP ranges"
  value       = [for _, location in azuread_named_location.github_actions_ips : location.id]
}

# output "e2e_test_user_conditional_access_policy_id" {
#   description = "ID of the conditional access policy restricting the e2e test user to GitHub Actions IP ranges"
#   value       = azuread_conditional_access_policy.e2e_test_user_gha_only.id
# }

output "tfstate_storage_account_name" {
  description = "Storage account name for remote state backend"
  value       = azurerm_storage_account.tfstate.name
}

output "tfstate_container_name" {
  description = "Container name for remote state backend"
  value       = azurerm_storage_container.tfstate.name
}

output "tfstate_resource_group_name" {
  description = "Resource group for remote state backend"
  value       = azurerm_resource_group.ci.name
}
