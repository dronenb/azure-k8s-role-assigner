// Package controller contains Kubernetes reconcilers that keep Microsoft Entra
// group assignments in sync with RoleBinding, ClusterRoleBinding and Argo CD
// RBAC sources.
package controller

import (
	"context"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// fullStateRequest is the single work item every binding event maps to. The
// workqueue de-duplicates identical keys, so a burst of binding events collapses
// into one full-state pass, and only one resync timer exists per controller.
var fullStateRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: "full-state"}}

// mapToFullState maps any object event to fullStateRequest.
func mapToFullState(context.Context, client.Object) reconcile.Request {
	return fullStateRequest
}

// enqueueFullState enqueues fullStateRequest for every event.
var enqueueFullState = handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
	return []reconcile.Request{mapToFullState(ctx, obj)}
})

// initialFullState enqueues one full-state pass at startup so orphaned Azure
// assignments are cleaned up even if no binding references a managed group.
var initialFullState = source.Func(func(_ context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
	q.Add(fullStateRequest)
	return nil
})

// bindingGroupPredicate drops binding events that cannot change the desired
// group set: creates and deletes of bindings without managed group subjects,
// and updates that leave the managed group subjects unchanged.
var bindingGroupPredicate = predicate.Funcs{
	CreateFunc: func(e event.CreateEvent) bool { return hasManagedGroupSubjects(e.Object) },
	DeleteFunc: func(e event.DeleteEvent) bool { return hasManagedGroupSubjects(e.Object) },
	UpdateFunc: func(e event.UpdateEvent) bool {
		if !e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp()) {
			return hasManagedGroupSubjects(e.ObjectOld) || hasManagedGroupSubjects(e.ObjectNew)
		}
		return !sameStringSet(managedGroupSubjects(e.ObjectOld), managedGroupSubjects(e.ObjectNew))
	},
	GenericFunc: func(e event.GenericEvent) bool { return hasManagedGroupSubjects(e.Object) },
}

func hasManagedGroupSubjects(obj client.Object) bool {
	return len(managedGroupSubjects(obj)) > 0
}

// managedGroupSubjects returns the Group subjects of a binding that the
// controller would consider, without touching metrics.
func managedGroupSubjects(obj client.Object) []string {
	var subjects []rbacv1.Subject
	switch b := obj.(type) {
	case *rbacv1.RoleBinding:
		subjects = b.Subjects
	case *rbacv1.ClusterRoleBinding:
		subjects = b.Subjects
	default:
		return nil
	}
	groups := []string{}
	for _, s := range subjects {
		if s.Kind == "Group" && isValidAzureUUID(s.Name) {
			groups = append(groups, strings.ToLower(s.Name))
		}
	}
	return groups
}

func sameStringSet(a, b []string) bool {
	set := make(map[string]struct{}, len(a))
	for _, v := range a {
		set[v] = struct{}{}
	}
	other := make(map[string]struct{}, len(b))
	for _, v := range b {
		if _, ok := set[v]; !ok {
			return false
		}
		other[v] = struct{}{}
	}
	return len(set) == len(other)
}
