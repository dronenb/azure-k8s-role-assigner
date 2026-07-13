package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const testResyncPeriod = 7 * time.Minute

func newReconcilers(azm AzureGroupManager, objs ...client.Object) (*RoleBindingReconciler, *ClusterRoleBindingReconciler, client.Client) {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	state := &StateReconciler{Client: c, Scheme: scheme, AzureClient: azm}
	rb := &RoleBindingReconciler{StateReconciler: state, ResyncPeriod: testResyncPeriod}
	crb := &ClusterRoleBindingReconciler{StateReconciler: state, ResyncPeriod: testResyncPeriod}
	return rb, crb, c
}

func TestRoleBindingReconcile_RequeuesAfterResyncPeriod(t *testing.T) {
	azm := newFakeAzureManager()
	rb, _, _ := newReconcilers(azm, roleBinding("ns1", "rb-a", groupA))

	res, err := rb.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "rb-a"},
	})
	require.NoError(t, err)
	assert.Equal(t, testResyncPeriod, res.RequeueAfter, "should requeue after the configured resync period")
	assert.Contains(t, azm.assignedSet(), groupA)
}

func TestClusterRoleBindingReconcile_RequeuesAfterResyncPeriod(t *testing.T) {
	azm := newFakeAzureManager()
	_, crb, _ := newReconcilers(azm, clusterRoleBinding("crb-a", groupA))

	res, err := crb.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "crb-a"},
	})
	require.NoError(t, err)
	assert.Equal(t, testResyncPeriod, res.RequeueAfter)
	assert.Contains(t, azm.assignedSet(), groupA)
}

// TestRoleBindingReconcile_DeletedObjectStillConverges verifies that a reconcile
// request for a RoleBinding that no longer exists (NotFound) still runs a full
// convergence pass and removes the now-orphaned Azure assignment. This is the
// core deletion behavior that replaced finalizers.
func TestRoleBindingReconcile_DeletedObjectStillConverges(t *testing.T) {
	// groupA is assigned in Azure, but the referencing binding does not exist
	// in the cluster (already deleted).
	azm := newFakeAzureManager(groupA)
	rb, _, _ := newReconcilers(azm /* no objects */)

	res, err := rb.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "rb-a"},
	})
	require.NoError(t, err)
	assert.Equal(t, testResyncPeriod, res.RequeueAfter)
	assert.Equal(t, []string{groupA}, azm.removeCalls, "orphaned group must be removed on delete-triggered reconcile")
	assert.Empty(t, azm.assignedSet())
}

// TestReconcile_DeletionEndToEnd exercises the full lifecycle through the
// reconciler: a binding is created (group gets assigned), then deleted, and a
// subsequent reconcile removes the group.
func TestReconcile_DeletionEndToEnd(t *testing.T) {
	azm := newFakeAzureManager()
	crbObj := clusterRoleBinding("crb-a", groupA)
	_, crb, c := newReconcilers(azm, crbObj)
	ctx := context.Background()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "crb-a"}}

	// 1. Initial reconcile assigns the group.
	_, err := crb.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Contains(t, azm.assignedSet(), groupA)

	// 2. Delete the binding from the cluster.
	require.NoError(t, c.Delete(ctx, crbObj))

	// 3. Reconcile after deletion removes the group from Azure.
	_, err = crb.Reconcile(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []string{groupA}, azm.removeCalls)
	assert.NotContains(t, azm.assignedSet(), groupA)
}

// TestReconcilers_ShareState verifies the two reconcilers operate on the same
// StateReconciler, so both see the union of all bindings.
func TestReconcilers_ShareState(t *testing.T) {
	azm := newFakeAzureManager()
	rb, crb, _ := newReconcilers(
		azm,
		roleBinding("ns1", "rb-a", groupA),
		clusterRoleBinding("crb-b", groupB),
	)
	ctx := context.Background()

	// Reconciling either controller converges the full desired state.
	_, err := rb.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "rb-a"}})
	require.NoError(t, err)

	assert.Same(t, rb.StateReconciler, crb.StateReconciler, "both reconcilers must share one StateReconciler")
	assert.ElementsMatch(t, []string{groupA, groupB}, azm.assignCalls)
}
