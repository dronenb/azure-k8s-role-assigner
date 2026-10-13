# Static CI Infrastructure (`tofu/`)

This directory contains the **one-time provisioned** infrastructure for CI/CD and end-to-end testing. It must be applied by a tenant admin before the GitHub Actions workflows or local e2e tests will function.

## What it provisions

- Azure subscription (`*-ci`): Dedicated subscription for all CI resources.
- Resource group (`*-ci`): Container for Azure resources.
- RSA 2048-bit signing key (`tls_private_key`): Stable service account token signing key for kind OIDC tests.
- Key Vault (`kv-k8sroleassigner`): Stores the private signing key as a secret.
- Storage account + static website: Hosts OIDC discovery (`.well-known/openid-configuration`) and JWKS (`openid/v1/jwks`).
- GitHub Actions app registration + service principal: WIF identity for CI (federated for PR + main branch).
- Federated identity credentials: OIDC trust for `repo:<org>/<repo>:pull_request` and `:ref:refs/heads/main`.
- Key Vault role assignment: GHA SP gets `Key Vault Secrets User` to download the signing key.
- Graph API role assignments: GHA SP gets `Application.ReadWrite.OwnedBy`, `User.Read.All`, and `DelegatedPermissionGrant.ReadWrite.All` for per-run e2e provisioning.
- Static e2e test user: User principal used by e2e verification via ROPC.
- Named locations for GitHub Actions runner IP ranges. The Conditional Access policy that would restrict the e2e test user to those ranges is present in code but currently commented out.
- Four Entra security groups used by RoleBinding, ClusterRoleBinding, Argo CD ConfigMap, and Argo CD AppProject tests, plus filler groups that push the static test user over the 200-group token overage threshold.
- State backend storage account + container: Remote OpenTofu state storage (for migration from local state).
- GitHub repo variables: Automatically set via `gh variable set` on apply.

## Prerequisites

- Azure CLI authenticated (`az login`)
- OpenTofu >= 1.6.0
- `gh` CLI authenticated (for setting repository variables)
- Python 3 (for the JWKS generation script)
- Permissions:
  - Microsoft Entra ID: create app registrations, grant admin consent, create groups
  - Azure: create resource groups, storage accounts, key vaults in the target subscription

## Usage

```bash
task tofu:init
task tofu:apply -- \
  -var="billing_scope_id=<your-billing-scope-id>" \
  -var="subscription_id=$(az account show --query id -o tsv)"
```

The `task tofu:plan`, `task tofu:apply`, and `task tofu:destroy` wrappers use `-parallelism=25` by default because the static stack creates hundreds of independent Entra groups. Override with `TOFU_BASE_PARALLELISM=10` if Microsoft Graph throttles, or a higher value if your tenant reliably tolerates it.

On first apply, a dedicated subscription is created and all resources are provisioned within it. GitHub repository variables are set by the bootstrap script after apply.

### Variables

- `billing_scope_id` (required, no default): Billing scope for subscription creation (see [Finding billing_scope_id](#finding-billing_scope_id)).
- `subscription_id` (required, no default): Any existing subscription for initial provider auth (replaced by the new sub after first apply).
- `github_repository` (optional, default `dronenb/azure-k8s-role-assigner`): GitHub repo for federated credentials and `gh variable set`.
- `location` (optional, default `eastus`): Azure region.
- `resource_prefix` (optional, default `azure-k8s-role-assigner`): Prefix for all resource display names.

### Finding `billing_scope_id`

In the Azure Portal:

1. Go to **Cost Management + Billing**
2. Select your billing account
3. Go to **Properties** (or **Billing scopes**)
4. Copy the **Billing scope ID** / **Resource ID**

It looks like:

```text
/providers/Microsoft.Billing/billingAccounts/<id>/billingProfiles/<id>/invoiceSections/<id>
```

### Bootstrap workflow

The first apply uses any existing subscription for provider auth, creates the dedicated subscription, then subsequent applies use the new one:

```bash
# One-liner: auto-discover billing scope, create subscription, provision all resources
BILLING_ACCOUNT=$(az billing account list --query '[0].name' -o tsv) && \
BILLING_PROFILE=$(az billing profile list --account-name "$BILLING_ACCOUNT" --query '[0].name' -o tsv) && \
BILLING_SCOPE=$(az billing invoice section list --account-name "$BILLING_ACCOUNT" --profile-name "$BILLING_PROFILE" --query '[0].id' -o tsv) && \
cat > terraform.tfvars <<EOF
billing_scope_id = "$BILLING_SCOPE"
EOF
task tofu:init && \
  TENANT_ID=$(az account show --query tenantId -o tsv) && \
  tofu apply -parallelism=25 -target=azurerm_subscription.ci \
    -var="subscription_id=$(az account show --query id -o tsv)" && \
  az logout && az login --tenant "$TENANT_ID" && \
  task tofu:apply -- -var="subscription_id=$(tofu output -raw subscription_id)" && \
  echo "subscription_id = \"$(tofu output -raw subscription_id)\"" >> terraform.tfvars
```

### Outputs

- `github_actions_client_id`: Client ID for WIF login.
- `github_actions_object_id`: Object ID of GHA SP (used by bootstrap script).
- `tenant_id`: Microsoft Entra tenant ID.
- `subscription_id`: Azure subscription ID.
- `oidc_issuer_url`: Storage account static website URL (OIDC issuer).
- `key_vault_name`: Key Vault name.
- `signing_key_secret_name`: Secret name containing the RSA private key.
- `e2e_test_user_password_secret_name`: Secret name containing the e2e test user password.
- `storage_account_name`: Storage account name.
- `test_group_crb_id`: Object ID of the ClusterRoleBinding test group.
- `test_group_rb_id`: Object ID of the RoleBinding test group.
- `test_group_argocd_configmap_id`: Object ID of the Argo CD ConfigMap test group.
- `test_group_argocd_appproject_id`: Object ID of the Argo CD AppProject test group.
- `e2e_test_user_object_id`: Object ID of the static e2e test user.
- `e2e_test_user_upn`: UPN of the static e2e test user.
- `github_actions_named_location_id`: Named location IDs containing GitHub Actions runner IP CIDRs.
- `tfstate_storage_account_name`: Storage account for remote state backend.
- `tfstate_container_name`: Container name for remote state backend.
- `tfstate_resource_group_name`: Resource group for remote state backend.

## GitHub Actions Named Locations

The static stack manages named locations for GitHub Actions hosted runner IP ranges:

- A named location is built from `https://api.github.com/meta` runner IP ranges.
- Large CIDR lists are split across multiple named locations to stay within policy size limits.
- Existing named-location IP ranges are ignored by default during updates to avoid large recurring churn and Graph throttling as GitHub changes its published runner IPs.
- The Conditional Access policy that would target the static e2e test user and block non-GitHub source IPs is currently commented out in `main.tf`.

If you enable that policy, it keeps ROPC usable for CI while preventing general use from non-GitHub source IPs.

## Bootstrap: Limited Consent Policy

After `tofu apply`, run the PowerShell bootstrap script to create a limited consent policy. This allows the GHA SP to grant **only** `Group.Read.All`, `Application.ReadWrite.OwnedBy`, and delegated `email`/`openid`/`profile` scopes needed by per-run e2e apps — without requiring full tenant admin:

```powershell
./bootstrap.ps1 -GhaSpObjectId "$(tofu output -raw github_actions_object_id)"
```

This creates:

1. A permission grant policy scoped to only those Graph app roles and delegated scopes
2. A custom directory role that allows granting consent under that policy
3. An assignment of that role to the GHA SP

This is idempotent — safe to re-run.

## Architecture: Why a stable signing key?

Kubernetes service account tokens must be verifiable by Microsoft Entra ID for Workload Identity Federation. Azure caches JWKS documents and can take up to 24 hours to pick up a new key. By using a **stable RSA key** stored in Key Vault:

- The OIDC discovery + JWKS are written once to the storage account static website and remain valid across all CI runs.
- There is no race condition between starting kind and Azure accepting the projected SA token.
- CI and maintainers can download the signing key before `task e2e:cluster-up`; the Taskfile mounts it into the kind control plane when present.

## File layout

```text
tofu/
├── main.tf            # All resources (providers, key vault, storage, app reg, groups, outputs)
├── bootstrap.ps1      # One-time limited consent policy (PowerShell + Microsoft Graph SDK)
└── scripts/
    └── pem_to_jwks.py # External data source: converts RSA PEM → JWKS JSON
```
