// Package controller contains Kubernetes RBAC reconcilers and shared reconcile state logic.
package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	appmetrics "github.com/dronenb/azure-k8s-role-assigner/internal/metrics"
	"github.com/microsoftgraph/msgraph-sdk-go/models"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DesiredGroupBuilder returns the desired set of Microsoft Entra group object IDs.
type DesiredGroupBuilder func(ctx context.Context) (map[string]struct{}, error)

// AzureGroupManager is the subset of the Azure client used by the reconcilers.
//
// It exists as an interface so the reconcile logic can be unit-tested with a
// fake, without making live Microsoft Graph API calls. The concrete
// *azure.Client satisfies this interface.
type AzureGroupManager interface {
	// GetGroupByID resolves an Azure group by its object ID.
	GetGroupByID(ctx context.Context, groupID string) (models.Groupable, error)
	// AssignGroupToServicePrincipal assigns a group to one service principal.
	AssignGroupToServicePrincipal(ctx context.Context, spID, groupID string) error
	// RemoveGroupFromServicePrincipal removes a group assignment from one service principal.
	RemoveGroupFromServicePrincipal(ctx context.Context, spID, groupID string) error
	// ListManagedAssignments returns, per configured service principal, the set
	// of group IDs assigned via this controller's app role.
	ListManagedAssignments(ctx context.Context) (map[string]map[string]struct{}, error)
}

// StateReconciler holds the shared state and logic used by both the
// RoleBinding and ClusterRoleBinding reconcilers.
//
// Both reconcilers converge the same desired state (the union of Azure groups
// referenced by all RoleBindings and ClusterRoleBindings) against the actual
// assignments in Azure. The reconcile is fully read-only with respect to
// binding resources: it never writes to RoleBinding or ClusterRoleBinding
// objects (no finalizers, annotations, or other metadata mutations).
//
// A single StateReconciler value is shared by both reconcilers so that the
// full-state convergence pass is serialized across them (controller-runtime
// only serializes reconciles within a single controller, not across
// controllers). This prevents one controller from assigning a group while the
// other is concurrently removing it.
type StateReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	AzureClient          AzureGroupManager
	BuildDesiredGroupIDs DesiredGroupBuilder

	// mu serializes full-state convergence so the RoleBinding and
	// ClusterRoleBinding reconcilers cannot interleave assign/remove operations.
	mu sync.Mutex
}

// reconcileDesiredState computes the desired set of Azure group assignments
// from all live RoleBindings and ClusterRoleBindings, then converges Azure to
// match: assigning any missing groups and removing any managed assignments that
// are no longer referenced by any binding.
//
// This function is the sole mechanism for both assignment and cleanup. Because
// it operates on the full cluster state rather than a single object, it is safe
// to run on every reconcile event and on the periodic resync, and it correctly
// handles deletions without requiring finalizers: once a binding is gone, its
// groups simply no longer appear in the desired set and are removed from Azure.
func (r *StateReconciler) reconcileDesiredState(ctx context.Context) error {
	logger := log.FromContext(ctx)
	started := time.Now()

	// Serialize convergence across the RoleBinding and ClusterRoleBinding
	// reconcilers, which share this StateReconciler.
	r.mu.Lock()
	defer r.mu.Unlock()

	buildDesired := r.BuildDesiredGroupIDs
	if buildDesired == nil {
		buildDesired = r.buildKubernetesDesiredGroupIDs
	}

	desired, err := buildDesired(ctx)
	if err != nil {
		return err
	}
	appmetrics.ReconcileGroupsDesired.Set(float64(len(desired)))

	// Read the actual state once, per service principal, then act only on the
	// difference. Already-correct assignments cost no further Graph calls.
	actual, err := r.AzureClient.ListManagedAssignments(ctx)
	if err != nil {
		return fmt.Errorf("failed to list managed group assignments from Azure: %w", err)
	}

	spIDs := make([]string, 0, len(actual))
	for spID := range actual {
		spIDs = append(spIDs, spID)
	}
	sort.Strings(spIDs)

	groupsToAssign := make(map[string]struct{})
	groupsToRemove := make(map[string]struct{})
	actualAfter := make(map[string]struct{})
	assignedCount := 0
	removedCount := 0
	for _, spID := range spIDs {
		assigned := actual[spID]

		for groupID := range desired {
			if _, ok := assigned[groupID]; ok {
				actualAfter[groupID] = struct{}{}
				logger.V(2).Info("Ignoring group because it is already assigned", "groupID", groupID, "servicePrincipalID", spID)
				continue
			}
			groupsToAssign[groupID] = struct{}{}
			if err := r.AzureClient.AssignGroupToServicePrincipal(ctx, spID, groupID); err != nil {
				// Continue with other groups; the next reconcile retries.
				logger.Error(err, "Failed to assign group to service principal", "groupID", groupID, "servicePrincipalID", spID)
				continue
			}
			actualAfter[groupID] = struct{}{}
			assignedCount++
			logger.Info("Assigned group to service principal", "groupID", groupID, "servicePrincipalID", spID)
		}

		for groupID := range assigned {
			if _, ok := desired[groupID]; ok {
				continue
			}
			groupsToRemove[groupID] = struct{}{}
			if err := r.AzureClient.RemoveGroupFromServicePrincipal(ctx, spID, groupID); err != nil {
				// Continue with other groups; the next reconcile retries.
				logger.Error(err, "Failed to remove group from service principal", "groupID", groupID, "servicePrincipalID", spID)
				actualAfter[groupID] = struct{}{}
				continue
			}
			removedCount++
			logger.Info("Removed group no longer referenced by any binding from service principal", "groupID", groupID, "servicePrincipalID", spID)
		}
	}

	logger.V(1).Info("Reconciled group assignments",
		"desired", len(desired),
		"actual", len(actualAfter),
		"assigned", assignedCount,
		"removed", removedCount,
		"duration", time.Since(started).String())

	appmetrics.ReconcileGroupsEnsured.Set(float64(len(groupsToAssign)))
	appmetrics.ReconcileGroupsToRemove.Set(float64(len(groupsToRemove)))
	appmetrics.ReconcileGroupsActual.Set(float64(len(actualAfter)))

	return nil
}

// buildDesiredGroupIDs lists all RoleBindings and ClusterRoleBindings and
// returns the set of resolved Azure group object IDs referenced by them.
//
// Bindings that are being deleted (non-zero deletion timestamp) are ignored so
// their groups do not keep an assignment alive during termination.
func (r *StateReconciler) buildKubernetesDesiredGroupIDs(ctx context.Context) (map[string]struct{}, error) {
	roleBindings := &rbacv1.RoleBindingList{}
	if err := r.List(ctx, roleBindings); err != nil {
		return nil, fmt.Errorf("failed to list rolebindings: %w", err)
	}

	clusterRoleBindings := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(ctx, clusterRoleBindings); err != nil {
		return nil, fmt.Errorf("failed to list clusterrolebindings: %w", err)
	}

	// Collect candidate group IDs (Azure UUIDs) from every live binding.
	logger := log.FromContext(ctx)
	candidates := make(map[string]struct{})
	for i := range roleBindings.Items {
		rb := &roleBindings.Items[i]
		if !rb.DeletionTimestamp.IsZero() {
			logger.V(2).Info("Ignoring binding that is being deleted", "kind", "RoleBinding", "namespace", rb.Namespace, "name", rb.Name)
			continue
		}
		for _, groupID := range extractGroupsFromSubjects(rb.Subjects, "rolebinding") {
			candidates[groupID] = struct{}{}
			logger.V(2).Info("Found group reference in binding", "kind", "RoleBinding", "namespace", rb.Namespace, "name", rb.Name, "groupID", groupID)
		}
	}
	for i := range clusterRoleBindings.Items {
		crb := &clusterRoleBindings.Items[i]
		if !crb.DeletionTimestamp.IsZero() {
			logger.V(2).Info("Ignoring binding that is being deleted", "kind", "ClusterRoleBinding", "name", crb.Name)
			continue
		}
		for _, groupID := range extractGroupsFromSubjects(crb.Subjects, "clusterrolebinding") {
			candidates[groupID] = struct{}{}
			logger.V(2).Info("Found group reference in binding", "kind", "ClusterRoleBinding", "name", crb.Name, "groupID", groupID)
		}
	}

	return r.resolveCandidateGroupIDs(ctx, candidates)
}

// resolveCandidateGroupIDs resolves each candidate against Azure to its canonical object ID.
// Groups that cannot be resolved are skipped rather than aborting the whole reconcile.
func (r *StateReconciler) resolveCandidateGroupIDs(ctx context.Context, candidates map[string]struct{}) (map[string]struct{}, error) {
	logger := log.FromContext(ctx)
	desired := make(map[string]struct{}, len(candidates))
	for groupID := range candidates {
		group, err := r.AzureClient.GetGroupByID(ctx, groupID)
		if err != nil {
			appmetrics.GroupLookupTotal.WithLabelValues(appmetrics.ClassifyAzureError(err)).Inc()
			logger.V(1).Error(err, "Failed to find group in Azure; skipping", "groupID", groupID)
			continue
		}
		resolvedGroupID := group.GetId()
		if resolvedGroupID == nil {
			appmetrics.GroupLookupTotal.WithLabelValues("error").Inc()
			logger.V(1).Error(fmt.Errorf("group ID is nil"), "Resolved group ID is nil; skipping", "groupID", groupID)
			continue
		}
		appmetrics.GroupLookupTotal.WithLabelValues("success").Inc()
		desired[*resolvedGroupID] = struct{}{}
	}

	return desired, nil
}

// isValidAzureUUID checks if a string is a valid Azure UUID (36-char format)
func isValidAzureUUID(s string) bool {
	uuidRegex := regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
	return uuidRegex.MatchString(strings.ToLower(s))
}

// extractGroupsFromSubjects extracts unique group IDs from subjects, filtering system groups and non-UUID groups.
func extractGroupsFromSubjects(subjects []rbacv1.Subject, source string) []string {
	groups := []string{}
	seen := make(map[string]bool)

	for _, subject := range subjects {
		if subject.Kind != "Group" {
			continue
		}
		appmetrics.GroupCandidatesTotal.WithLabelValues(source).Inc()

		groupID := subject.Name

		// Skip Kubernetes built-in groups
		if strings.HasPrefix(groupID, "system:") {
			appmetrics.InvalidGroupSubjectsTotal.WithLabelValues(source, "system_group").Inc()
			continue
		}
		if strings.HasPrefix(groupID, "kubeadm:") {
			appmetrics.InvalidGroupSubjectsTotal.WithLabelValues(source, "kubeadm_group").Inc()
			continue
		}

		// Only process valid Azure UUIDs
		if !isValidAzureUUID(groupID) {
			appmetrics.InvalidGroupSubjectsTotal.WithLabelValues(source, "non_uuid").Inc()
			continue
		}

		if !seen[groupID] {
			groups = append(groups, groupID)
			seen[groupID] = true
		}
	}

	return groups
}
