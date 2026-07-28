# Dynamic E2E Infrastructure (`e2e/tofu/`)

This directory contains **per-run** Entra ID resources provisioned by OpenTofu during each e2e test execution. These are created at the start of a test run and destroyed at the end, ensuring no leftover state between runs.

## What it provisions

| Resource                                                             | Purpose                                                                        |
| -------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| Controller app registration + SP                                     | The identity the controller authenticates as (WIF via projected SA token)      |
| Federated identity credential                                        | Links the controller service account to the controller app via OIDC issuer URL |
| Graph API grants (`Group.Read.All`, `Application.ReadWrite.OwnedBy`) | Controller permissions to read groups and manage app role assignments          |
| Cluster app registration + SP                                        | Represents the "cluster" resource — groups are assigned app roles on this SP   |
| `Cluster.Access` app role                                            | Custom app role on the cluster app (assigned to groups by the controller)      |
| Argo CD app registration + SP                                        | Represents the Argo CD resource — groups are assigned app roles on this SP     |
| Delegated Graph permission grant                                     | Allows the static e2e user to obtain ROPC tokens without interactive consent   |

Test groups are **not** created here — they come from the static infra (`tofu/`) and are passed through as variables.
The e2e verification user is also **not** created here — it comes from static infra and is used by the verify step via ROPC.

## How it fits together

```text
┌─────────────────────────────────────────────────────────────────────────┐
│                        Static (tofu/)                                    │
│  RSA Key → Key Vault    Storage: OIDC discovery + JWKS                  │
│  GHA SP (WIF)           Test Groups (admins, developers)                │
│  Static e2e user (ROPC)                                                 │
│  Limited Consent Policy                                                 │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │ Provides: OIDC issuer URL, signing key,
                               │           GHA identity, group IDs
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                     Dynamic per-run (e2e/tofu/)                          │
│  Controller app + federated cred (OIDC issuer → SA)                     │
│  Cluster app + Cluster.Access role                                      │
│  Argo CD app + ArgoCD.Access role                                       │
│  Graph API consent grants (via limited consent policy)                  │
└──────────────────────────────┬──────────────────────────────────────────┘
                               │ Provides: controller_client_id,
                               │           cluster_sp_object_id,
                               │           cluster_app_role_id, group IDs
                               ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                          kind Cluster                                    │
│  Started with a stable service-account signing key when available        │
│  SA issuer matches OIDC issuer URL from static infra                    │
│  Controller pod authenticates via projected SA token → WIF              │
│  RBAC bindings reference test group object IDs                          │
│  Controller reconciles bindings → assigns Cluster.Access role on SP     │
└─────────────────────────────────────────────────────────────────────────┘
```

## Usage

### Automated (via Taskfile)

```bash
# Full suite (infra-up through verification)
task e2e

# Cleanup after a local run
task e2e:cleanup
task e2e:infra-down

# Individual steps
task e2e:infra-up       # tofu apply in e2e/tofu/
task e2e:infra-down     # tofu destroy in e2e/tofu/
task e2e:infra-output   # Print outputs as env var assignments
```

### Manual

```bash
cd e2e/tofu/
tofu init
tofu apply \
  -var="oidc_issuer_url=https://k8soidcassigner.z13.web.core.windows.net" \
  -var="test_group_crb_id=<GROUP_ID>" \
  -var="test_group_rb_id=<GROUP_ID>" \
  -var="test_group_argocd_configmap_id=<GROUP_ID>" \
  -var="test_group_argocd_appproject_id=<GROUP_ID>" \
  -var="e2e_test_user_upn=<USER_UPN>"

# When done:
tofu destroy
```

### Variables

| Variable                             | Required | Default                   | Description                                                                  |
| ------------------------------------ | -------- | ------------------------- | ---------------------------------------------------------------------------- |
| `oidc_issuer_url`                    | Yes      | —                         | OIDC issuer URL from static infra (`tofu output oidc_issuer_url` in `tofu/`) |
| `test_group_crb_id`                  | Yes      | —                         | ClusterRoleBinding group object ID (from static infra)                       |
| `test_group_rb_id`                   | Yes      | —                         | RoleBinding group object ID (from static infra)                              |
| `test_group_argocd_configmap_id`     | Yes      | —                         | Argo CD ConfigMap group object ID (from static infra)                        |
| `test_group_argocd_appproject_id`    | Yes      | —                         | Argo CD AppProject group object ID (from static infra)                       |
| `e2e_test_user_upn`                  | Yes      | —                         | UPN of static e2e test user (used to resolve object ID for delegated grant)  |
| `name_suffix`                        | No       | random 8-char             | Unique suffix to avoid collisions (set to GHA run ID in CI)                  |
| `service_account_namespace`          | No       | `azure-k8s-role-assigner` | K8s namespace of controller SA                                               |
| `service_account_name`               | No       | `azure-k8s-role-assigner` | K8s service account name                                                     |

### Outputs

| Output                                | Description                                                      |
| ------------------------------------- | ---------------------------------------------------------------- |
| `tenant_id`                           | Microsoft Entra tenant ID                                        |
| `controller_client_id`                | Client ID the controller authenticates as                        |
| `cluster_app_client_id`               | Client ID of the cluster app used as the OIDC token audience     |
| `cluster_sp_object_id`                | SP where app role assignments are created                        |
| `cluster_app_role_id`                 | ID of the `Cluster.Access` app role                              |
| `argocd_app_client_id`                | Client ID of the Argo CD app used as the OIDC token audience     |
| `argocd_sp_object_id`                 | SP where Argo CD app role assignments are created                |
| `argocd_app_role_id`                  | ID of the `ArgoCD.Access` app role                               |
| `test_group_crb_id`                   | Object ID of ClusterRoleBinding group (pass-through)             |
| `test_group_rb_id`                    | Object ID of RoleBinding group (pass-through)                    |
| `test_group_argocd_configmap_id`      | Object ID of Argo CD ConfigMap group (pass-through)              |
| `test_group_argocd_appproject_id`     | Object ID of Argo CD AppProject group (pass-through)             |
| `e2e_test_user_object_id`             | Object ID of static e2e user (resolved from `e2e_test_user_upn`) |

## Lifecycle

1. **Created** at e2e test start (`task e2e:infra-up` / `tofu apply`)
2. **Used** by the controller during the test run
3. **Destroyed** at e2e test end (`task e2e:infra-down` / `tofu destroy`)

State is stored locally in `e2e/tofu/`. If a run is interrupted, re-run `tofu destroy` to clean up.

## Authentication context

- **In CI**: The GHA SP authenticates via WIF (`azure/login`) and uses the limited consent policy to grant Graph permissions to the per-run controller app.
- **Locally**: You authenticate via `az login`. You need permissions to create app registrations and grant admin consent (or be assigned the limited consent custom role).
