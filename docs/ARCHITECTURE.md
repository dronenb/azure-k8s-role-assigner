# Controller Architecture

This document explains how the controller works today, including its read-only RBAC model, reconcile strategy, conflict handling, and cleanup guarantees.

## Purpose

The controller watches Kubernetes RBAC bindings and ensures Azure AD group assignments exist for groups referenced by those bindings, so Entra-issued tokens include the relevant groups for cluster authorization.

In scope:

- `RoleBinding` subjects of kind `Group`
- `ClusterRoleBinding` subjects of kind `Group`

Out of scope:

- Users or service accounts in binding subjects
- Non-UUID group subject names
- Kubernetes built-in groups (for example `system:*`, `kubeadm:*`)

## High-Level Flow

For each reconcile event (`RoleBinding` or `ClusterRoleBinding`):

1. Read the binding.
2. Extract candidate group IDs from `subjects[].name` where `kind=Group`.
3. Filter candidates to valid Azure UUIDs only.
4. For each valid Azure group, validate in Azure AD and assign to configured service principals.
5. Compute desired state from all current bindings and remove stale Azure assignments that are no longer referenced.
6. Requeue periodically for full convergence (eventual consistency), even if no binding events arrive.

## Group Extraction and Validation

Group extraction is intentionally strict:

- only `subject.kind == Group`
- ignores `system:*` and `kubeadm:*`
- ignores non-UUID values
- de-duplicates within a single binding

This avoids attempting Azure operations for Kubernetes/system identities and prevents noisy or invalid Graph API calls.

## Why We Do Not Use Finalizers

This controller intentionally does not write to `RoleBinding` or `ClusterRoleBinding` objects.

Why:

- Binding updates are privilege-sensitive in Kubernetes RBAC and can trigger escalation checks.
- Even metadata-only changes on binding resources require write permissions that are broader than desired for this controller.
- For `RoleBinding` and `ClusterRoleBinding` in this environment, finalizers are not exposed as a dedicated `/finalizers` subresource; they are part of object metadata on the main resource.
- As a result, adding or removing finalizers still requires write access (`patch`/`update`) on the binding resources themselves.
- The target security posture is read-only access to binding resources (`get`, `list`, `watch`) with no `patch` or `update`.

Instead of delete-time hooks, cleanup is achieved through periodic full reconciliation against live cluster state and current Azure assignments.

## Why Not Metadata Tracking

Updating RoleBinding/ClusterRoleBinding object metadata (including annotations) can trigger Kubernetes RBAC escalation protections. In clusters with high-privilege/system bindings, metadata writes may be denied with errors like:

- `attempting to grant RBAC permissions not currently held`

Because this controller is read-only for binding resources, it does not use annotations or finalizers for lifecycle tracking.

## Why RBAC “grant” Errors Happen

Kubernetes treats RBAC binding writes as potentially privilege-granting operations and performs escalation checks on updates. Even if your controller intends to mutate metadata, updates on binding objects can be denied if the caller cannot grant the referenced role permissions.

The controller avoids this class of failures by not mutating binding resources.

## Cleanup and Leak Prevention

Cleanup is designed for many-to-many relationships:

- a group can be referenced by multiple bindings
- a binding can include multiple groups

During reconciliation, the controller:

1. Lists all `RoleBinding` resources.
2. Lists all `ClusterRoleBinding` resources.
3. Builds the desired set of Azure group assignments from all live bindings.
4. Removes Azure assignment only if no references remain in that desired set.

This prevents premature de-assignment when multiple bindings share the same group and helps prevent cloud-side orphaning.

## Conflict Handling and Idempotency

- Azure assignment calls are effectively idempotent in practice; "already exists" conditions are logged and reconciliation continues.
- Group-level failures do not abort processing of other groups in the same binding reconcile.
- Reconciles are level-based and periodic, so retries naturally converge when transient failures clear.

## Failure Modes and Behavior

1. Azure group lookup fails:
   - group processing logs error
   - other groups continue

2. Azure assignment removal fails during cleanup:
   - error logged
   - cleanup continues for other groups

3. Assignment already exists in Azure:
   - treated as success (idempotent state)
   - reconciliation continues

4. Binding contains no valid Azure UUID groups:
   - no assignment work for that binding
   - global cleanup still enforces convergence

## Alternative Considered: Dedicated CRD

An alternative design was to create a controller-owned custom resource for each discovered Azure group reference (for example, an `AzureGroupBinding` CR) and attach finalizers to those CRs.

Potential advantages:

- Clear Kubernetes-native inventory of desired assignments.
- Finalizers on controller-owned resources avoid mutating RBAC bindings directly.

Tradeoffs that led to not choosing this design:

- Additional API surface and operational overhead (CRD schema/versioning, migration, RBAC, validation).
- Extra reconciliation paths for parent-child lifecycle and orphan handling.
- More moving parts for little benefit over periodic full reconciliation in this use case.

Given the requirement to keep permissions narrow and implementation simple, periodic reconcile with read-only binding access was chosen.

## Operational Notes

- This controller expects `subject.name` for `Group` subjects to be Azure AD group object IDs.
- If your RBAC definitions use names that are not Azure UUIDs, they are intentionally ignored by design.
- The controller does not mutate `RoleBinding` or `ClusterRoleBinding` objects.

## Code Map

- RoleBinding reconciler:
  - `internal/controller/rolebinding_controller.go`
- ClusterRoleBinding reconciler:
  - `internal/controller/clusterrolebinding_controller.go`
