package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"

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
	roleBindingFinalizerName = "azure-k8s-role-assigner.dronenb.github.io/finalizer"
)

// RoleBindingReconciler reconciles a RoleBinding object
type RoleBindingReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	AzureClient *azure.Client
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;patch

// Reconcile processes RoleBinding events
func (r *RoleBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling RoleBinding", "namespace", req.Namespace, "name", req.Name)

	// Fetch the RoleBinding
	roleBinding := &rbacv1.RoleBinding{}
	err := r.Get(ctx, req.NamespacedName, roleBinding)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// RoleBinding was deleted, nothing to do (finalizer already handled cleanup)
			logger.Info("RoleBinding not found, likely deleted", "namespace", req.Namespace, "name", req.Name)
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to get RoleBinding")
		return reconcile.Result{}, err
	}

	// Extract current groups from subjects
	currentGroups := extractGroupsFromSubjects(roleBinding.Subjects)

	// Capture original for patch base before any mutations.
	original := roleBinding.DeepCopy()

	// If no Azure groups, remove finalizer and skip management (e.g., system bindings)
	if len(currentGroups) == 0 {
		logger.Info("No Azure groups found, skipping management", "namespace", req.Namespace, "name", req.Name)
		if controllerutil.ContainsFinalizer(roleBinding, roleBindingFinalizerName) {
			controllerutil.RemoveFinalizer(roleBinding, roleBindingFinalizerName)
			if err := r.patchRoleBindingFinalizers(ctx, roleBinding, original); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to remove finalizer for rolebinding %s/%s: %w", req.Namespace, req.Name, err)
			}
		}
		return reconcile.Result{}, nil
	}

	// Check if object is being deleted
	if !roleBinding.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(roleBinding, roleBindingFinalizerName) {
			// Handle cleanup before deletion
			logger.Info("RoleBinding is being deleted, cleaning up Azure AD assignments")
			if err := r.cleanupGroups(ctx, currentGroups); err != nil {
				logger.Error(err, "Failed to cleanup Azure AD assignments")
				return reconcile.Result{}, err
			}

			// Remove finalizer to allow deletion
			controllerutil.RemoveFinalizer(roleBinding, roleBindingFinalizerName)
			if err := r.patchRoleBindingFinalizers(ctx, roleBinding, original); err != nil {
				return reconcile.Result{}, fmt.Errorf("failed to remove finalizer during delete for rolebinding %s/%s: %w", req.Namespace, req.Name, err)
			}
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(roleBinding, roleBindingFinalizerName) {
		controllerutil.AddFinalizer(roleBinding, roleBindingFinalizerName)
		if err := r.patchRoleBindingFinalizers(ctx, roleBinding, original); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to add finalizer for rolebinding %s/%s: %w", req.Namespace, req.Name, err)
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
func (r *RoleBindingReconciler) processGroup(ctx context.Context, groupID string) error {
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
func (r *RoleBindingReconciler) cleanupGroups(ctx context.Context, groupIDs []string) error {
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

// isValidAzureUUID checks if a string is a valid Azure UUID (36-char format)
func isValidAzureUUID(s string) bool {
	uuidRegex := regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	return uuidRegex.MatchString(strings.ToLower(s))
}

// extractGroupsFromSubjects extracts unique group IDs from subjects, filtering system groups and non-UUID groups
func extractGroupsFromSubjects(subjects []rbacv1.Subject) []string {
	groups := []string{}
	seen := make(map[string]bool)

	for _, subject := range subjects {
		if subject.Kind != "Group" {
			continue
		}

		groupID := subject.Name

		// Skip Kubernetes built-in groups
		if strings.HasPrefix(groupID, "system:") || strings.HasPrefix(groupID, "kubeadm:") {
			continue
		}

		// Only process valid Azure UUIDs
		if !isValidAzureUUID(groupID) {
			continue
		}

		if !seen[groupID] {
			groups = append(groups, groupID)
			seen[groupID] = true
		}
	}

	return groups
}

// containsGroup checks if subjects contain a specific group ID
func containsGroup(subjects []rbacv1.Subject, groupID string) bool {
	for _, subject := range subjects {
		if subject.Kind == "Group" && subject.Name == groupID {
			return true
		}
	}
	return false
}

// difference returns elements in a that are not in b
func difference(a, b []string) []string {
	mb := make(map[string]bool, len(b))
	for _, x := range b {
		mb[x] = true
	}

	var diff []string
	for _, x := range a {
		if !mb[x] {
			diff = append(diff, x)
		}
	}
	return diff
}

func (r *RoleBindingReconciler) patchRoleBindingFinalizers(ctx context.Context, rb *rbacv1.RoleBinding, original *rbacv1.RoleBinding) error {
	return r.Patch(ctx, rb, client.MergeFrom(original))
}

// SetupWithManager sets up the controller with the Manager
func (r *RoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rbacv1.RoleBinding{}).
		Complete(r)
}
