# Limited-Privilege Terraform Consent for azure-k8s-role-assigner

This guide uses three separate identities and a constrained permission grant policy so Terraform can grant only the required Graph permissions without being tenant admin.

## Identity Model (Three Identities)

1. Terraform provisioner identity

- Service principal used by Terraform to provision Azure and Entra resources.
- Gets a custom role bound to a permission grant policy.
- Can grant admin consent only for the specific Microsoft Graph permissions allowed by that permission grant policy.

1. OIDC app registration (per cluster)

- The app registration users authenticate to for Kubernetes OIDC.
- Exposes the cluster app role that groups are assigned to.

1. azure-k8s controller identity (per cluster)

- App registration/service principal used by the `azure-k8s-role-assigner` controller (also per cluster).
- Must be owner on the OIDC app registration/service principal pair used for Kubernetes authentication.

This separation is least privilege and avoids self-ownership coupling.

## Required Graph App Permissions (on controller identity)

- `Group.Read.All`
- `Application.ReadWrite.OwnedBy`

## Why a Permission Grant Policy Is Needed

Terraform needs to grant admin consent programmatically, but you do not want Terraform to be tenant admin. The policy and custom role restrict Terraform to only:

- Microsoft Graph as resource app
- Application permission grants for the two permission IDs above
- Optional delegated permission grants for tightly-scoped e2e ROPC verification scopes

For the e2e ROPC flow, the same policy can include delegated Graph scopes such as `email`, `openid`, and `profile`. That keeps the delegated-consent path under the same policy boundary instead of creating a separate consent mechanism.

## Bootstrap (One-Time) with PowerShell

Use a privileged bootstrap identity once to configure policy + custom role + assignment to the Terraform provisioner SP.

If you are bootstrapping the delegated e2e flow as well, add the delegated Graph scopes to the same policy.

### 1) Connect and set variables

```pwsh
Connect-MgGraph -Scopes "Policy.ReadWrite.PermissionGrant,RoleManagement.ReadWrite.Directory,Application.Read.All"

$policyId = "tf-consent-azure-k8s-role-assigner-graph-limited"
$terraformProvisionerSpAppId = "<terraform-service-principal-id>"
```

### 2) Resolve Graph app role and delegated scope IDs

```pwsh
$graphSp = Get-MgServicePrincipal -Filter "displayName eq 'Microsoft Graph'" -ErrorAction Stop | Select-Object -First 1

$groupReadAll = $graphSp.AppRoles | Where-Object { $_.Value -eq "Group.Read.All" } | Select-Object -First 1
$appRwOwnedBy = $graphSp.AppRoles | Where-Object { $_.Value -eq "Application.ReadWrite.OwnedBy" } | Select-Object -First 1
$emailScope = $graphSp.Oauth2PermissionScopes | Where-Object { $_.Value -eq "email" } | Select-Object -First 1
$openidScope = $graphSp.Oauth2PermissionScopes | Where-Object { $_.Value -eq "openid" } | Select-Object -First 1
$profileScope = $graphSp.Oauth2PermissionScopes | Where-Object { $_.Value -eq "profile" } | Select-Object -First 1

if (-not $groupReadAll -or -not $appRwOwnedBy -or -not $emailScope -or -not $openidScope -or -not $profileScope) {
    throw "Could not resolve required Graph app role and delegated scope IDs."
}
```

### 3) Create permission grant policy

```pwsh
New-MgPolicyPermissionGrantPolicy `
    -Id $policyId `
    -DisplayName $policyId `
    -Description "Allows Terraform to grant only the Graph permissions required for azure-k8s-role-assigner."
```

### 4) Add include conditions

```pwsh
New-MgPolicyPermissionGrantPolicyInclude `
    -PermissionGrantPolicyId $policyId `
    -PermissionType "application" `
    -ResourceApplication $graphSp.AppId `
    -Permissions @($groupReadAll.Id, $appRwOwnedBy.Id) `
    -ClientApplicationIds @("all")

New-MgPolicyPermissionGrantPolicyInclude `
    -PermissionGrantPolicyId $policyId `
    -PermissionType "delegated" `
    -ResourceApplication $graphSp.AppId `
    -Permissions @($emailScope.Id, $openidScope.Id, $profileScope.Id) `
    -ClientApplicationIds @("all")
```

### 5) Create custom role bound to this policy

```pwsh
$role = New-MgRoleManagementDirectoryRoleDefinition -BodyParameter @{
    displayName = "Terraform Consent Operator (azure-k8s-role-assigner)"
    description = "Allows Terraform provisioner SP to grant policy-constrained admin consent only."
    isEnabled = $true
    rolePermissions = @(
        @{
            allowedResourceActions = @(
                "microsoft.directory/applications/permissions/update"
                "microsoft.directory/servicePrincipals/managePermissionGrantsForAll.$policyId"
            )
        }
    )
}
```

If your workflow needs it, also add:

- `microsoft.directory/applications/basic/update`

### 6) Assign custom role to Terraform provisioner SP

```pwsh
$terraformProvisionerSp = Get-MgServicePrincipal -Filter "appId eq '$terraformProvisionerSpAppId'" -ErrorAction Stop | Select-Object -First 1

if (-not $terraformProvisionerSp) {
    throw "Terraform provisioner service principal not found."
}

New-MgRoleManagementDirectoryRoleAssignment -BodyParameter @{
    roleDefinitionId = $role.Id
    principalId = $terraformProvisionerSp.Id
    directoryScopeId = "/"
}
```

## PowerShell: Initialize Terraform as the Terraform Provisioner Identity

Use environment variables so Terraform authenticates as the Terraform provisioner SP.

```pwsh
$env:ARM_TENANT_ID = "<tenant-id>"
$env:ARM_CLIENT_ID = "<terraform-provisioner-sp-app-id>"
$env:ARM_CLIENT_SECRET = "<terraform-provisioner-sp-client-secret>"
$env:ARM_SUBSCRIPTION_ID = "<subscription-id>"

cd ./docs/limited-consent-test
terraform init
terraform plan
terraform apply
```

If you use OpenTofu:

```pwsh
cd ./docs/limited-consent-test
tofu init
tofu plan
tofu apply
```

The reference infrastructure stack is [docs/limited-consent-test/main.tf](limited-consent-test/main.tf).

## Terraform Pattern

The Terraform provisioner identity creates and wires the per-cluster OIDC and per-cluster controller identities.

### 1) Controller identity (per cluster)

```hcl
resource "azuread_application" "controller" {
  display_name = "azure-k8s-role-assigner-controller-${var.cluster_name}"
}

resource "azuread_service_principal" "controller" {
  client_id = azuread_application.controller.client_id
}
```

### 2) Cluster OIDC identity (per cluster)

```hcl
resource "azuread_application" "cluster_oidc" {
  display_name = "azure-k8s-oidc-${var.cluster_name}"
}

resource "azuread_service_principal" "cluster_oidc" {
  client_id = azuread_application.cluster_oidc.client_id
}
```

### 3) Make controller identity owner of cluster OIDC service principal (enterprise app)

```hcl
resource "azuread_service_principal" "cluster_oidc" {
  client_id = azuread_application.cluster_oidc.client_id
  owners = [
    data.azuread_client_config.current.object_id,
    azuread_service_principal.controller.object_id,
  ]
}
```

Important: ownership on the OIDC service principal is the crucial requirement for `Application.ReadWrite.OwnedBy` flows in this model.

### 4) Grant required Graph app roles to the controller SP

```hcl
data "azuread_service_principal" "microsoft_graph" {
  client_id = "00000003-0000-0000-c000-000000000000"
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
```

## Operational Flow

1. Bootstrap policy and custom role once (PowerShell).
1. Assign custom role to Terraform provisioner SP.
1. Run Terraform as Terraform provisioner SP.
1. Terraform creates per-cluster controller identity.
1. Terraform creates per-cluster OIDC identity.
1. Terraform sets controller as owner of OIDC service principal (enterprise app).
1. Terraform grants only required Graph permissions to the controller SP.

This yields programmatic consent with strict boundaries and no tenant-admin Terraform identity.
