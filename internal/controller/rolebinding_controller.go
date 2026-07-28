// Package controller contains Kubernetes reconcilers for Azure group assignments.
package controller

import (
	"context"
	"time"

	appmetrics "github.com/dronenb/azure-k8s-role-assigner/internal/metrics"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
// bindings. The specific object in the request is not used directly; any event
// (create, update, delete) simply triggers a full-state reconcile.
func (r *RoleBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	var reconcileErr error
	defer func() {
		appmetrics.ObserveReconcile("rolebinding", start, reconcileErr)
	}()

	logger := log.FromContext(ctx)
	logger.Info("Reconciling RoleBinding", "namespace", req.Namespace, "name", req.Name)

	// Confirm whether the object still exists purely for logging/clarity. Its
	// contents are not needed: reconcileDesiredState rebuilds desired state from
	// all live bindings, so a deleted binding's groups are naturally dropped.
	roleBinding := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, req.NamespacedName, roleBinding); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("RoleBinding not found, likely deleted; running full-state reconcile", "namespace", req.Namespace, "name", req.Name)
		} else {
			logger.Error(err, "Failed to get RoleBinding")
			reconcileErr = err
			return reconcile.Result{}, err
		}
	}

	if err := r.reconcileDesiredState(ctx); err != nil {
		reconcileErr = err
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *RoleBindingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rbacv1.RoleBinding{}).
		Complete(r)
}
