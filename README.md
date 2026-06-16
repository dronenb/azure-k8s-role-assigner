# azure-k8s-role-assigner

This Kubernetes controller watches for ClusterRoleBindings and RoleBindings, and assigns the groups in said object to the Azure AD Service Principal, so that the groups returned in the JWT token are filtered.

## Why This Exists

Microsoft Entra (formerly Azure AD) implements distributed claims, but not correctly according to the OIDC specification. When using structured authentication with Kubernetes (via OIDC), if a user is in more than 200 groups, Microsoft Entra will not return all groups in the JWT token. Instead, it returns an `_claim_names` and `_claim_sources` reference that points to a Microsoft Graph API endpoint.

Unfortunately, the Kubernetes API server does not support resolving these distributed claims, since Microsoft's implementation doesn't follow the spec correctly (see <https://github.com/kubernetes/kubernetes/issues/62920> and <https://github.com/argoproj/argo-cd/issues/7127>), which means users with more than 200 group memberships effectively lose all their group-based RBAC permissions in Kubernetes.

This controller works around this limitation by:

1. Monitoring all RoleBindings and ClusterRoleBindings in your cluster
2. Ensuring each referenced group exists in Azure AD by group Object ID
3. Explicitly assigning those groups to your Azure AD Service Principal(s)
4. This forces Azure AD to filter the groups in the token to only include relevant groups, keeping the count below 200

## How It Works

The controller operates as follows:

1. **Watches Kubernetes RBAC**: Monitors all `RoleBinding` and `ClusterRoleBinding` resources across all namespaces
2. **Extracts Group IDs**: Reads each `Group` subject `name` as an Azure group Object ID
3. **Validates in Azure**: Looks up each group in Azure AD by that Object ID
4. **Assigns to Service Principals**: Creates app role assignments between the groups and your configured Azure AD Service Principal(s)
5. **Result**: Azure AD now knows which groups are relevant for your Kubernetes authentication and will include only those in the JWT token

For detailed reconciliation lifecycle behavior (finalizers, conflict handling, cleanup guarantees, and RBAC escalation semantics), see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Prerequisites

- A Kubernetes cluster (v1.27+)
- Azure AD tenant with appropriate permissions
- Azure AD App Registration(s) used for Kubernetes OIDC authentication
- App registration for the controller with the following:
  - **Owner** of the app registration(s) used for Kubernetes OIDC authentication
  - `Application.ReadWrite.OwnedBy` permission (to manage applications it owns)
  - `Group.Read.All` permission (to look up groups)

**Authentication Options:**

- **Recommended**: Azure Workload Identity with AKS (requires OIDC issuer and Workload Identity webhook)
- **Alternative**: Manual Workload Identity setup with federated credentials (requires OIDC issuer, no webhook needed)
- **Alternative**: Client Secret authentication (works anywhere, less secure)
- **Alternative**: Azure Managed Identity (for Azure VMs/VMSS)

**Important**: The controller's app registration must be set as an owner of the cluster authentication app registration(s). This allows it to manage group assignments with only the `Application.ReadWrite.OwnedBy` permission instead of requiring broader `Application.ReadWrite.All` permissions.

## Installation

### 1. Configure Azure Workload Identity

Edit [config/deployment.yaml](config/deployment.yaml) and update the ServiceAccount annotation and environment variables:

**ServiceAccount annotation (required for Workload Identity):**

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  annotations:
    azure.workload.identity/client-id: "<CONTROLLER_CLIENT_ID>"
```

**Environment variables in the deployment:**

- `AZURE_SERVICE_PRINCIPALS`: Comma-separated list of Object IDs (not client IDs) of the service principals used for Kubernetes OIDC authentication
- `AZURE_TENANT_ID`: Your Azure AD tenant ID
- `AZURE_CLIENT_ID`: Automatically injected by Azure Workload Identity webhook from the ServiceAccount annotation. If not using Workload Identity, uncomment and set manually.
- `AZURE_CLIENT_SECRET`: (Optional) Only needed if using client secret authentication instead of Workload Identity
- `AZURE_FEDERATED_TOKEN_FILE`: Path to federated token file for Workload Identity (default: `/var/run/secrets/azure/tokens/azure-identity-token`)
- `AZURE_TOKEN_CREDENTIALS`: (Optional) Set to `prod` in production to disable Azure CLI, Azure Dev CLI, and Azure PowerShell authentication methods

**Finding Service Principal Object IDs:**

```bash
az ad sp show --id <CLIENT_ID> --query id -o tsv
```

**Setting up Workload Identity:**

```bash
# Create federated credential for the controller's app registration
az ad app federated-credential create \
  --id <CONTROLLER_APP_OBJECT_ID> \
  --parameters '{
    "name": "azure-k8s-role-assigner",
    "issuer": "https://oidc.prod-aks.azure.com/<TENANT_ID>/",
    "subject": "system:serviceaccount:azure-k8s-role-assigner:azure-k8s-role-assigner",
    "audiences": ["api://AzureADTokenExchange"]
  }'
```

**Setting up ownership**: Ensure the controller's app registration owns the cluster authentication app registration(s):

```bash
# Get the controller app's service principal object ID
CONTROLLER_SP_ID=$(az ad sp show --id <CONTROLLER_CLIENT_ID> --query id -o tsv)

# Add it as owner of the cluster auth app registration
az ad app owner add --id <CLUSTER_AUTH_APP_ID> --owner-object-id $CONTROLLER_SP_ID
```

### 2. Deploy the Controller

```bash
kubectl apply -f config/deployment.yaml
```

The deployment includes comprehensive security hardening:

- **Pod Security Standards**: Namespace enforces the `restricted` profile
- **Read-only root filesystem**: Container filesystem is immutable
- **Non-root user**: Runs as UID/GID 65532
- **Dropped capabilities**: All Linux capabilities dropped
- **No privilege escalation**: `allowPrivilegeEscalation: false`
- **Seccomp**: RuntimeDefault seccomp profile applied
- **No service account token**: `automountServiceAccountToken: false`

**High Availability:**

- **Leader election**: Enabled by default with `--leader-elect` flag
- **Multiple replicas**: Configured with 2 replicas for high availability
- Only one replica actively reconciles resources at a time; others are on standby
- Automatic failover if the leader pod fails
- You can scale to more replicas by adjusting `spec.replicas` in the deployment

### 3. Using Azure Workload Identity (Recommended for AKS)

The deployment is pre-configured for Azure Workload Identity:

1. **Ensure Azure Workload Identity is enabled on your AKS cluster:**

   ```bash
   # Check if Workload Identity is enabled
   az aks show --resource-group <RG_NAME> --name <CLUSTER_NAME> --query "oidcIssuerProfile.enabled"

   # Enable if not already enabled
   az aks update --resource-group <RG_NAME> --name <CLUSTER_NAME> --enable-oidc-issuer --enable-workload-identity
   ```

2. Update the `azure.workload.identity/client-id` annotation on the ServiceAccount with your controller's client ID
3. The pod template already has the `azure.workload.identity/use: "true"` label
4. The Azure Workload Identity webhook will automatically inject:
   - `AZURE_CLIENT_ID` environment variable
   - `AZURE_TENANT_ID` environment variable (if not already set)
   - Service account token volume mount
5. No `AZURE_CLIENT_SECRET` is needed
6. Ensure federated credentials are configured (see step 1 above)

### 4. Without Azure Workload Identity Webhook

If you're running on a cluster without the Azure Workload Identity webhook installed (non-AKS, self-managed cluster, or Workload Identity not enabled):

**Option A: Manual Workload Identity Setup (Federated Credentials)**

If your cluster has OIDC issuer configured but the webhook is not installed:

1. Keep the federated credential configuration from step 1
2. Manually configure the environment variables in [config/deployment.yaml](config/deployment.yaml):
   ```yaml
   env:
     - name: AZURE_CLIENT_ID
       value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
     - name: AZURE_TENANT_ID
       value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
     - name: AZURE_FEDERATED_TOKEN_FILE
       value: /var/run/secrets/azure/tokens/azure-identity-token
   ```
3. Ensure the service account token volume is mounted (already configured)
4. Remove or ignore the ServiceAccount annotation and pod label (they won't do anything without the webhook)

**Option B: Client Secret Authentication**

If you cannot use federated credentials at all:

1. Remove or ignore the ServiceAccount annotation: `azure.workload.identity/client-id`
2. Remove or ignore the pod label: `azure.workload.identity/use: "true"`
3. Uncomment and set these environment variables in [config/deployment.yaml](config/deployment.yaml):
   ```yaml
   env:
     - name: AZURE_CLIENT_ID
       value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
     - name: AZURE_CLIENT_SECRET
       value: "your-client-secret"
     - name: AZURE_TENANT_ID
       value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
   ```
4. Remove the `AZURE_FEDERATED_TOKEN_FILE` environment variable
5. Remove the azure-identity-token volume and volume mount (not needed for client secret auth)

**Option C: Managed Identity (Azure VMs/VMSS)**

If running on Azure VMs or VMSS with managed identity assigned:

1. Assign a user-assigned or system-assigned managed identity to your nodes
2. Grant the managed identity the required permissions
3. Remove or ignore the ServiceAccount annotation and pod label
4. Set only the required environment variable:

   ```yaml
   env:
     - name: AZURE_CLIENT_ID # Only needed for user-assigned managed identity
       value: "xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx"
   ```

5. DefaultAzureCredential will automatically detect and use the managed identity

## Configuration

### Environment Variables

The controller supports the following environment variables (configured directly in [config/deployment.yaml](config/deployment.yaml)):

- **`AZURE_SERVICE_PRINCIPALS`** (required): Comma-separated list of Azure AD Service Principal Object IDs
- **`AZURE_TENANT_ID`** (required): Azure AD tenant ID
- **`AZURE_CLIENT_ID`** (auto-injected by Workload Identity): Client ID of the controller's app registration. When using Workload Identity, this is automatically injected from the ServiceAccount annotation. For client secret auth, set manually.
- **`AZURE_CLIENT_SECRET`** (optional): Client secret (only if not using Workload Identity)
- **`AZURE_FEDERATED_TOKEN_FILE`** (optional): Path to federated token file for Workload Identity
- **`AZURE_TOKEN_CREDENTIALS`** (recommended for production): Set to `prod` to disable Azure CLI, Azure Dev CLI, and Azure PowerShell authentication methods

### Command-line Flags

- `--metrics-bind-address`: Address for metrics endpoint (default: `:8080`)
- `--health-probe-bind-address`: Address for health probes (default: `:8081`)
- `--leader-elect`: Enable leader election for high availability (enabled by default in the deployment)

## Development

### Prerequisites

- Go 1.22+
- Docker
- kubectl
- Access to a Kubernetes cluster

For detailed testing instructions with Minikube and Azure AD setup, see [TESTING.md](TESTING.md).

### Building

```bash
# Build the binary
make build

# Run locally (requires kubeconfig)
make run

# Run tests
make test

# Build Docker image
make docker-build IMG=your-registry/azure-k8s-role-assigner:tag

# Push Docker image
make docker-push IMG=your-registry/azure-k8s-role-assigner:tag
```

### Running Locally

To run the controller locally against your cluster:

1. Set up your Azure credentials:

```bash
export AZURE_TENANT_ID="your-tenant-id"
export AZURE_CLIENT_ID="your-client-id"
export AZURE_CLIENT_SECRET="your-client-secret"
export AZURE_SERVICE_PRINCIPALS="sp-object-id-1,sp-object-id-2"
```

2. Run the controller:

```bash
make run
```

## Monitoring

The controller exposes metrics on port 8080 at `/metrics` in Prometheus format. Health and readiness probes are available at `/healthz` and `/readyz` on port 8081.

## Troubleshooting

### Groups Not Being Assigned

1. Check controller logs: `kubectl logs -n azure-k8s-role-assigner deployment/azure-k8s-role-assigner`
2. Verify the referenced Azure group Object ID exists: `az ad group show --group <GROUP_OBJECT_ID>`
3. Ensure each `RoleBinding`/`ClusterRoleBinding` `subjects[].name` for `kind: Group` is the Azure group Object ID (UUID), not display name
4. Ensure the controller's app registration is an owner of the cluster auth app registration(s)
5. Ensure the controller's identity has the required Graph API permissions (`Application.ReadWrite.OwnedBy` and `Group.Read.All`)
6. Verify the service principal Object IDs are correct (not client IDs)

### Authentication Errors

1. Verify the `azure.workload.identity/client-id` annotation on the ServiceAccount matches your controller's client ID
2. Check that federated credentials are configured correctly for the controller's app registration
3. Ensure the controller's app registration has the required Graph API permissions (`Application.ReadWrite.OwnedBy` and `Group.Read.All`)
4. Verify the `AZURE_CLIENT_ID` environment variable is being injected (check pod env): `kubectl get pod -n azure-k8s-role-assigner -l app=azure-k8s-role-assigner -o jsonpath='{.items[0].spec.containers[0].env}'`
5. For client secret auth, verify credentials are set correctly in the deployment's environment variables
6. Ensure the controller's app registration owns the cluster auth app registration(s)

**If the Workload Identity webhook is not working:**

- Check if the webhook is installed: `kubectl get mutatingwebhookconfiguration azure-wi-webhook-mutating-webhook-configuration`
- Verify Workload Identity is enabled on AKS: `az aks show -g <RG> -n <CLUSTER> --query "oidcIssuerProfile.enabled"`
- If webhook is not available, manually set `AZURE_CLIENT_ID` in the deployment (see section 4 in Installation)
- For manual Workload Identity setup, ensure the service account token volume is correctly mounted at `/var/run/secrets/azure/tokens/azure-identity-token`
- Check the OIDC issuer URL matches the federated credential configuration

### Controller Not Starting

1. Check that the namespace exists: `kubectl get namespace azure-k8s-role-assigner`
2. Verify environment variables are set correctly: `kubectl describe pod -n azure-k8s-role-assigner -l app=azure-k8s-role-assigner`
3. Check for RBAC issues: `kubectl get clusterrolebinding azure-k8s-role-assigner`
4. Review pod logs for configuration errors: `kubectl logs -n azure-k8s-role-assigner -l app=azure-k8s-role-assigner`

## Testing

For a complete testing guide including setting up Azure AD app registrations, configuring Minikube with OIDC authentication, and verifying the controller functionality, see [TESTING.md](TESTING.md).

## Terraform Limited-Privilege Consent

To let Terraform grant only the required Microsoft Graph application permissions for this controller (without making Terraform a tenant admin), see [docs/TERRAFORM_LIMITED_CONSENT.md](docs/TERRAFORM_LIMITED_CONSENT.md).

## License

See [LICENSE](LICENSE) file for details.

## Contributing

Contributions are welcome! Please open an issue or pull request.
