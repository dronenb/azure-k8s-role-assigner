#!/usr/bin/env pwsh
<#
.SYNOPSIS
    One-time bootstrap script for the limited consent policy and custom role.
    Run this AFTER 'tofu apply' in this directory (needs the GHA SP object ID from outputs).

.DESCRIPTION
    Creates:
	1. A permission grant policy that constrains consent to Group.Read.All + Application.ReadWrite.OwnedBy and delegated email/openid/profile scopes
    2. A custom directory role that allows the GHA SP to grant consent under that policy
    3. Assigns the custom role to the GHA SP

	This enables the GHA SP to grant only the specific Graph permissions/scopes needed
	(Group.Read.All + Application.ReadWrite.OwnedBy + delegated email/openid/profile) when provisioning per-run e2e resources,
    without being a tenant admin.

.PARAMETER GhaSpObjectId
    The object ID of the GitHub Actions service principal. Get from: tofu output -raw github_actions_object_id

.EXAMPLE
    ./bootstrap.ps1 -GhaSpObjectId "$(tofu output -raw github_actions_object_id)"
#>

param(
	[Parameter(Mandatory = $true)]
	[string]$GhaSpObjectId,

	[Parameter(Mandatory = $false)]
	[string]$GitHubRepository = "dronenb/azure-k8s-role-assigner"
)

$ErrorActionPreference = "Stop"

$policyId = "azure-k8s-role-assigner-e2e-consent"
$roleName = "E2E Consent Operator (azure-k8s-role-assigner)"

# Connect to Microsoft Graph
Write-Host "Connecting to Microsoft Graph..."
Connect-MgGraph -Scopes "Policy.ReadWrite.PermissionGrant", "RoleManagement.ReadWrite.Directory", "Application.Read.All" -ErrorAction Stop

# Resolve Microsoft Graph service principal and app role IDs
Write-Host "Resolving Graph app role IDs..."
$graphSp = Get-MgServicePrincipal -Filter "displayName eq 'Microsoft Graph'" -ErrorAction Stop | Select-Object -First 1

$groupReadAll = $graphSp.AppRoles | Where-Object { $_.Value -eq "Group.Read.All" } | Select-Object -First 1
$appRwOwnedBy = $graphSp.AppRoles | Where-Object { $_.Value -eq "Application.ReadWrite.OwnedBy" } | Select-Object -First 1
$emailScope = $graphSp.Oauth2PermissionScopes | Where-Object { $_.Value -eq "email" } | Select-Object -First 1
$openidScope = $graphSp.Oauth2PermissionScopes | Where-Object { $_.Value -eq "openid" } | Select-Object -First 1
$profileScope = $graphSp.Oauth2PermissionScopes | Where-Object { $_.Value -eq "profile" } | Select-Object -First 1

if (-not $groupReadAll -or -not $appRwOwnedBy -or -not $emailScope -or -not $openidScope -or -not $profileScope) {
	throw "Could not resolve required Graph app role and delegated scope IDs."
}

Write-Host "  Group.Read.All ID: $($groupReadAll.Id)"
Write-Host "  Application.ReadWrite.OwnedBy ID: $($appRwOwnedBy.Id)"
Write-Host "  email scope ID: $($emailScope.Id)"
Write-Host "  openid scope ID: $($openidScope.Id)"
Write-Host "  profile scope ID: $($profileScope.Id)"

# Create permission grant policy (idempotent)
Write-Host "Creating permission grant policy '$policyId'..."
$existingPolicy = Get-MgPolicyPermissionGrantPolicy -PermissionGrantPolicyId $policyId -ErrorAction SilentlyContinue
if ($existingPolicy) {
	Write-Host "  Policy already exists, skipping creation."
} else {
	New-MgPolicyPermissionGrantPolicy `
		-Id $policyId `
		-DisplayName $policyId `
		-Description "Allows GHA SP to grant only Group.Read.All and Application.ReadWrite.OwnedBy for azure-k8s-role-assigner e2e tests."

	Write-Host "  Policy created."
}


# Ensure include for application permissions exists (idempotent)
$existingAppInclude = @(Get-MgPolicyPermissionGrantPolicyInclude -PermissionGrantPolicyId $policyId -ErrorAction SilentlyContinue) | Where-Object {
	$_.PermissionType -eq "application" -and
	$_.ResourceApplication -eq $graphSp.AppId -and
	($_.Permissions -contains $groupReadAll.Id) -and
	($_.Permissions -contains $appRwOwnedBy.Id)
} | Select-Object -First 1

if ($existingAppInclude) {
	Write-Host "  Application include already exists, skipping."
} else {
	New-MgPolicyPermissionGrantPolicyInclude `
		-PermissionGrantPolicyId $policyId `
		-PermissionType "application" `
		-ResourceApplication $graphSp.AppId `
		-Permissions @($groupReadAll.Id, $appRwOwnedBy.Id) `
		-ClientApplicationIds @("all")
	Write-Host "  Application include created."
}

# Ensure include for delegated email/openid/profile scopes exists (idempotent)
$existingDelegatedInclude = @(Get-MgPolicyPermissionGrantPolicyInclude -PermissionGrantPolicyId $policyId -ErrorAction SilentlyContinue) | Where-Object {
	$_.PermissionType -eq "delegated" -and
	$_.ResourceApplication -eq $graphSp.AppId -and
	($_.Permissions -contains $emailScope.Id) -and
	($_.Permissions -contains $openidScope.Id) -and
	($_.Permissions -contains $profileScope.Id)
} | Select-Object -First 1

if ($existingDelegatedInclude) {
	Write-Host "  Delegated include already exists, skipping."
} else {
	New-MgPolicyPermissionGrantPolicyInclude `
		-PermissionGrantPolicyId $policyId `
		-PermissionType "delegated" `
		-ResourceApplication $graphSp.AppId `
		-Permissions @($emailScope.Id, $openidScope.Id, $profileScope.Id) `
		-ClientApplicationIds @("all")
	Write-Host "  Delegated include created for scopes: email, openid, profile"
}

# Create custom directory role (idempotent)
Write-Host "Creating custom directory role '$roleName'..."
$existingRole = Get-MgRoleManagementDirectoryRoleDefinition -Filter "displayName eq '$roleName'" -ErrorAction SilentlyContinue | Select-Object -First 1
$requiredAllowedResourceActions = @(
	"microsoft.directory/servicePrincipals/managePermissionGrantsForAll.$policyId",
	"microsoft.directory/servicePrincipals/managePermissionGrantsForSelf.$policyId"
)

if ($existingRole) {
	$existingAllowedResourceActions = @(
		$existingRole.RolePermissions |
		ForEach-Object { $_.AllowedResourceActions } |
		Where-Object { $_ }
	)
	$mergedAllowedResourceActions = @($existingAllowedResourceActions + $requiredAllowedResourceActions | Select-Object -Unique)

	$missingAllowedResourceActions = @(
		$requiredAllowedResourceActions |
		Where-Object { $_ -notin $existingAllowedResourceActions }
	)

	if ($missingAllowedResourceActions.Count -gt 0) {
		Write-Host "  Role exists (ID: $($existingRole.Id)) but is missing required actions. Updating role definition..."
		Update-MgRoleManagementDirectoryRoleDefinition -UnifiedRoleDefinitionId $existingRole.Id -BodyParameter @{
			description     = $existingRole.Description
			displayName     = $existingRole.DisplayName
			isEnabled       = $existingRole.IsEnabled
			rolePermissions = @(
				@{
					allowedResourceActions = $mergedAllowedResourceActions
				}
			)
		} | Out-Null
		Write-Host "  Role updated with required allowedResourceActions."
		$role = Get-MgRoleManagementDirectoryRoleDefinition -UnifiedRoleDefinitionId $existingRole.Id -ErrorAction Stop
	} else {
		Write-Host "  Role already exists (ID: $($existingRole.Id)) with required actions, skipping update."
		$role = $existingRole
	}

} else {
	$role = New-MgRoleManagementDirectoryRoleDefinition -BodyParameter @{
		displayName     = $roleName
		description     = "Allows GHA SP to grant policy-constrained admin consent for azure-k8s-role-assigner e2e."
		isEnabled       = $true
		rolePermissions = @(
			@{
				allowedResourceActions = $requiredAllowedResourceActions
			}
		)
	}
	Write-Host "  Role created (ID: $($role.Id))."
}

# Assign custom role to GHA SP (idempotent)
Write-Host "Assigning role to GHA SP (Object ID: $GhaSpObjectId)..."
$existingAssignment = Get-MgRoleManagementDirectoryRoleAssignment -Filter "principalId eq '$GhaSpObjectId' and roleDefinitionId eq '$($role.Id)'" -ErrorAction SilentlyContinue | Select-Object -First 1
if ($existingAssignment) {
	Write-Host "  Role assignment already exists, skipping."
} else {
	New-MgRoleManagementDirectoryRoleAssignment -BodyParameter @{
		roleDefinitionId = $role.Id
		principalId      = $GhaSpObjectId
		directoryScopeId = "/"
	}
	Write-Host "  Role assigned."
}

Write-Host ""
Write-Host "Bootstrap complete. The GHA SP can now grant Group.Read.All + Application.ReadWrite.OwnedBy and delegated email/openid/profile via the limited consent policy."
Write-Host ""

# --- Set GitHub Repository Variables from tofu outputs ---
Write-Host "Setting GitHub repository variables for '$GitHubRepository'..."

$vars = @{
	"AZURE_CLIENT_ID"                    = (tofu output -raw github_actions_client_id)
	"AZURE_TENANT_ID"                    = (tofu output -raw tenant_id)
	"AZURE_SUBSCRIPTION_ID"              = (tofu output -raw subscription_id)
	"OIDC_ISSUER_URL"                    = (tofu output -raw oidc_issuer_url)
	"KEY_VAULT_NAME"                     = (tofu output -raw key_vault_name)
	"SIGNING_KEY_SECRET_NAME"            = (tofu output -raw signing_key_secret_name)
	"E2E_TEST_USER_PASSWORD_SECRET_NAME" = (tofu output -raw e2e_test_user_password_secret_name)
	"TEST_GROUP_CRB_ID"                  = (tofu output -raw test_group_crb_id)
	"TEST_GROUP_RB_ID"                   = (tofu output -raw test_group_rb_id)
	"TEST_GROUP_ARGOCD_CONFIGMAP_ID"     = (tofu output -raw test_group_argocd_configmap_id)
	"TEST_GROUP_ARGOCD_APPPROJECT_ID"    = (tofu output -raw test_group_argocd_appproject_id)
	"E2E_TEST_USER_UPN"                  = (tofu output -raw e2e_test_user_upn)
}

foreach ($kv in $vars.GetEnumerator()) {
	Write-Host "  $($kv.Key) = $($kv.Value)"
	gh variable set $kv.Key --repo $GitHubRepository --body $kv.Value
}

Write-Host ""
Write-Host "Done. GitHub repository variables are configured."
