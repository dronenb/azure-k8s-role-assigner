# Testing Guide

This document explains how to run end-to-end (E2E) tests for the controller.

## Quick Start for Contributors

To test the controller, you need:

1. **Your own OIDC issuer** (serving OIDC discovery + JWKS for Kubernetes SA tokens)
2. **A signing key** (RSA private key matching your JWKS)
3. **A test client app** (Azure AD app registration used as the Kubernetes API audience)
4. **A static test user** (Azure AD user for ROPC token acquisition)
5. **Two test groups** (security groups with your test user as a member)

Everything is created in your own Azure AD tenant — no special permissions or static infrastructure required.

### Step 1: Create your test resources (one-time)

**Create the test client app:**

```bash
az login

az ad app create --display-name "my-e2e-test-client"
TEST_CLIENT_OBJECT_ID="<copy the object ID>"
TEST_CLIENT_ID="<copy the client ID>"
```

**Create a static test user:**

```bash
TEST_USER_UPN="my-e2e-test-user@<your-tenant-domain>"
TEST_USER_PASSWORD="<strong-password>"

az ad user create \
  --display-name "my-e2e-test-user" \
  --user-principal-name "$TEST_USER_UPN" \
  --password "$TEST_USER_PASSWORD" \
  --force-change-password-next-sign-in false
```

**Create two test groups and add the user to them:**

```bash
az ad group create --display-name "my-test-crb" --mail-nickname "my-test-crb"
TEST_GROUP_CRB_ID="<copy group object ID>"

az ad group create --display-name "my-test-rb" --mail-nickname "my-test-rb"
TEST_GROUP_RB_ID="<copy group object ID>"

TEST_USER_OBJECT_ID="$(az ad user show --id "$TEST_USER_UPN" --query id -o tsv)"

az ad group member add --group "$TEST_GROUP_CRB_ID" --member-id "$TEST_USER_OBJECT_ID"
az ad group member add --group "$TEST_GROUP_RB_ID" --member-id "$TEST_USER_OBJECT_ID"
```

The test client app is still required as the token audience (`api://<client-id>`), but token acquisition for verification uses the static user via ROPC.

### Why e2e uses a user (ROPC) instead of a service principal

The controller exists to force Entra group filtering via app role assignments so Kubernetes receives only relevant group claims. Service principals do not exercise this user group filtering path in the same way, so they are not valid for this e2e assertion. The verification step therefore uses a static user context and acquires tokens through the Resource Owner Password Credentials flow.

Because ROPC is sensitive, lock it down with Conditional Access so it is only allowed from GitHub Actions IP ranges.

### Step 2: Prepare your OIDC issuer and signing key

#### Option A: Use an existing Azure storage account

If you already have an Azure storage account with a static website serving OIDC discovery + JWKS:

```bash
export OIDC_ISSUER_URL="https://your-storage-account.blob.core.windows.net"
```

#### Option B: Create a new one

See the "Creating Your Own OIDC Issuer" section below for step-by-step instructions.

### Step 3: Add your signing key

Place the RSA private key that matches your JWKS:

```bash
mkdir -p .e2e
cp /path/to/your/sa-signing.key .e2e/sa-signing.key
chmod 600 .e2e/sa-signing.key
```

### Step 4: Run tests

```bash
export TEST_CLIENT_ID="..."
export E2E_TEST_USER_UPN="..."
export E2E_TEST_USER_PASSWORD="..."
export TEST_GROUP_CRB_ID="..."
export TEST_GROUP_RB_ID="..."
export OIDC_ISSUER_URL="https://your-issuer-url"

az login
task e2e
```

That's all you need. Everything else is automated.

## Creating Your Own OIDC Issuer

If you don't have an existing OIDC issuer, here's how to create one:

### 1. Generate a signing key

```bash
openssl genrsa -out sa-signing.key 2048
```

### 2. Create a storage account with static website

```bash
STORAGE_ACCOUNT="k8stest$(date +%s)"
az storage account create \
  --name "$STORAGE_ACCOUNT" \
  --resource-group <your-rg> \
  --location eastus \
  --sku Standard_LRS

az storage account update \
  --name "$STORAGE_ACCOUNT" \
  --set properties.minimumTlsVersion=TLS1_2

az storage blob service-properties update \
  --account-name "$STORAGE_ACCOUNT" \
  --static-website \
  --index-document index.html

export OIDC_ISSUER_URL="https://${STORAGE_ACCOUNT}.z13.web.core.windows.net"
echo "Your OIDC issuer: $OIDC_ISSUER_URL"
```

### 3. Generate JWKS from your signing key

Use the project's script:

```bash
python3 tofu/scripts/pem_to_jwks.py < sa-signing.key > jwks.json
```

Or manually with openssl:

```bash
openssl rsa -in sa-signing.key -pubout -outform PEM > sa-public.key
# Then use any RSA-to-JWKS converter (e.g., online tools or libraries)
```

### 4. Upload OIDC discovery and JWKS

Create `openid-configuration.json`:

```json
{
  "issuer": "<your-OIDC_ISSUER_URL>",
  "jwks_uri": "<your-OIDC_ISSUER_URL>/openid/v1/jwks",
  "response_types_supported": ["id_token"],
  "subject_types_supported": ["public"],
  "id_token_signing_alg_values_supported": ["RS256"]
}
```

Upload both files:

```bash
az storage blob upload \
  --account-name "$STORAGE_ACCOUNT" \
  --container-name '$web' \
  --name '.well-known/openid-configuration' \
  --file openid-configuration.json

az storage blob upload \
  --account-name "$STORAGE_ACCOUNT" \
  --container-name '$web' \
  --name 'openid/v1/jwks' \
  --file jwks.json
```

### 5. Test it

```bash
curl "${OIDC_ISSUER_URL}/.well-known/openid-configuration" | jq .
curl "${OIDC_ISSUER_URL}/openid/v1/jwks" | jq .
```

Now follow the quick start above with your new issuer URL.

## For Project Maintainers: Optional Static Infrastructure

The project maintains a **static infrastructure** in `tofu/` that provides a shared OIDC issuer, signing key, test client app, and static test user. This is entirely optional and exists for transparency and CI/CD consistency — not because contributors need it.

### Benefits of static infrastructure

- **Openness:** The project's OIDC issuer and signing key are auditable in version control
- **CI clarity:** GitHub Actions workflows and local tests use the exact same infrastructure
- **Convenience:** Maintainers don't have to manage multiple OIDC issuers

### Using static infrastructure (maintainers only)

If you have access to the project's Key Vault:

```bash
az login

# Fetch all prerequisites from static infra
export OIDC_ISSUER_URL="$(cd tofu && tofu output -raw oidc_issuer_url)"
export KEY_VAULT_NAME="$(cd tofu && tofu output -raw key_vault_name)"
export TEST_GROUP_CRB_ID="$(cd tofu && tofu output -raw test_group_crb_id)"
export TEST_GROUP_RB_ID="$(cd tofu && tofu output -raw test_group_rb_id)"
export E2E_TEST_USER_UPN="$(cd tofu && tofu output -raw e2e_test_user_upn)"
export E2E_TEST_USER_PASSWORD_SECRET_NAME="$(cd tofu && tofu output -raw e2e_test_user_password_secret_name)"

# Run full e2e (downloads key from Key Vault automatically)
task e2e:ci
```

### Setting up static infrastructure (admins only)

To set up the static infrastructure in your own environment:

```bash
cd tofu/
tofu init
tofu apply -var="subscription_id=<YOUR_SUBSCRIPTION_ID>"
./bootstrap.ps1 -GhaSpObjectId "$(tofu output -raw github_actions_object_id)"
```

See [`tofu/README.md`](tofu/README.md) for details.

## Local Development (Faster Iteration)

For quick iteration without running the full test suite:

```bash
# Ensure cluster and infra are provisioned
task e2e:cluster-up
task e2e:infra-up

# Get environment variables
eval "$(task e2e:infra-output)"

# Run controller locally with your Azure credentials
go run cmd/main.go
```

In another terminal, create/delete RBAC bindings and watch the controller reconcile in real time.

## CI/CD

The `.github/workflows/e2e.yml` runs on PRs touching Go, config, Terraform, or workflows:

1. Authenticates via Workload Identity Federation (no secrets)
2. Downloads signing key from Key Vault
3. Starts minikube with stable OIDC issuer
4. Provisions per-run Azure resources
5. Builds, loads, deploys
6. Verifies groups in token
7. Always cleans up with `task e2e:infra-down`

No secrets stored in GitHub.

## How It Works

### WIF Authentication Flow

```text
┌─────────────────────┐          ┌──────────────────┐
│  K8s Service Account │          │  Azure Entra ID  │
│  (minikube)         │──token──▶ │  (validates)     │
│                     │           │                  │
└─────────────────────┘           └────┬─────────────┘
                                       │
                         (fetches JWKS from)
                                       │
                                       ▼
                         ┌──────────────────────┐
                         │  OIDC Issuer         │
                         │  (storage account)   │
                         │  /openid/v1/jwks    │
                         └──────────────────────┘
```

1. Minikube signs SA tokens with your RSA signing key
2. Azure fetches the JWKS from your OIDC issuer to verify the signature
3. Federated identity credential trusts the issuer + subject
4. Azure issues an access token to the controller
5. Controller uses the token to call Microsoft Graph

**Key point:** The JWKS at your issuer must match the signing key minikube uses. If using the project's stable key, both are aligned automatically.

### Workload Identity Federation

- **No client secrets** — Tokens are bound to projected service account identities
- **No Key Vault overhead** — Minikube generates/signs tokens locally
- **Automatic cleanup** — `task e2e:infra-down` destroys all per-run resources

## Notes

- **Stable signing key**: Azure caches JWKS for up to 24h. Use a stable key to avoid race conditions at cluster startup.
- **Group Object IDs**: RBAC bindings expect Azure AD group Object IDs (UUIDs), not names.
- **Per-run cleanup**: `tofu destroy` in `e2e/tofu/` always removes Azure resources, regardless of test outcome.
