package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestBindingGroupPredicate_CreateDelete(t *testing.T) {
	systemOnly := clusterRoleBinding("system-only")
	systemOnly.Subjects = []rbacv1.Subject{
		{Kind: "Group", Name: "system:authenticated"},
		{Kind: "User", Name: groupA},
		{Kind: "ServiceAccount", Name: "default", Namespace: "ns1"},
	}

	assert.True(t, bindingGroupPredicate.Create(event.CreateEvent{Object: roleBinding("ns1", "rb", groupA)}))
	assert.True(t, bindingGroupPredicate.Delete(event.DeleteEvent{Object: clusterRoleBinding("crb", groupA)}))
	assert.False(t, bindingGroupPredicate.Create(event.CreateEvent{Object: systemOnly}))
	assert.False(t, bindingGroupPredicate.Delete(event.DeleteEvent{Object: systemOnly}))
	assert.False(t, bindingGroupPredicate.Create(event.CreateEvent{Object: roleBinding("ns1", "empty")}))
}

func TestBindingGroupPredicate_Update(t *testing.T) {
	oldRB := roleBinding("ns1", "rb", groupA, groupB)

	annotated := oldRB.DeepCopy()
	annotated.Annotations = map[string]string{"touched": "true"}
	annotated.ResourceVersion = "2"
	assert.False(t, bindingGroupPredicate.Update(event.UpdateEvent{ObjectOld: oldRB, ObjectNew: annotated}),
		"metadata-only updates must not trigger a reconcile")

	reordered := roleBinding("ns1", "rb", groupB, groupA)
	assert.False(t, bindingGroupPredicate.Update(event.UpdateEvent{ObjectOld: oldRB, ObjectNew: reordered}),
		"reordering subjects does not change the group set")

	changed := roleBinding("ns1", "rb", groupA, groupC)
	assert.True(t, bindingGroupPredicate.Update(event.UpdateEvent{ObjectOld: oldRB, ObjectNew: changed}))

	removed := roleBinding("ns1", "rb", groupA)
	assert.True(t, bindingGroupPredicate.Update(event.UpdateEvent{ObjectOld: oldRB, ObjectNew: removed}))

	deleting := oldRB.DeepCopy()
	now := metaNow()
	deleting.DeletionTimestamp = &now
	assert.True(t, bindingGroupPredicate.Update(event.UpdateEvent{ObjectOld: oldRB, ObjectNew: deleting}),
		"a binding starting deletion drops its groups from the desired set")

	systemOld := roleBinding("ns1", "sys")
	systemOld.Subjects = []rbacv1.Subject{{Kind: "Group", Name: "system:masters"}}
	systemNew := systemOld.DeepCopy()
	systemNew.DeletionTimestamp = &now
	assert.False(t, bindingGroupPredicate.Update(event.UpdateEvent{ObjectOld: systemOld, ObjectNew: systemNew}))
}

func TestEnqueueFullState_CoalescesToOneRequest(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, fullStateRequest, mapToFullState(ctx, roleBinding("ns1", "a", groupA)))
	assert.Equal(t, fullStateRequest, mapToFullState(ctx, clusterRoleBinding("b", groupB)))
}
