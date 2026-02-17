package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dronenb/azure-k8s-role-assigner/internal/azure"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	roleBindingFinalizerName = "azure-k8s-role-assigner.dronenb.github.io/finalizer"
	groupsAnnotationKey      = "azure-k8s-role-assigner.dronenb.github.io/managed-groups"
)

// RoleBindingReconciler reconciles a RoleBinding object
type RoleBindingReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	AzureClient *azure.Client
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings/status,verbs=get
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings/finalizers,verbs=update

// Reconcile processes RoleBinding events
func (r *RoleBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling RoleBinding", "namespace", req.Namespace, "name", req.Name)

	// Fetch the RoleBinding
	roleBinding := &rbacv1.RoleBinding{}
	err := r.Get(ctx, req.NamespacedName, roleBinding)
	if err != nil {
		if errors.IsNotFound(err) {
			// RoleBinding was deleted, nothing to do (finalizer already handled cleanup)
			logger.Info("RoleBinding not found, likely deleted", "namespace", req.Namespace, "name", req.Name)
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to get RoleBinding")
		return reconcile.Result{}, err
	}

	// Extract current groups from subjects
	currentGroups := extractGroupsFromSubjects(roleBinding.Subjects)

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
			if err := r.Update(ctx, roleBinding); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(roleBinding, roleBindingFinalizerName) {
		controllerutil.AddFinalizer(roleBinding, roleBindingFinalizerName)
		if err := r.Update(ctx, roleBinding); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{Requeue: true}, nil
	}

	// Get previously managed groups from annotation
	previousGroups := r.getManagedGroups(roleBinding)

	// Determine groups to add and remove
	groupsToAdd := difference(currentGroups, previousGroups)
	groupsToRemove := difference(previousGroups, currentGroups)

	// Remove groups that are no longer in subjects
	if len(groupsToRemove) > 0 {
		logger.Info("Removing groups from Azure AD", "groups", groupsToRemove)
		if err := r.cleanupGroups(ctx, groupsToRemove); err != nil {
			logger.Error(err, "Failed to remove groups from Azure AD")
			return reconcile.Result{}, err
		}
	}

	// Process new/current groups
	if len(groupsToAdd) > 0 {
		logger.Info("Adding groups to Azure AD", "groups", groupsToAdd)
		for _, groupName := range groupsToAdd {
			if err := r.processGroup(ctx, groupName); err != nil {
				logger.Error(err, "Failed to process group", "groupName", groupName)
				// Continue processing other groups even if one fails
				continue
			}
		}
	}

	// Update the managed groups annotation
	if err := r.updateManagedGroups(ctx, roleBinding, currentGroups); err != nil {
		logger.Error(err, "Failed to update managed groups annotation")
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

// processGroup handles assignment for a single group
func (r *RoleBindingReconciler) processGroup(ctx context.Context, groupName string) error {
	logger := log.FromContext(ctx)

	logger.Info("Processing group", "groupName", groupName)

	// Check if group exists in Azure
	group, err := r.AzureClient.GetGroupByName(ctx, groupName)
	if err != nil {
		return fmt.Errorf("failed to find group in Azure: %w", err)
	}

	groupID := group.GetId()
	if groupID == nil {
		return fmt.Errorf("group ID is nil for group %s", groupName)
	}

	logger.Info("Found group in Azure", "groupName", groupName, "groupID", *groupID)

	// Assign group to service principals
	if err := r.AzureClient.AssignGroupToServicePrincipals(ctx, *groupID); err != nil {
		return fmt.Errorf("failed to assign group to service principals: %w", err)
	}

	logger.Info("Successfully assigned group to service principals", "groupName", groupName, "groupID", *groupID)
	return nil
}

// cleanupGroups removes Azure AD assignments for groups that are no longer referenced
func (r *RoleBindingReconciler) cleanupGroups(ctx context.Context, groupNames []string) error {
	logger := log.FromContext(ctx)

	// For each group, check if it's still referenced in any other RoleBinding or ClusterRoleBinding
	for _, groupName := range groupNames {
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
			if rb.DeletionTimestamp.IsZero() && containsGroup(rb.Subjects, groupName) {
				stillReferenced = true
				break
			}
		}
		if !stillReferenced {
			for _, crb := range clusterRoleBindings.Items {
				if crb.DeletionTimestamp.IsZero() && containsGroup(crb.Subjects, groupName) {
					stillReferenced = true
					break
				}
			}
		}

		// If group is not referenced anywhere, remove from Azure AD
		if !stillReferenced {
			logger.Info("Group is no longer referenced, removing from Azure AD", "groupName", groupName)
			group, err := r.AzureClient.GetGroupByName(ctx, groupName)
			if err != nil {
				logger.Error(err, "Failed to find group in Azure during cleanup", "groupName", groupName)
				continue
			}

			groupID := group.GetId()
			if groupID == nil {
				logger.Error(fmt.Errorf("group ID is nil"), "Group ID is nil during cleanup", "groupName", groupName)
				continue
			}

			if err := r.AzureClient.RemoveGroupFromServicePrincipals(ctx, *groupID); err != nil {
				logger.Error(err, "Failed to remove group from service principals", "groupName", groupName)
				continue
			}
			logger.Info("Successfully removed group from Azure AD", "groupName", groupName)
		} else {
			logger.Info("Group is still referenced in other bindings, skipping removal", "groupName", groupName)
		}
	}

	return nil
}

// getManagedGroups retrieves the list of groups managed by this controller from the annotation
func (r *RoleBindingReconciler) getManagedGroups(rb *rbacv1.RoleBinding) []string {
	if rb.Annotations == nil {
		return []string{}
	}

	groupsJSON, ok := rb.Annotations[groupsAnnotationKey]
	if !ok {
		return []string{}
	}

	var groups []string
	if err := json.Unmarshal([]byte(groupsJSON), &groups); err != nil {
		return []string{}
	}

	return groups
}

// updateManagedGroups updates the annotation with the current list of managed groups
func (r *RoleBindingReconciler) updateManagedGroups(ctx context.Context, rb *rbacv1.RoleBinding, groups []string) error {
	if rb.Annotations == nil {
		rb.Annotations = make(map[string]string)
	}

	groupsJSON, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("failed to marshal groups: %w", err)
	}

	rb.Annotations[groupsAnnotationKey] = string(groupsJSON)
	return r.Update(ctx, rb)
}

// extractGroupsFromSubjects extracts unique group names from subjects, filtering system groups
func extractGroupsFromSubjects(subjects []rbacv1.Subject) []string {
	groups := []string{}
	seen := make(map[string]bool)

	for _, subject := range subjects {
		if subject.Kind != "Group" {
			continue
		}

		groupName := subject.Name

		// Skip Kubernetes built-in groups
		if strings.HasPrefix(groupName, "system:") || strings.HasPrefix(groupName, "kubeadm:") {
			continue
		}

		if !seen[groupName] {
			groups = append(groups, groupName)
			seen[groupName] = true
		}
	}

	return groups
}

// containsGroup checks if subjects contain a specific group name
func containsGroup(subjects []rbacv1.Subject, groupName string) bool {
	for _, subject := range subjects {
		if subject.Kind == "Group" && subject.Name == groupName {
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

// SetupWithManager sets up the controller with the Manager
func (r *RoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rbacv1.RoleBinding{}).
		Complete(r)
}
