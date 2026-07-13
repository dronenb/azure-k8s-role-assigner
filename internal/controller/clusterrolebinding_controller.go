package controller

import (
	"context"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
// bindings. The specific object in the request is not used directly; any event
// (create, update, delete) simply triggers a full-state reconcile.
func (r *ClusterRoleBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ClusterRoleBinding", "name", req.Name)

	// Confirm whether the object still exists purely for logging/clarity. Its
	// contents are not needed: reconcileDesiredState rebuilds desired state from
	// all live bindings, so a deleted binding's groups are naturally dropped.
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, req.NamespacedName, clusterRoleBinding); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("ClusterRoleBinding not found, likely deleted; running full-state reconcile", "name", req.Name)
		} else {
			logger.Error(err, "Failed to get ClusterRoleBinding")
			return reconcile.Result{}, err
		}
	}

	if err := r.reconcileDesiredState(ctx); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *ClusterRoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rbacv1.ClusterRoleBinding{}).
		Complete(r)
}
