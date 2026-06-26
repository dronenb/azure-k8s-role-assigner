package controller

import (
	"context"
	"fmt"

	"github.com/dronenb/azure-k8s-role-assigner/internal/azure"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	clusterRoleBindingFinalizerName = "azure-k8s-role-assigner.dronenb.github.io/finalizer"
)

// ClusterRoleBindingReconciler reconciles a ClusterRoleBinding object
type ClusterRoleBindingReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	AzureClient *azure.Client
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;patch

// Reconcile processes ClusterRoleBinding events
func (r *ClusterRoleBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ClusterRoleBinding", "name", req.Name)

	// Fetch the ClusterRoleBinding
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, req.NamespacedName, clusterRoleBinding)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// ClusterRoleBinding was deleted, nothing to do (finalizer already handled cleanup)
			logger.Info("ClusterRoleBinding not found, likely deleted", "name", req.Name)
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to get ClusterRoleBinding")
		return reconcile.Result{}, err
	}

	// Extract current groups from subjects
	currentGroups := extractGroupsFromSubjects(clusterRoleBinding.Subjects)

	// Capture original for patch base before any mutations.
	original := clusterRoleBinding.DeepCopy()

	// If no Azure groups, remove finalizer and skip management (e.g., system bindings)
	if len(currentGroups) == 0 {
		logger.Info("No Azure groups found, skipping management", "name", req.Name)
		if controllerutil.ContainsFinalizer(clusterRoleBinding, clusterRoleBindingFinalizerName) {
			controllerutil.RemoveFinalizer(clusterRoleBinding, clusterRoleBindingFinalizerName)
			if err := r.patchClusterRoleBindingFinalizers(ctx, clusterRoleBinding, original); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to remove finalizer for clusterrolebinding %s: %w", req.Name, err)
			}
		}
		return reconcile.Result{}, nil
	}

	// Check if object is being deleted
	if !clusterRoleBinding.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(clusterRoleBinding, clusterRoleBindingFinalizerName) {
			// Handle cleanup before deletion
			logger.Info("ClusterRoleBinding is being deleted, cleaning up Azure AD assignments")
			if err := r.cleanupGroups(ctx, currentGroups); err != nil {
				logger.Error(err, "Failed to cleanup Azure AD assignments")
				return reconcile.Result{}, err
			}

			// Remove finalizer to allow deletion
			controllerutil.RemoveFinalizer(clusterRoleBinding, clusterRoleBindingFinalizerName)
			if err := r.patchClusterRoleBindingFinalizers(ctx, clusterRoleBinding, original); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to remove finalizer during delete for clusterrolebinding %s: %w", req.Name, err)
			}
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(clusterRoleBinding, clusterRoleBindingFinalizerName) {
		controllerutil.AddFinalizer(clusterRoleBinding, clusterRoleBindingFinalizerName)
		if err := r.patchClusterRoleBindingFinalizers(ctx, clusterRoleBinding, original); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to add finalizer for clusterrolebinding %s: %w", req.Name, err)
		}
		return reconcile.Result{Requeue: true}, nil
	}

	logger.Info("Ensuring groups are assigned in Azure AD", "groups", currentGroups)
	for _, groupID := range currentGroups {
		if err := r.processGroup(ctx, groupID); err != nil {
			logger.Error(err, "Failed to process group", "groupID", groupID)
			// Continue processing other groups even if one fails
			continue
		}
	}

	return reconcile.Result{}, nil
}

// processGroup handles assignment for a single group
func (r *ClusterRoleBindingReconciler) processGroup(ctx context.Context, groupID string) error {
	logger := log.FromContext(ctx)

	logger.Info("Processing group", "groupID", groupID)

	// Validate group exists in Azure by object ID from subject.name.
	group, err := r.AzureClient.GetGroupByID(ctx, groupID)
	if err != nil {
		return fmt.Errorf("failed to find group in Azure: %w", err)
	}

	resolvedGroupID := group.GetId()
	if resolvedGroupID == nil {
		return fmt.Errorf("group ID is nil for input %s", groupID)
	}

	logger.Info("Found group in Azure", "groupID", *resolvedGroupID)

	// Assign group to service principals
	if err := r.AzureClient.AssignGroupToServicePrincipals(ctx, *resolvedGroupID); err != nil {
		return fmt.Errorf("failed to assign group to service principals: %w", err)
	}

	logger.Info("Successfully assigned group to service principals", "groupID", *resolvedGroupID)
	return nil
}

// cleanupGroups removes Azure AD assignments for groups that are no longer referenced
func (r *ClusterRoleBindingReconciler) cleanupGroups(ctx context.Context, groupIDs []string) error {
	logger := log.FromContext(ctx)

	// For each group, check if it's still referenced in any other RoleBinding or ClusterRoleBinding
	for _, groupID := range groupIDs {
		// Check all RoleBindings
		roleBindings := &rbacv1.RoleBindingList{}
		if err := r.List(ctx, roleBindings); err != nil {
			return fmt.Errorf("failed to list rolebindings: %w", err)
		}

		// Check all ClusterRoleBindings
		clusterRoleBindings := &rbacv1.ClusterRoleBindingList{}
		if err := r.List(ctx, clusterRoleBindings); err != nil {
			return fmt.Errorf("failed to list clusterrolebindings: %w", err)
		}

		// Check if group is still referenced
		stillReferenced := false
		for _, rb := range roleBindings.Items {
			if rb.DeletionTimestamp.IsZero() && containsGroup(rb.Subjects, groupID) {
				stillReferenced = true
				break
			}
		}
		if !stillReferenced {
			for _, crb := range clusterRoleBindings.Items {
				if crb.DeletionTimestamp.IsZero() && containsGroup(crb.Subjects, groupID) {
					stillReferenced = true
					break
				}
			}
		}

		// If group is not referenced anywhere, remove from Azure AD
		if !stillReferenced {
			logger.Info("Group is no longer referenced, removing from Azure AD", "groupID", groupID)
			group, err := r.AzureClient.GetGroupByID(ctx, groupID)
			if err != nil {
				logger.Error(err, "Failed to find group in Azure during cleanup", "groupID", groupID)
				continue
			}

			resolvedGroupID := group.GetId()
			if resolvedGroupID == nil {
				logger.Error(fmt.Errorf("group ID is nil"), "Group ID is nil during cleanup", "groupID", groupID)
				continue
			}

			if err := r.AzureClient.RemoveGroupFromServicePrincipals(ctx, *resolvedGroupID); err != nil {
				logger.Error(err, "Failed to remove group from service principals", "groupID", groupID)
				continue
			}
			logger.Info("Successfully removed group from Azure AD", "groupID", *resolvedGroupID)
		} else {
			logger.Info("Group is still referenced in other bindings, skipping removal", "groupID", groupID)
		}
	}

	return nil
}

func (r *ClusterRoleBindingReconciler) patchClusterRoleBindingFinalizers(ctx context.Context, crb *rbacv1.ClusterRoleBinding, original *rbacv1.ClusterRoleBinding) error {
	return r.Patch(ctx, crb, client.MergeFrom(original))
}

// SetupWithManager sets up the controller with the Manager
func (r *ClusterRoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rbacv1.ClusterRoleBinding{}).
		Complete(r)
}
