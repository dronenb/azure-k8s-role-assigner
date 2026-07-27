package controller

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"

	appmetrics "github.com/dronenb/azure-k8s-role-assigner/internal/metrics"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apierrutil "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var (
	appProjectGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "AppProject"}
	argoCDGVK     = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1beta1", Kind: "ArgoCD"}
)

// ArgoCDReconciler reconciles Argo CD RBAC groups into an Argo CD app registration.
type ArgoCDReconciler struct {
	*StateReconciler

	Name               string
	ResourceNamespace  string
	ResourceName       string
	ConfigMapName      string
	AppProjectsEnabled bool
	ResyncPeriod       time.Duration
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=argocds,verbs=get;list;watch
// +kubebuilder:rbac:groups=argoproj.io,resources=appprojects,verbs=get;list;watch

// Reconcile converges Azure group assignments against all configured Argo CD RBAC sources.
func (r *ArgoCDReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	start := time.Now()
	var reconcileErr error
	defer func() {
		appmetrics.ObserveReconcile("argocd", start, reconcileErr)
	}()

	logger := log.FromContext(ctx)
	logger.Info("Reconciling Argo CD RBAC", "namespace", req.Namespace, "name", req.Name)

	if err := r.reconcileDesiredState(ctx); err != nil {
		reconcileErr = err
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: r.ResyncPeriod}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArgoCDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cmPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.ResourceNamespace && obj.GetName() == r.ConfigMapName
	})
	appProjectPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.ResourceNamespace
	})
	argoCDPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.ResourceNamespace && obj.GetName() == r.ResourceName
	})

	b := ctrl.NewControllerManagedBy(mgr).
		Named(r.controllerName()).
		For(&corev1.ConfigMap{}, builder.WithPredicates(cmPredicate))

	if r.ResourceName != "" {
		argoCD := &unstructured.Unstructured{}
		argoCD.SetGroupVersionKind(argoCDGVK)
		b = b.Watches(argoCD, &handler.EnqueueRequestForObject{}, builder.WithPredicates(argoCDPredicate))
	}

	if r.AppProjectsEnabled {
		if _, err := mgr.GetRESTMapper().RESTMapping(appProjectGVK.GroupKind(), appProjectGVK.Version); err != nil {
			if !apierrutil.IsNoMatchError(err) {
				return fmt.Errorf("failed to discover AppProject resource: %w", err)
			}
			log.Log.Info("AppProject CRD not found; Argo CD AppProject reconciliation disabled")
		} else {
			appProject := &unstructured.Unstructured{}
			appProject.SetGroupVersionKind(appProjectGVK)
			b = b.Watches(appProject, &handler.EnqueueRequestForObject{}, builder.WithPredicates(appProjectPredicate))
		}
	}

	return b.Complete(r)
}

func (r *ArgoCDReconciler) controllerName() string {
	name := r.Name
	if name == "" {
		name = r.ResourceNamespace
	}
	return "argocd-" + strings.NewReplacer("_", "-", ".", "-", "/", "-").Replace(strings.ToLower(name))
}

// buildArgoCDDesiredGroupIDs returns the union of valid group IDs referenced by
// argocd-rbac-cm policy.csv and AppProject role groups.
func (r *ArgoCDReconciler) BuildArgoCDDesiredGroupIDs(ctx context.Context) (map[string]struct{}, error) {
	logger := log.FromContext(ctx)
	candidates := make(map[string]struct{})
	if r.ResourceName != "" {
		argoCD := &unstructured.Unstructured{}
		argoCD.SetGroupVersionKind(argoCDGVK)
		argoCDKey := types.NamespacedName{Namespace: r.ResourceNamespace, Name: r.ResourceName}
		if err := r.Get(ctx, argoCDKey, argoCD); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("ArgoCD resource not found; desired Argo CD RBAC groups are empty", "namespace", r.ResourceNamespace, "name", r.ResourceName)
				return r.resolveCandidateGroupIDs(ctx, candidates)
			}
			return nil, fmt.Errorf("failed to get ArgoCD resource %s: %w", argoCDKey.String(), err)
		}
		if !argoCD.GetDeletionTimestamp().IsZero() {
			logger.Info("ArgoCD resource is deleting; desired Argo CD RBAC groups are empty", "namespace", r.ResourceNamespace, "name", r.ResourceName)
			return r.resolveCandidateGroupIDs(ctx, candidates)
		}
	}

	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{Namespace: r.ResourceNamespace, Name: r.ConfigMapName}
	if err := r.Get(ctx, cmKey, cm); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("failed to get Argo CD RBAC ConfigMap %s: %w", cmKey.String(), err)
		}
		logger.Info("Argo CD RBAC ConfigMap not found; continuing with other sources", "namespace", r.ResourceNamespace, "name", r.ConfigMapName)
	} else if cm.DeletionTimestamp.IsZero() {
		for key, policy := range cm.Data {
			if !isArgoCDPolicyCSVKey(key) {
				continue
			}
			for _, groupID := range extractGroupsFromArgoCDPolicy(policy) {
				candidates[groupID] = struct{}{}
			}
		}
	}

	if r.AppProjectsEnabled {
		groups, err := r.buildAppProjectCandidateGroupIDs(ctx)
		if err != nil {
			return nil, err
		}
		for groupID := range groups {
			candidates[groupID] = struct{}{}
		}
	}

	return r.resolveCandidateGroupIDs(ctx, candidates)
}

func (r *ArgoCDReconciler) buildAppProjectCandidateGroupIDs(ctx context.Context) (map[string]struct{}, error) {
	appProjects := &unstructured.UnstructuredList{}
	appProjects.SetGroupVersionKind(schema.GroupVersionKind{Group: appProjectGVK.Group, Version: appProjectGVK.Version, Kind: "AppProjectList"})

	if err := r.List(ctx, appProjects, client.InNamespace(r.ResourceNamespace)); err != nil {
		if apierrutil.IsNoMatchError(err) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("failed to list AppProjects: %w", err)
	}

	groups := make(map[string]struct{})
	for i := range appProjects.Items {
		appProject := &appProjects.Items[i]
		if !appProject.GetDeletionTimestamp().IsZero() {
			continue
		}
		for _, groupID := range extractGroupsFromAppProject(appProject) {
			groups[groupID] = struct{}{}
		}
	}

	return groups, nil
}

func extractGroupsFromArgoCDPolicy(policy string) []string {
	groups := []string{}
	seen := make(map[string]bool)

	reader := csv.NewReader(strings.NewReader(policy))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) < 3 {
			continue
		}

		kind := strings.TrimSpace(record[0])
		if strings.HasPrefix(kind, "#") || kind != "g" {
			continue
		}

		groupID := strings.TrimSpace(record[1])
		appmetrics.GroupCandidatesTotal.WithLabelValues("argocd-rbac-cm").Inc()
		if !isValidAzureUUID(groupID) {
			appmetrics.InvalidGroupSubjectsTotal.WithLabelValues("argocd-rbac-cm", "non_uuid").Inc()
			continue
		}
		if !seen[groupID] {
			groups = append(groups, groupID)
			seen[groupID] = true
		}
	}

	return groups
}

func isArgoCDPolicyCSVKey(key string) bool {
	return key == "policy.csv" || (strings.HasPrefix(key, "policy.") && strings.HasSuffix(key, ".csv"))
}

func extractGroupsFromAppProject(appProject *unstructured.Unstructured) []string {
	roles, found, err := unstructured.NestedSlice(appProject.Object, "spec", "roles")
	if err != nil || !found {
		return []string{}
	}

	groups := []string{}
	seen := make(map[string]bool)
	for _, role := range roles {
		roleMap, ok := role.(map[string]interface{})
		if !ok {
			continue
		}
		roleGroups, found, err := unstructured.NestedStringSlice(roleMap, "groups")
		if err != nil || !found {
			continue
		}
		for _, groupID := range roleGroups {
			appmetrics.GroupCandidatesTotal.WithLabelValues("argocd-appproject").Inc()
			if !isValidAzureUUID(groupID) {
				appmetrics.InvalidGroupSubjectsTotal.WithLabelValues("argocd-appproject", "non_uuid").Inc()
				continue
			}
			if !seen[groupID] {
				groups = append(groups, groupID)
				seen[groupID] = true
			}
		}
	}

	return groups
}
