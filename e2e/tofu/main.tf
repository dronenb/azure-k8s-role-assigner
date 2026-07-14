terraform {
  required_version = ">= 1.6.0"

  required_providers {
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/azuread | jq -r .version
    azuread = {
      source  = "hashicorp/azuread"
      version = "~> 3.8"
    }
    # Check latest: curl -s https://registry.terraform.io/v1/providers/hashicorp/random | jq -r .version
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}

provider "azuread" {}

data "azuread_client_config" "current" {}

data "azuread_service_principal" "microsoft_graph" {
  client_id = "00000003-0000-0000-c000-000000000000"
}

# --- Variables ---

variable "name_suffix" {
  description = "Suffix for resource names (e.g., GHA run ID or local identifier)"
  type        = string
  default     = ""
}

variable "oidc_issuer_url" {
  description = "OIDC issuer URL for Kubernetes service account tokens"
  type        = string
}

variable "static_oidc_issuer_url" {
  description = "Static OIDC issuer URL from static infra; when equal to `oidc_issuer_url`, dynamic test-client credential will be skipped"
  type        = string
  default     = ""
}

variable "service_account_namespace" {
  description = "Namespace of controller service account for federated identity credential"
  type        = string
  default     = "azure-k8s-role-assigner"
}

variable "service_account_name" {
  description = "Service account name for federated identity credential"
  type        = string
  default     = "azure-k8s-role-assigner"
}

variable "test_group_crb_id" {
  description = "Object ID of the static ClusterRoleBinding test group (from tofu/ outputs)"
  type        = string
}

variable "test_group_rb_id" {
  description = "Object ID of the static RoleBinding test group (from tofu/ outputs)"
  type        = string
}

variable "e2e_test_user_upn" {
  description = "UPN of the static e2e test user (from tofu/ outputs)"
  type        = string
}

data "azuread_user" "e2e_test_user" {
  user_principal_name = var.e2e_test_user_upn
}

# --- Locals ---

resource "random_string" "suffix" {
  count   = var.name_suffix == "" ? 1 : 0
  length  = 8
  special = false
  upper   = false
}

locals {
  suffix    = var.name_suffix != "" ? var.name_suffix : random_string.suffix[0].result
  base_name = "azure-k8s-role-assigner-e2e-${local.suffix}"
}

# --- Controller App Registration ---

resource "azuread_application" "controller" {
  display_name     = "${local.base_name}-controller"
  sign_in_audience = "AzureADMyOrg"
  owners           = [data.azuread_client_config.current.object_id]

  required_resource_access {
    resource_app_id = data.azuread_service_principal.microsoft_graph.client_id

    resource_access {
      id   = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Group.Read.All"])
      type = "Role"
    }

    resource_access {
      id   = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Application.ReadWrite.OwnedBy"])
      type = "Role"
    }
  }
}

resource "azuread_service_principal" "controller" {
  client_id = azuread_application.controller.client_id
  owners    = [data.azuread_client_config.current.object_id]
}

# Consent: Group.Read.All
resource "azuread_app_role_assignment" "controller_group_read_all" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Group.Read.All"])
  principal_object_id = azuread_service_principal.controller.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

# Consent: Application.ReadWrite.OwnedBy
resource "azuread_app_role_assignment" "controller_app_rw_ownedby" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Application.ReadWrite.OwnedBy"])
  principal_object_id = azuread_service_principal.controller.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

# Federated identity credential (WIF for controller pod via SA token)
resource "azuread_application_federated_identity_credential" "controller_k8s" {
  application_id = azuread_application.controller.id
  display_name   = "kubernetes-e2e"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = var.oidc_issuer_url
  subject        = "system:serviceaccount:${var.service_account_namespace}:${var.service_account_name}"
}

# --- Cluster OIDC App Registration ---

resource "random_uuid" "cluster_access_role_id" {}
resource "random_uuid" "cluster_kubernetes_scope_id" {}

resource "azuread_application" "cluster" {
  display_name                   = "${local.base_name}-cluster"
  sign_in_audience               = "AzureADMyOrg"
  fallback_public_client_enabled = true
  owners = [
    data.azuread_client_config.current.object_id,
    azuread_service_principal.controller.object_id,
  ]

  required_resource_access {
    resource_app_id = data.azuread_service_principal.microsoft_graph.client_id

    resource_access {
      id   = one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "email"])
      type = "Scope"
    }

    resource_access {
      id   = one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "openid"])
      type = "Scope"
    }

    resource_access {
      id   = one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "profile"])
      type = "Scope"
    }
  }

  group_membership_claims = ["ApplicationGroup"]

  api {
    requested_access_token_version = 2

    oauth2_permission_scope {
      admin_consent_description  = "Access Kubernetes APIs"
      admin_consent_display_name = "Access Kubernetes"
      enabled                    = true
      id                         = random_uuid.cluster_kubernetes_scope_id.result
      type                       = "User"
      user_consent_description   = "Access Kubernetes APIs"
      user_consent_display_name  = "Access Kubernetes"
      value                      = "kubernetes"
    }
  }

  optional_claims {
    access_token {
      name = "email"
    }

    access_token {
      name = "upn"
    }

    access_token {
      name = "groups"
    }

    access_token {
      name = "preferred_username"
    }

    id_token {
      name = "email"
    }

    id_token {
      name = "upn"
    }

    id_token {
      name = "groups"
    }

    id_token {
      name = "preferred_username"
    }
  }

  app_role {
    allowed_member_types = ["User", "Application"]
    description          = "Access to Kubernetes cluster"
    display_name         = "Cluster Access"
    enabled              = true
    id                   = random_uuid.cluster_access_role_id.result
    value                = "Cluster.Access"
  }

  lifecycle {
    ignore_changes = [identifier_uris]
  }
}

resource "azuread_application_identifier_uri" "cluster_api" {
  application_id = azuread_application.cluster.id
  identifier_uri = "api://${azuread_application.cluster.client_id}"
}

resource "azuread_service_principal" "cluster" {
  client_id                    = azuread_application.cluster.client_id
  app_role_assignment_required = false
  owners = [
    data.azuread_client_config.current.object_id,
    azuread_service_principal.controller.object_id,
  ]
}

resource "azuread_service_principal_delegated_permission_grant" "cluster_e2e_test_user" {
  service_principal_object_id          = azuread_service_principal.cluster.object_id
  resource_service_principal_object_id = data.azuread_service_principal.microsoft_graph.object_id
  claim_values = [
    one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "email"]),
    one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "openid"]),
    one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "profile"])
  ]
  user_object_id = data.azuread_user.e2e_test_user.object_id
}

# --- Outputs ---

output "tenant_id" {
  description = "Azure AD tenant ID"
  value       = data.azuread_client_config.current.tenant_id
}

output "controller_client_id" {
  description = "Client ID of controller app registration"
  value       = azuread_application.controller.client_id
}

output "cluster_app_client_id" {
  description = "Client ID of cluster app (for OIDC authentication config)"
  value       = azuread_application.cluster.client_id
}

output "cluster_sp_object_id" {
  description = "Object ID of cluster service principal"
  value       = azuread_service_principal.cluster.object_id
}

output "cluster_app_role_id" {
  description = "ID of the Cluster.Access app role — set as AZURE_APP_ROLE_ID env var on the controller"
  value       = random_uuid.cluster_access_role_id.result
}

output "test_group_crb_id" {
  description = "Object ID of ClusterRoleBinding test group (passthrough)"
  value       = var.test_group_crb_id
}

output "test_group_rb_id" {
  description = "Object ID of RoleBinding test group (passthrough)"
  value       = var.test_group_rb_id
}

output "e2e_test_user_object_id" {
  description = "Object ID of static e2e test user (resolved from e2e_test_user_upn)"
  value       = data.azuread_user.e2e_test_user.object_id
}
