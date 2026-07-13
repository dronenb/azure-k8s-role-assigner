package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	groupA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	groupB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	groupC = "cccccccc-cccc-cccc-cccc-cccccccccccc"
)

// newStateReconciler wires a StateReconciler with a fake k8s client seeded with
// the given objects and the provided fake Azure manager.
func newStateReconciler(azm AzureGroupManager, objs ...client.Object) *StateReconciler {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &StateReconciler{
		Client:      c,
		Scheme:      scheme,
		AzureClient: azm,
	}
}

func TestReconcileDesiredState_AssignsReferencedGroups(t *testing.T) {
	azm := newFakeAzureManager()
	r := newStateReconciler(
		azm,
		clusterRoleBinding("crb-a", groupA),
		roleBinding("ns1", "rb-b", groupB),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.ElementsMatch(t, []string{groupA, groupB}, azm.assignCalls)
	assert.Empty(t, azm.removeCalls, "nothing should be removed when all assignments are desired")
	assert.Equal(t, map[string]struct{}{groupA: {}, groupB: {}}, azm.assignedSet())
}

// TestReconcileDesiredState_RemovesAssignmentWhenBindingAbsent is the unit-level
// analogue of the deletion e2e: a group is assigned in Azure but no binding
// references it, so it must be removed.
func TestReconcileDesiredState_RemovesAssignmentWhenBindingAbsent(t *testing.T) {
	// groupA is referenced by a binding; groupC is assigned in Azure but has no
	// binding (as if its binding was just deleted).
	azm := newFakeAzureManager(groupA, groupC)
	r := newStateReconciler(
		azm,
		clusterRoleBinding("crb-a", groupA),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupC}, azm.removeCalls, "the group with no backing binding must be removed")
	assert.Equal(t, map[string]struct{}{groupA: {}}, azm.assignedSet())
}

// TestReconcileDesiredState_TerminatingBindingIsIgnored verifies that a binding
// currently being deleted (non-zero deletion timestamp) does not keep its group
// assigned. This is what makes deletion converge without finalizers.
func TestReconcileDesiredState_TerminatingBindingIsIgnored(t *testing.T) {
	// A ClusterRoleBinding referencing groupA is present but terminating.
	// Fake-client objects require a finalizer to retain a deletion timestamp.
	terminating := clusterRoleBinding("crb-a", groupA)
	now := metaNow()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{"test.local/keep"}

	azm := newFakeAzureManager(groupA) // groupA currently assigned in Azure
	r := newStateReconciler(azm, terminating)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Empty(t, azm.assignCalls, "terminating binding must not drive assignment")
	assert.Equal(t, []string{groupA}, azm.removeCalls, "group backed only by a terminating binding must be removed")
	assert.Empty(t, azm.assignedSet())
}

func TestReconcileDesiredState_UnionAcrossBindings(t *testing.T) {
	// groupA is shared by two bindings; groupB only by the rolebinding.
	azm := newFakeAzureManager()
	r := newStateReconciler(
		azm,
		clusterRoleBinding("crb-a", groupA),
		roleBinding("ns1", "rb-a", groupA, groupB),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.ElementsMatch(t, []string{groupA, groupB}, azm.assignCalls)
	assert.Empty(t, azm.removeCalls)
}

// TestReconcileDesiredState_SharedGroupNotRemovedWhenOneBindingDeleted ensures a
// group referenced by multiple bindings is not de-assigned just because one of
// those bindings is gone.
func TestReconcileDesiredState_SharedGroupNotRemovedWhenOneBindingDeleted(t *testing.T) {
	// groupA already assigned; still referenced by the surviving rolebinding
	// (the clusterrolebinding that also referenced it is simply not present).
	azm := newFakeAzureManager(groupA)
	r := newStateReconciler(
		azm,
		roleBinding("ns1", "rb-a", groupA),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Empty(t, azm.removeCalls, "shared group must remain assigned while any binding references it")
	assert.Equal(t, map[string]struct{}{groupA: {}}, azm.assignedSet())
}

func TestReconcileDesiredState_IgnoresSystemAndNonUUIDGroups(t *testing.T) {
	azm := newFakeAzureManager()
	r := newStateReconciler(
		azm,
		clusterRoleBinding("crb-system", "system:masters", "kubeadm:cluster-admins", "platform-admins"),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Empty(t, azm.assignCalls)
	assert.Empty(t, azm.removeCalls)
}

func TestReconcileDesiredState_UnresolvableGroupIsSkippedNotAssigned(t *testing.T) {
	azm := newFakeAzureManager()
	azm.unknownGroups[groupB] = struct{}{} // groupB cannot be resolved in Azure

	r := newStateReconciler(
		azm,
		clusterRoleBinding("crb-a", groupA),
		roleBinding("ns1", "rb-b", groupB),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupA}, azm.assignCalls, "only resolvable groups are assigned")
}

func TestReconcileDesiredState_NilResolvedIDIsSkipped(t *testing.T) {
	azm := newFakeAzureManager()
	azm.nilIDGroups[groupB] = struct{}{} // resolves to a group with nil ID

	r := newStateReconciler(
		azm,
		roleBinding("ns1", "rb-a", groupA, groupB),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupA}, azm.assignCalls)
}

func TestReconcileDesiredState_AssignErrorDoesNotAbortOthers(t *testing.T) {
	azm := newFakeAzureManager()
	azm.assignErr[groupA] = errors.New("boom")

	r := newStateReconciler(
		azm,
		clusterRoleBinding("crb-a", groupA),
		roleBinding("ns1", "rb-b", groupB),
	)

	// A per-group assign failure is logged and skipped; reconcile still succeeds.
	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.ElementsMatch(t, []string{groupA, groupB}, azm.assignCalls)
	// groupB was assigned successfully despite groupA failing.
	assert.Contains(t, azm.assignedSet(), groupB)
}

func TestReconcileDesiredState_RemoveErrorDoesNotAbortOthers(t *testing.T) {
	// Two stale groups assigned in Azure, neither referenced by a binding.
	azm := newFakeAzureManager(groupB, groupC)
	azm.removeErr[groupB] = errors.New("boom")

	r := newStateReconciler(azm /* no bindings */)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.ElementsMatch(t, []string{groupB, groupC}, azm.removeCalls)
	// groupC removed despite groupB failing; groupB remains due to the error.
	remaining := azm.assignedSet()
	assert.Contains(t, remaining, groupB)
	assert.NotContains(t, remaining, groupC)
}

func TestReconcileDesiredState_ListErrorIsReturned(t *testing.T) {
	azm := newFakeAzureManager(groupA)
	azm.listErr = errors.New("graph unavailable")

	r := newStateReconciler(azm, clusterRoleBinding("crb-a", groupA))

	err := r.reconcileDesiredState(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list managed group assignments")
}

func TestBuildDesiredGroupIDs_ListRoleBindingsErrorPropagates(t *testing.T) {
	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*rbacv1.RoleBindingList); ok {
					return errors.New("list rolebindings failed")
				}
				return nil
			},
		}).
		Build()
	r := &StateReconciler{Client: c, Scheme: scheme, AzureClient: newFakeAzureManager()}

	err := r.reconcileDesiredState(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list rolebindings")
}
