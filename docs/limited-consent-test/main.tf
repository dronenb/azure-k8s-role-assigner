terraform {
  required_version = ">= 1.6.0"

  required_providers {
    azuread = {
      source  = "hashicorp/azuread"
      version = "~> 2.53"
    }
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 3.116"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

provider "azuread" {}

provider "azurerm" {
  features {}
}

data "azuread_client_config" "current" {}

variable "name_prefix" {
  description = "Prefix used for Entra app registrations and groups"
  type        = string
  default     = "azure-k8s-role-assigner"
}

variable "cluster_name" {
  description = "Cluster name used to name OIDC and controller identities"
  type        = string
}

variable "location" {
  description = "Azure region for optional storage account infrastructure"
  type        = string
  default     = "eastus"
}

variable "create_oidc_storage" {
  description = "Create storage account static website for service account issuer metadata"
  type        = bool
  default     = false
}

variable "oidc_issuer_url" {
  description = "Explicit OIDC issuer URL to use for federated credential (if not using created storage endpoint)"
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

variable "create_test_groups" {
  description = "Create two test groups used by TESTING.md RBAC scenarios"
  type        = bool
  default     = true
}

data "azuread_service_principal" "microsoft_graph" {
  client_id = "00000003-0000-0000-c000-000000000000"
}

resource "random_string" "suffix" {
  length  = 6
  special = false
  upper   = false
}

locals {
  suffix                    = random_string.suffix.result
  base_name                 = "${var.name_prefix}-${var.cluster_name}-${local.suffix}"
  test_group_admin_name     = "k8s-test-admins-${local.suffix}"
  test_group_developer_name = "k8s-test-developers-${local.suffix}"
  effective_oidc_issuer_url = var.oidc_issuer_url != "" ? trimsuffix(var.oidc_issuer_url, "/") : (
    var.create_oidc_storage ? trimsuffix(azurerm_storage_account.oidc[0].primary_web_endpoint, "/") : null
  )
}

resource "azuread_application" "controller" {
  display_name     = "azure-k8s-role-assigner-controller-${local.base_name}"
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

resource "azuread_application_password" "controller" {
  application_id = azuread_application.controller.id
  display_name   = "e2e-client-secret"
}

resource "azuread_application" "cluster_oidc" {
  display_name                   = "kubernetes-cluster-oidc-${local.base_name}"
  sign_in_audience               = "AzureADMyOrg"
  fallback_public_client_enabled = true
  group_membership_claims        = ["ApplicationGroup"]
  owners                         = [data.azuread_client_config.current.object_id]

  web {
    redirect_uris = ["http://localhost:8000/"]

    implicit_grant {
      id_token_issuance_enabled = true
    }
  }

  app_role {
    allowed_member_types = ["User", "Application"]
    description          = "Access to Kubernetes cluster"
    display_name         = "Cluster Access"
    enabled              = true
    id                   = "a9a6f6cc-3ce2-4f48-b4fc-77e4af8abcb8"
    value                = "Cluster.Access"
  }

  api {
    requested_access_token_version = 2

    oauth2_permission_scope {
      admin_consent_description  = "Access Kubernetes"
      admin_consent_display_name = "Access Kubernetes"
      enabled                    = true
      id                         = "2fef7f4f-43da-4bc5-aa2a-7344ebca55ce"
      type                       = "User"
      user_consent_description   = "Access Kubernetes"
      user_consent_display_name  = "Access Kubernetes"
      value                      = "access"
    }
  }

  required_resource_access {
    resource_app_id = data.azuread_service_principal.microsoft_graph.client_id

    resource_access {
      id   = one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "profile"])
      type = "Scope"
    }

    resource_access {
      id   = one([for s in data.azuread_service_principal.microsoft_graph.oauth2_permission_scopes : s.id if s.value == "User.Read"])
      type = "Scope"
    }
  }
}

resource "azuread_service_principal" "cluster_oidc" {
  client_id = azuread_application.cluster_oidc.client_id
  owners = [
    data.azuread_client_config.current.object_id,
    azuread_service_principal.controller.object_id,
  ]
}

resource "azuread_application_owner" "cluster_app_controller_owner" {
  application_id  = azuread_application.cluster_oidc.id
  owner_object_id = azuread_service_principal.controller.object_id
}

resource "azuread_app_role_assignment" "controller_group_read_all" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Group.Read.All"])
  principal_object_id = azuread_service_principal.controller.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

resource "azuread_app_role_assignment" "controller_app_rw_ownedby" {
  app_role_id         = one([for r in data.azuread_service_principal.microsoft_graph.app_roles : r.id if r.value == "Application.ReadWrite.OwnedBy"])
  principal_object_id = azuread_service_principal.controller.object_id
  resource_object_id  = data.azuread_service_principal.microsoft_graph.object_id
}

resource "azuread_group" "test_admins" {
  count            = var.create_test_groups ? 1 : 0
  display_name     = local.test_group_admin_name
  security_enabled = true
  owners           = [data.azuread_client_config.current.object_id]
}

resource "azuread_group" "test_developers" {
  count            = var.create_test_groups ? 1 : 0
  display_name     = local.test_group_developer_name
  security_enabled = true
  owners           = [data.azuread_client_config.current.object_id]
}

resource "azurerm_resource_group" "oidc" {
  count    = var.create_oidc_storage ? 1 : 0
  name     = "k8s-oidc-federation-${local.suffix}"
  location = var.location
}

resource "azurerm_storage_account" "oidc" {
  count                    = var.create_oidc_storage ? 1 : 0
  name                     = "k8soidc${local.suffix}"
  resource_group_name      = azurerm_resource_group.oidc[0].name
  location                 = azurerm_resource_group.oidc[0].location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  min_tls_version          = "TLS1_2"

  static_website {}
}

resource "azuread_application_federated_identity_credential" "controller" {
  count          = local.effective_oidc_issuer_url != null ? 1 : 0
  application_id = azuread_application.controller.id
  display_name   = "kubernetes-federated-credential"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = local.effective_oidc_issuer_url
  subject        = "system:serviceaccount:${var.service_account_namespace}:${var.service_account_name}"
}

output "controller_app_id" {
  description = "Client ID of controller app registration"
  value       = azuread_application.controller.client_id
}

output "controller_app_object_id" {
  description = "Object ID of controller app registration"
  value       = azuread_application.controller.object_id
}

output "controller_sp_object_id" {
  description = "Object ID of controller service principal"
  value       = azuread_service_principal.controller.object_id
}

output "controller_client_secret" {
  description = "Client secret for controller app registration"
  value       = azuread_application_password.controller.value
  sensitive   = true
}

output "cluster_oidc_app_id" {
  description = "Client ID of cluster OIDC app registration"
  value       = azuread_application.cluster_oidc.client_id
}

output "cluster_oidc_sp_object_id" {
  description = "Object ID of cluster OIDC service principal"
  value       = azuread_service_principal.cluster_oidc.object_id
}

output "cluster_oidc_app_object_id" {
  description = "Object ID of cluster OIDC app registration"
  value       = azuread_application.cluster_oidc.object_id
}

output "cluster_oidc_app_role_id" {
  description = "ID of Cluster.Access app role"
  value       = one([for r in azuread_application.cluster_oidc.app_role : r.id if r.value == "Cluster.Access"])
}

output "test_group_admin_id" {
  description = "Object ID of k8s admin test group"
  value       = var.create_test_groups ? azuread_group.test_admins[0].object_id : null
}

output "test_group_developer_id" {
  description = "Object ID of k8s developer test group"
  value       = var.create_test_groups ? azuread_group.test_developers[0].object_id : null
}

output "oidc_issuer_url" {
  description = "OIDC issuer URL for service account federation"
  value       = local.effective_oidc_issuer_url
}
