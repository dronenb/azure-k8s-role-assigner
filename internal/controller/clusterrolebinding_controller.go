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

// ClusterRoleBindingReconciler reconciles a ClusterRoleBinding object.
//
// It embeds the shared StateReconciler and delegates all work to the full-state
// convergence pass. It never writes to ClusterRoleBinding objects.
type ClusterRoleBindingReconciler struct {
	*StateReconciler

	// ResyncPeriod is how long to wait before requeuing a full-state reconcile
	// when no binding events occur. This ensures deletions converge even in the
	// absence of watch events.
	ResyncPeriod time.Duration
}

// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch

// Reconcile converges Azure group assignments against the full set of live
// bindings. Every relevant ClusterRoleBinding event maps to the same request, so the
// request itself carries no information.
func (r *ClusterRoleBindingReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	var reconcileErr error
	defer func() {
		appmetrics.ObserveReconcile("clusterrolebinding", start, reconcileErr)
	}()

	if err := r.reconcileDesiredState(ctx); err != nil {
		reconcileErr = err
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterRoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("clusterrolebinding").
		Watches(&rbacv1.ClusterRoleBinding{}, enqueueFullState, builder.WithPredicates(bindingGroupPredicate)).
		WatchesRawSource(initialFullState).
		Complete(r)
}
