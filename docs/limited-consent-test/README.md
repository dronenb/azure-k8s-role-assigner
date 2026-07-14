# limited-consent-test

This OpenTofu stack is the Azure/Entra infrastructure side of the end-to-end setup documented in [TESTING.md](../../TESTING.md).

The stack provisions nearly all prerequisites used in that guide:

1. Controller app registration + service principal
2. Cluster OIDC app registration + service principal
3. Graph permission requests and admin-consent role assignments for controller identity
4. Ownership wiring for `Application.ReadWrite.OwnedBy`
5. Optional test groups used by RoleBinding/ClusterRoleBinding checks
6. Optional storage account static website endpoint for service-account issuer metadata
7. Optional federated identity credential on the controller app

## Files

- [main.tf](main.tf): Full E2E infrastructure definition.
- [terraform.tfvars.example](terraform.tfvars.example): Variable template.

## Run

1. Copy vars file:

```bash
cp terraform.tfvars.example terraform.tfvars
```

1. Set auth environment variables for the Terraform/OpenTofu provisioner identity:

```bash
export ARM_TENANT_ID="<tenant-id>"
export ARM_CLIENT_ID="<terraform-provisioner-sp-app-id>"
export ARM_CLIENT_SECRET="<terraform-provisioner-sp-client-secret>"
export ARM_SUBSCRIPTION_ID="<subscription-id>"
```

1. Execute OpenTofu:

```bash
tofu init
tofu plan
tofu apply
```

1. Use outputs in `TESTING.md` flow:

- `controller_app_id`
- `controller_sp_object_id`
- `controller_client_secret`
- `cluster_oidc_app_id`
- `cluster_oidc_sp_object_id`
- `cluster_oidc_app_role_id`
- `test_group_admin_id`
- `test_group_developer_id`
- `oidc_issuer_url`
