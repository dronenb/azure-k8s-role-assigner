# Contributing

## Prerequisites

- [Go](https://go.dev/dl/) (version in `go.mod`)
- [Task](https://taskfile.dev/) (v3+)
- [ko](https://ko.build/) (v0.18.1)
- [Helm](https://helm.sh/) (v4.2.3)
- [git-cliff](https://git-cliff.org/) (v2.13.1)
- [OpenTofu](https://opentofu.org/) (>= 1.6.0)
- [kind](https://kind.sigs.k8s.io/) (v0.32.0+)
- [Podman](https://podman.io/) (for kind driver)
- [Azure CLI](https://learn.microsoft.com/en-us/cli/azure/) (for e2e tests)

## Development

```bash
task build      # Build the manager binary
task test       # Run unit tests (fmt + vet first)
task run        # Run the controller locally
task build-image  # Build an OCI image tarball via ko
```

Run `task --list` to see all available tasks.

## Commit Convention

This project uses [Conventional Commits](https://www.conventionalcommits.org/). All commit messages must follow the format:

```text
<type>[optional scope]: <description>

[optional body]

[optional footer(s)]
```

**Types that affect versioning:**

| Type                               | Version bump |
| ---------------------------------- | ------------ |
| `feat`                             | Minor        |
| `fix`                              | Patch        |
| `perf`                             | Patch        |
| `feat!` / `BREAKING CHANGE` footer | Major        |

Types like `chore`, `ci`, `docs`, `style`, `test`, and `build` do not trigger a version bump.

## End-to-End Tests

E2E tests run against a real `kind` cluster with real Azure Entra ID resources, using Workload Identity Federation (projected SA tokens, no client secrets).

### Quick start for contributors

You must create your own test resources in your Microsoft Entra tenant (one-time setup):

**1. Create two test groups and add the test user to them:**

```bash
az ad group create --display-name "my-test-crb" --mail-nickname "my-test-crb"
TEST_GROUP_CRB_ID="<copy group object ID>"

az ad group create --display-name "my-test-rb" --mail-nickname "my-test-rb"
TEST_GROUP_RB_ID="<copy group object ID>"

TEST_USER_UPN="my-e2e-test-user@<your-tenant-domain>"
TEST_USER_PASSWORD="<strong-password>"

az ad user create \
  --display-name "my-e2e-test-user" \
  --user-principal-name "$TEST_USER_UPN" \
  --password "$TEST_USER_PASSWORD" \
  --force-change-password-next-sign-in false

TEST_USER_OBJECT_ID="$(az ad user show --id "$TEST_USER_UPN" --query id -o tsv)"

az ad group member add --group "$TEST_GROUP_CRB_ID" --member-id "$TEST_USER_OBJECT_ID"
az ad group member add --group "$TEST_GROUP_RB_ID" --member-id "$TEST_USER_OBJECT_ID"
```

**2. Prepare your OIDC issuer and signing key (optional):**

You need an OIDC issuer serving JWKS. You can either:

- Use the project's issuer (maintainers only): `$(cd tofu && tofu output -raw oidc_issuer_url)`
- Create your own (see [TESTING.md](TESTING.md#creating-your-own-oidc-issuer))

Optionally, provide a stable signing key for the Kubernetes cluster. If omitted, kind will generate its own:

```bash
# Optional: use a stable signing key
mkdir -p .e2e
cp /path/to/your/sa-signing.key .e2e/sa-signing.key
```

**3. Run the full test suite:**

Token verification uses ROPC against the static test user:

```bash
export OIDC_ISSUER_URL="https://your-oidc-issuer-url"
export E2E_TEST_USER_UPN="$TEST_USER_UPN"
export E2E_TEST_USER_PASSWORD="$TEST_USER_PASSWORD"
export TEST_GROUP_CRB_ID="$TEST_GROUP_CRB_ID"
export TEST_GROUP_RB_ID="$TEST_GROUP_RB_ID"

task e2e
```

Full details: see [TESTING.md](TESTING.md).

### Quick start for maintainers (with Key Vault access)

If you have access to the project's Key Vault:

```bash
az login

export OIDC_ISSUER_URL="$(cd tofu && tofu output -raw oidc_issuer_url)"
export KEY_VAULT_NAME="$(cd tofu && tofu output -raw key_vault_name)"
export TEST_GROUP_CRB_ID="$(cd tofu && tofu output -raw test_group_crb_id)"
export TEST_GROUP_RB_ID="$(cd tofu && tofu output -raw test_group_rb_id)"
export E2E_TEST_USER_UPN="$(cd tofu && tofu output -raw e2e_test_user_upn)"
export E2E_TEST_USER_PASSWORD_SECRET_NAME="$(cd tofu && tofu output -raw e2e_test_user_password_secret_name)"
export AZURE_APP_ROLE_ID="$(cd e2e/tofu && tofu output -raw cluster_app_role_id)"

task e2e:ci  # Downloads key from Key Vault, then runs full suite
```

`AZURE_APP_ROLE_ID` is shown for ad hoc/local deployment flows. `task e2e:ci` derives the dynamic value from `e2e/tofu` during deployment.

### Setting up static infrastructure (optional, project admins only)

The project maintains optional static infrastructure in `tofu/` for transparency and CI consistency. If you're a project maintainer:

```bash
cd tofu/
tofu init
tofu apply \
  -var="billing_scope_id=<your-billing-scope-id>" \
  -var="subscription_id=<your-subscription-id>"
./bootstrap.ps1 -GhaSpObjectId "$(tofu output -raw github_actions_object_id)"
```

See [`tofu/README.md`](tofu/README.md) for details.

## Release Process

Releases are automated via a PR-based flow using git-cliff for versioning/changelog and GoReleaser for building/publishing.

### Steps to cut a release

1. **Create a release PR**: Open a PR to `main` (can be empty or contain last-minute fixes) and add the **`release-candidate`** label.

2. **Preview**: The `release-preview` workflow runs automatically and posts a PR comment with:
   - The next semantic version (computed from conventional commits)
   - Generated release notes
   - A GoReleaser dry-run to verify the build succeeds

3. **Merge**: Once the preview looks good and CI passes, merge the PR.

4. **Automatic release**: On merge, the `release` workflow:
   - Computes the next version via `git-cliff --bumped-version`
   - Creates and pushes a git tag (e.g., `v1.2.0`)
   - Generates release notes with git-cliff
   - Builds the Go binary and container image (via ko)
   - Pushes the image to `ghcr.io/dronenb/azure-k8s-role-assigner`
   - Signs the checksum file and container manifest with cosign (keyless OIDC)
   - Creates a GitHub Release with the signed artifacts and release notes

### What gets published

| Artifact                    | Location                                            |
| --------------------------- | --------------------------------------------------- |
| Go binary (`manager`)       | GitHub Release assets                               |
| Container image             | `ghcr.io/dronenb/azure-k8s-role-assigner:<version>` |
| Cosign signature (checksum) | `*.sigstore.json` in release assets                 |
| Cosign signature (image)    | Attached to OCI manifest in GHCR                    |

### Verifying signatures

```bash
# Verify the container image
cosign verify ghcr.io/dronenb/azure-k8s-role-assigner:<version> \
  --certificate-identity-regexp="https://github.com/dronenb/azure-k8s-role-assigner" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com"

# Verify a release asset
cosign verify-blob \
  --bundle manager-linux-amd64.sigstore.json \
  manager-linux-amd64
```

## Security

- All GitHub Actions are pinned by full SHA with version comments
- The container base image is SHA-pinned in `.ko.yaml`
- Fork PRs cannot access repository secrets or OIDC identity
- CI authenticates via Workload Identity Federation (no static secrets)
- Controller in e2e uses projected SA token (no client secrets)
- Cosign keyless signing uses GitHub OIDC (no static keys)
- Workflow permissions are scoped to the minimum required
