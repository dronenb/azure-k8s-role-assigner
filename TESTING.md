# Testing Guide

This guide walks you through setting up a complete test environment with Minikube to test the azure-k8s-role-assigner controller.

## Prerequisites

- Azure CLI (`az`) installed and authenticated
- `minikube` installed
- `kubectl` installed
- `jq` installed
- Permissions to create app registrations in Azure AD
- Permissions to grant API permissions to app registrations

## Step 1: Create Azure AD App Registration for the Controller

This app registration will be used by the controller to authenticate to Azure AD and manage group assignments.

```bash
# Set variables
CONTROLLER_APP_NAME="azure-k8s-role-assigner-controller"
TENANT_ID=$(az account show --query tenantId -o tsv)

# Create the app registration
CONTROLLER_APP_ID=$(az ad app create \
  --display-name "${CONTROLLER_APP_NAME}" \
  --sign-in-audience AzureADMyOrg \
  --query appId -o tsv)

# Create service principal for the app
CONTROLLER_SP_ID=$(az ad sp create --id "${CONTROLLER_APP_ID}" --query id -o tsv)

# Get the app object ID (needed for some operations)
CONTROLLER_APP_OBJECT_ID=$(az ad app show --id "${CONTROLLER_APP_ID}" --query id -o tsv)

# Ensure current user is an owner (should be automatic, but making it explicit)
CURRENT_USER_ID=$(az ad signed-in-user show --query id -o tsv)
az ad app owner add \
  --id "${CONTROLLER_APP_OBJECT_ID}" \
  --owner-object-id "${CURRENT_USER_ID}" 2>/dev/null
```

## Step 2: Grant Microsoft Graph API Permissions to Controller

The controller needs permissions to read groups and manage application role assignments.

```bash
# Get Microsoft Graph API ID
GRAPH_API_ID="00000003-0000-0000-c000-000000000000"

# Group.Read.All - Application permission (read all groups)
GROUP_READ_ALL_ID="5b567255-7703-4780-807c-7be8301ae99b"

# Application.ReadWrite.OwnedBy - Application permission (manage owned apps)
APP_READWRITE_OWNEDBY_ID="18a4783c-866b-4cc7-a460-3d5e5662c884"

# Add required permissions
az ad app permission add \
  --id "${CONTROLLER_APP_ID}" \
  --api "${GRAPH_API_ID}" \
  --api-permissions "${GROUP_READ_ALL_ID}=Role" "${APP_READWRITE_OWNEDBY_ID}=Role"

# Grant admin consent (requires Global Admin or Privileged Role Admin)
az ad app permission admin-consent --id "${CONTROLLER_APP_ID}"
```

## Step 3: Create Azure AD App Registration for Cluster Authentication

This app registration will be used by the Kubernetes API server for OIDC authentication.

```bash
# Set variables
CLUSTER_APP_NAME="kubernetes-cluster-oidc"

# Create the app registration
CLUSTER_APP_ID=$(az ad app create \
  --display-name "${CLUSTER_APP_NAME}" \
  --sign-in-audience AzureADMyOrg \
  --query appId -o tsv)

# Create service principal for the app
CLUSTER_SP_ID=$(az ad sp create --id "${CLUSTER_APP_ID}" --query id -o tsv)

# Get the app object ID
CLUSTER_APP_OBJECT_ID=$(az ad app show --id "${CLUSTER_APP_ID}" --query id -o tsv)

# Ensure current user is an owner (should be automatic, but making it explicit)
CURRENT_USER_ID=$(az ad signed-in-user show --query id -o tsv)
az ad app owner add \
  --id "${CLUSTER_APP_OBJECT_ID}" \
  --owner-object-id "${CURRENT_USER_ID}" 2>/dev/null
```

## Step 4: Configure Cluster App Registration

Add the necessary configuration for the cluster authentication app.

```bash
# Add a redirect URI (optional, for web-based login flows)
az ad app update --id "${CLUSTER_APP_ID}" \
  --web-redirect-uris "http://localhost:8000" \
  --enable-id-token-issuance true

# Create an app role for the cluster
# This role will be assigned to groups by the controller
az ad app update --id "${CLUSTER_APP_ID}" \
  --app-roles "[{
    \"allowedMemberTypes\": [\"User\", \"Application\"],
    \"description\": \"Access to Kubernetes cluster\",
    \"displayName\": \"Cluster Access\",
    \"isEnabled\": true,
    \"value\": \"Cluster.Access\"
  }]"
CLUSTER_APP_ROLE_ID=$(az ad app show --id "${CLUSTER_APP_ID}" --query "appRoles[?value=='Cluster.Access'].id" -o tsv)

# Add Microsoft Graph API permissions for user authentication
# User.Read - allows reading user profile
az ad app permission add \
  --id "${CLUSTER_APP_ID}" \
  --api 00000003-0000-0000-c000-000000000000 \
  --api-permissions e1fe6dd8-ba31-4d61-89e7-88639da4683d=Scope

# profile - allows reading user's profile information (preferred_username, groups, etc.)
az ad app permission add \
  --id "${CLUSTER_APP_ID}" \
  --api 00000003-0000-0000-c000-000000000000 \
  --api-permissions 14dad69e-099b-42c9-810b-d002981feec1=Scope

# Add optional claims for UPN and groups (with cloud display name) in ID token and access token
az ad app update --id "${CLUSTER_APP_ID}" \
  --optional-claims "{
    \"idToken\": [
      {\"name\": \"upn\", \"essential\": false},
      {\"name\": \"groups\", \"essential\": false, \"additionalProperties\": [\"cloud_displayname\"]}
    ],
    \"accessToken\": [
      {\"name\": \"upn\", \"essential\": false},
      {\"name\": \"groups\", \"essential\": false, \"additionalProperties\": [\"cloud_displayname\"]}
    ]
  }"

# Configure group membership claims - only emit groups assigned to the application
# This prevents token bloat and is recommended for enterprise environments
az ad app update --id "${CLUSTER_APP_ID}" \
  --set groupMembershipClaims=ApplicationGroup

# Enable public client flows (for device code authentication)
az ad app update --id "${CLUSTER_APP_ID}" \
  --set isFallbackPublicClient=true

az ad app update  --id "${CLUSTER_APP_ID}" --set api='{"requestedAccessTokenVersion": 2}'
az ad app update  --id "${CLUSTER_APP_ID}" --identifier-uris "api://${CLUSTER_APP_ID}"
az ad app update \
  --id "${CLUSTER_APP_ID}" \
  --set api='{
    "oauth2PermissionScopes" : [
      {
        "adminConsentDescription": "Access Kubernetes",
        "adminConsentDisplayName": "Access Kubernetes",
        "id": "'$(uuidgen)'",
        "isEnabled": true,
        "type": "User",
        "userConsentDescription": "Access Kubernetes",
        "userConsentDisplayName": "Access Kubernetes",
        "value": "access"
      }
    ],
    "requestedAccessTokenVersion": 2
  }'
```

## Step 5: Set Controller as Owner of Cluster App Registration

This is critical - the controller app must own the cluster app to manage it with `Application.ReadWrite.OwnedBy` permission.

```bash
# Add controller's service principal as owner of cluster's app registration
az ad app owner add \
  --id "${CLUSTER_APP_OBJECT_ID}" \
  --owner-object-id "${CONTROLLER_SP_ID}"

# Add controller's service principal as owner of cluster's serivce principal
az rest --method POST \
  --url "https://graph.microsoft.com/v1.0/servicePrincipals/${CLUSTER_SP_ID}/owners/\$ref" \
  --headers "Content-Type=application/json" \
  --body "{\"@odata.id\": \"https://graph.microsoft.com/v1.0/directoryObjects/${CONTROLLER_SP_ID}\"}"

# Verify ownership (should show both current user and controller service principal)
az ad app owner list --id "${CLUSTER_APP_ID}" --query "[].{DisplayName:displayName, Id:id}" -o table
```

## Step 6: Create Test Azure AD Groups

Create some test groups to verify the controller functionality.

```bash
# Create test groups
TEST_GROUP_1="k8s-test-admins"
TEST_GROUP_2="k8s-test-developers"

GROUP_1_ID=$(az ad group create \
  --display-name "${TEST_GROUP_1}" \
  --mail-nickname "${TEST_GROUP_1}" \
  --query id -o tsv)

GROUP_2_ID=$(az ad group create \
  --display-name "${TEST_GROUP_2}" \
  --mail-nickname "${TEST_GROUP_2}" \
  --query id -o tsv)

# Add yourself to the test group (optional)
USER_ID=$(az ad signed-in-user show --query id -o tsv)
az ad group member add --group "${TEST_GROUP_1}" --member-id "${USER_ID}"
az ad group member add --group "${TEST_GROUP_2}" --member-id "${USER_ID}"
```

## Step 7: Create Azure Storage Account for Service Account Issuer

For Workload Identity Federation, you need to configure Kubernetes to use a **service account issuer** that Azure AD can verify. This requires hosting the service account's OIDC discovery and JWKS files publicly. Azure Storage with static website hosting provides this capability.

**Note**: This is different from the OIDC issuer for API server user authentication (which uses Azure AD's `login.microsoftonline.com`). This step configures the **service account token issuer** for pod service accounts.

```bash
# Set variables for the storage account
STORAGE_ACCOUNT_NAME="k8soidc${RANDOM}${RANDOM}"
RESOURCE_GROUP="k8s-oidc-federation"
LOCATION="eastus"
CONTAINER_NAME="\$web"

# Create resource group
az group create \
  --name "${RESOURCE_GROUP}" \
  --location "${LOCATION}"

# Create storage account
az storage account create \
  --name "${STORAGE_ACCOUNT_NAME}" \
  --resource-group "${RESOURCE_GROUP}" \
  --location "${LOCATION}" \
  --sku Standard_LRS \
  --kind StorageV2 \
  --allow-blob-public-access true

# Enable static website hosting
az storage blob service-properties update \
  --account-name "${STORAGE_ACCOUNT_NAME}" \
  --static-website \
  --404-document 404.html \
  --index-document index.html

# Get the primary endpoint for static website
OIDC_ISSUER_URL=$(az storage account show \
  --name "${STORAGE_ACCOUNT_NAME}" \
  --resource-group "${RESOURCE_GROUP}" \
  --query "primaryEndpoints.web" -o tsv | sed 's:/*$::')

```

**Note**: This service account issuer is used for Workload Identity Federation, allowing pod service accounts to authenticate to Azure AD. You'll configure Minikube to use this issuer in Step 8, and then upload the actual OIDC discovery document and JWKS directly from the cluster. For federated identity credentials on your app registration, configure them to trust this issuer URL with the appropriate subject claim (e.g., `system:serviceaccount:namespace:serviceaccountname`).

## Step 8: Create Minikube Cluster with OIDC Authentication

Create a Minikube cluster configured to use Azure AD for user authentication and the service account issuer for Workload Identity Federation.

```bash
# Delete existing minikube cluster if it exists
minikube delete

# Start Minikube with both OIDC user authentication and service account issuer
# Multi-tenant apps issue v2 tokens with login.microsoftonline.com issuer
minikube start \
  --driver=vfkit \
  --extra-config=apiserver.authorization-mode=Node,RBAC \
  --extra-config=apiserver.oidc-issuer-url="https://login.microsoftonline.com/${TENANT_ID}/v2.0" \
  --extra-config=apiserver.oidc-client-id="${CLUSTER_APP_ID}" \
  --extra-config=apiserver.oidc-username-claim=preferred_username \
  --extra-config=apiserver.oidc-username-prefix=- \
  --extra-config=apiserver.oidc-groups-claim=groups \
  --extra-config=apiserver.service-account-issuer="${OIDC_ISSUER_URL}" \
  --extra-config=apiserver.service-account-jwks-uri="${OIDC_ISSUER_URL}/openid/v1/jwks" \
  --cpus=2 \
  --memory=4096

# Verify the cluster is running
kubectl cluster-info

# Upload the OIDC discovery document
kubectl get --raw /.well-known/openid-configuration | \
  az storage blob upload \
    --account-name "${STORAGE_ACCOUNT_NAME}" \
    --container-name "\$web" \
    --name ".well-known/openid-configuration" \
    --data @/dev/stdin \
    --content-type "application/json" \
    --content-cache-control "no-cache, no-store, must-revalidate" \
    --overwrite

# Upload the JWKS
kubectl get --raw /openid/v1/jwks | \
  az storage blob upload \
    --account-name "${STORAGE_ACCOUNT_NAME}" \
    --container-name "\$web" \
    --name "openid/v1/jwks" \
    --data @/dev/stdin \
    --content-type "application/json" \
    --content-cache-control "no-cache, no-store, must-revalidate" \
    --overwrite

# Verify the OIDC endpoints are accessible
curl -s "${OIDC_ISSUER_URL}/.well-known/openid-configuration" | jq .
curl -s "${OIDC_ISSUER_URL}/openid/v1/jwks" | jq .
```

**Key Configuration Notes**:

- `--oidc-issuer-url`: For **user authentication** (multi-tenant apps use v2 endpoint: `login.microsoftonline.com/${TENANT_ID}/v2.0`)
- `--service-account-issuer`: For **service account tokens** (Azure AD validates pod service account tokens for Workload Identity against your storage account URL)

## Local Development: Run Controller Locally (Alternative to Steps 9-10)

For development and testing, you can run the controller locally instead of deploying it to the cluster. This approach is faster for iterative development and debugging.

**Prerequisites**: Complete Steps 1-8 above (Minikube must be running).

```bash
# Get the cluster service principal object ID and app role ID
CLUSTER_SP_ID=$(az ad sp list --filter "appId eq '${CLUSTER_APP_ID}'" --query "[0].id" -o tsv)
CLUSTER_APP_ROLE_ID=$(az ad app show --id "${CLUSTER_APP_ID}" --query "appRoles[?value=='Cluster.Access'].id" -o tsv)

# Verify the values are set
echo "CLUSTER_SP_ID: ${CLUSTER_SP_ID}"
echo "CLUSTER_APP_ROLE_ID: ${CLUSTER_APP_ROLE_ID}"

# Run the controller locally using Azure CLI credentials
# The controller will connect to your Minikube cluster using your current kubectl context
AZURE_SERVICE_PRINCIPALS="${CLUSTER_SP_ID}" \
AZURE_APP_ROLE_ID="${CLUSTER_APP_ROLE_ID}" \
go run cmd/main.go
```

**What this does**:
- Uses your Azure CLI credentials for authentication (via `DefaultAzureCredential`)
- Connects to your current kubectl context (Minikube)
- Watches RoleBindings and ClusterRoleBindings for groups
- Manages Azure AD app role assignments in real-time

**To test the cleanup functionality**:

In a separate terminal:
```bash
# Create a test binding with a group
kubectl create clusterrolebinding test-cleanup \
  --clusterrole=view \
  --group=k8s-test-developers

# Watch the controller logs - should show group assignment

# Delete the binding to test cleanup
kubectl delete clusterrolebinding test-cleanup

# Watch the controller logs - should show cleanup and Azure AD removal
```

**To stop the controller**: Press `Ctrl+C` in the terminal running the controller.

**Note**: When running locally, you must have Azure CLI authenticated and have permissions to manage the app registrations. If using this approach, skip Steps 9-10 (Build/Deploy) and proceed directly to creating test RoleBindings.

## Step 9: Build and Load Controller Image

Build the controller image and load it into the Minikube registry.

```bash
# Build the image with podman using localhost prefix
podman build -t localhost/azure-k8s-role-assigner:latest .

# Save the image to a tar file and load it into Minikube
podman save localhost/azure-k8s-role-assigner:latest | minikube image load -

# Verify the image is available in Minikube
minikube image ls | grep azure-k8s-role-assigner
```

**Note**: When using the podman driver, we build with podman using the `localhost/` prefix to make it clear this is a local image, then pipe the saved image directly to `minikube image load` to transfer it into Minikube's container runtime.

## Step 10: Deploy the Controller with Workload Identity Federation

Configure federated credentials and deploy the controller.

```bash
# Create the namespace and service account
kubectl apply -f config/deployment.yaml

# Get the service account details
SA_NAMESPACE="azure-k8s-role-assigner"
SA_NAME="azure-k8s-role-assigner"
SA_ISSUER="${OIDC_ISSUER_URL}"
SA_SUBJECT="system:serviceaccount:${SA_NAMESPACE}:${SA_NAME}"

# Create federated identity credential
az ad app federated-credential create \
  --id "${CONTROLLER_APP_ID}" \
  --parameters "{
    \"name\": \"kubernetes-federated-credential\",
    \"issuer\": \"${SA_ISSUER}\",
    \"subject\": \"${SA_SUBJECT}\",
    \"audiences\": [\"api://AzureADTokenExchange\"]
  }"

# Update the deployment with your actual values
cat > /tmp/patch.yaml <<EOF
spec:
  template:
    metadata:
      labels:
        azure.workload.identity/use: "true"
    spec:
      automountServiceAccountToken: true
      containers:
        - name: manager
          image: localhost/azure-k8s-role-assigner:latest
          imagePullPolicy: IfNotPresent
          env:
            - name: AZURE_SERVICE_PRINCIPALS
              value: "${CLUSTER_SP_ID}"
            - name: AZURE_TENANT_ID
              value: "${TENANT_ID}"
            - name: AZURE_CLIENT_ID
              value: "${CONTROLLER_APP_ID}"
            - name: AZURE_APP_ROLE_ID
              value: "${CLUSTER_APP_ROLE_ID}"
EOF

# Patch the deployment with workload identity settings
kubectl patch deployment azure-k8s-role-assigner \
  -n azure-k8s-role-assigner \
  --type strategic \
  --patch-file /tmp/patch.yaml

# Wait for the deployment to be ready
kubectl rollout status deployment/azure-k8s-role-assigner -n azure-k8s-role-assigner
```

**Note**:

- The `imagePullPolicy: IfNotPresent` setting tells Kubernetes to check locally first before attempting to pull from a remote registry. The `localhost/` prefix in the image name makes it explicit that this is a local image.

## Step 10: Create Test RoleBindings

Create RoleBindings that reference the Azure AD groups to test the controller.

```bash
# Create a test namespace
kubectl create namespace test-app

# Create a ClusterRoleBinding for test-admins
kubectl create clusterrolebinding test-admins-binding \
  --clusterrole=cluster-admin \
  --group="${TEST_GROUP_1}"

# Create a RoleBinding for test-developers
kubectl create rolebinding test-developers-binding \
  --namespace=test-app \
  --clusterrole=view \
  --group="${TEST_GROUP_2}"
```

## Step 12: Verify Controller Functionality

Check that the controller is working correctly.

```bash
# Check controller logs
kubectl logs -n azure-k8s-role-assigner -l app=azure-k8s-role-assigner --tail=50

# Verify the controller processed the RoleBindings
kubectl logs -n azure-k8s-role-assigner -l app=azure-k8s-role-assigner \
  | grep -i "processing group\|assigned group\|found group"

# Check if groups are assigned to the service principal using Azure CLI
az rest --method GET \
  --url "https://graph.microsoft.com/v1.0/servicePrincipals/${CLUSTER_SP_ID}/appRoleAssignedTo" \
  | jq -r '.value[] | "\(.principalDisplayName) (Principal ID: \(.principalId))"'
```

## Step 13: Test Group-Based Authentication (Optional)

To test actual authentication with groups, you would need to:

1. Get an access token for a user in one of the test groups
2. Configure kubectl to use that token
3. Verify access based on the RoleBindings

```bash
kubectl config set-credentials azure-user \
  --exec-api-version=client.authentication.k8s.io/v1beta1 \
  --exec-command=kubelogin \
  --exec-arg=get-token \
  --exec-arg=--login \
  --exec-arg=devicecode \
  --exec-arg=--server-id \
  --exec-arg="${CLUSTER_APP_ID}" \
  --exec-arg=--client-id \
  --exec-arg="${CLUSTER_APP_ID}" \
  --exec-arg=--tenant-id \
  --exec-arg="${TENANT_ID}" \
  --exec-arg=--legacy=false
kubectl config set-context azure-minikube \
  --cluster=minikube \
  --user=azure-user
kubectl auth whoami --context=azure-minikube
```

Decode the JWT:

```bash
kubelogin get-token --login devicecode --server-id "${CLUSTER_APP_ID}" --client-id "${CLUSTER_APP_ID}" --tenant-id "${TENANT_ID}" --legacy=false | jq -r '.status.token' | jc --jwt -p
```

## Cleanup

When you're done testing, clean up the resources.

```bash
# Delete Minikube cluster
minikube delete

# Delete test groups
az ad group delete --group "${GROUP_1_ID}"
az ad group delete --group "${GROUP_2_ID}"

# Delete test app registrations (optional - you may want to keep these for future testing)

az ad app delete --id "${CONTROLLER_APP_ID}"
az ad app delete --id "${CLUSTER_APP_ID}"

# Delete storage account resource group (if created in Step 7)
az group delete --name "${RESOURCE_GROUP}" --yes --no-wait
```

## Notes

- **Authentication**: This guide uses Workload Identity Federation with federated credentials. The controller authenticates using the Kubernetes service account token projected into the pod, eliminating the need for client secrets.
- **OIDC Issuer**: The storage account created in Step 7 hosts the OIDC discovery document and JWKS that Azure AD uses to validate the Kubernetes service account tokens.
- **Permissions**: The controller needs `Application.ReadWrite.OwnedBy` and `Group.Read.All` permissions. These are application permissions that require admin consent.
- **Ownership**: The controller app MUST be an owner of the cluster app registration for the `Application.ReadWrite.OwnedBy` permission to work.
- **Group Names**: The controller matches groups by `displayName` in Azure AD.
- **App Role ID**: The controller needs the `AZURE_APP_ROLE_ID` environment variable set to the ID of the app role (created in Step 4) that groups should be assigned to. This is retrieved using the query `appRoles[?value=='Cluster.Access'].id`.
- **Federated Credential**: The federated identity credential trusts tokens issued by your Kubernetes cluster's service account issuer for the specific service account (`system:serviceaccount:azure-k8s-role-assigner:azure-k8s-role-assigner`).
