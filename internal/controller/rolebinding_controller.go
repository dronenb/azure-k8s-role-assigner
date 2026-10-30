// Package controller contains Kubernetes reconcilers for Azure group assignments.
package controller

import (
	"context"
	"time"

	appmetrics "github.com/dronenb/azure-k8s-role-assigner/internal/metrics"
	rbacv1 "k8s.io/api/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// RoleBindingReconciler reconciles a RoleBinding object.
//
// It embeds the shared StateReconciler and delegates all work to the full-state
// convergence pass. It never writes to RoleBinding objects.
type RoleBindingReconciler struct {
	*StateReconciler

	// ResyncPeriod is how long to wait before requeuing a full-state reconcile
	// when no binding events occur. This ensures deletions converge even in the
	// absence of watch events.
	ResyncPeriod time.Duration
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch

// Reconcile converges Azure group assignments against the full set of live
// bindings. Every relevant RoleBinding event maps to the same request, so the
// request itself carries no information.
func (r *RoleBindingReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	var reconcileErr error
	defer func() {
		appmetrics.ObserveReconcile("rolebinding", start, reconcileErr)
	}()

	if err := r.reconcileDesiredState(ctx); err != nil {
		reconcileErr = err
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("rolebinding").
		Watches(&rbacv1.RoleBinding{}, enqueueFullState, builder.WithPredicates(bindingGroupPredicate)).
		WatchesRawSource(initialFullState).
		Complete(r)
}
