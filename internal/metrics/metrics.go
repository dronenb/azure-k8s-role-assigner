package metrics

import (
	"errors"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const namespace = "azure_k8s_role_assigner"

var (
	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_total",
			Help:      "Total number of reconciliation attempts.",
		},
		[]string{"controller", "result"},
	)

	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "reconcile_duration_seconds",
			Help:      "Duration of reconciliation attempts.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"controller"},
	)

	LastSuccessfulReconcileTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "last_successful_reconcile_timestamp_seconds",
			Help:      "Unix timestamp of the last successful reconciliation.",
		},
		[]string{"controller"},
	)

	ReconcileGroupsDesired = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "reconcile_groups_desired",
			Help:      "Number of valid desired Microsoft Entra group assignments from live RBAC bindings.",
		},
	)

	ReconcileGroupsActual = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "reconcile_groups_actual",
			Help:      "Number of managed Microsoft Entra group assignments observed after reconciliation.",
		},
	)

	ReconcileGroupsEnsured = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "reconcile_groups_ensured",
			Help:      "Number of desired groups the controller attempted to ensure during the last reconciliation.",
		},
	)

	ReconcileGroupsToRemove = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "reconcile_groups_to_remove",
			Help:      "Number of managed Microsoft Entra group assignments found stale during the last reconciliation.",
		},
	)

	GroupCandidatesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "group_candidates_total",
			Help:      "Total number of Kubernetes RBAC Group subjects seen during reconciliation.",
		},
		[]string{"source"},
	)

	InvalidGroupSubjectsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "invalid_group_subjects_total",
			Help:      "Total number of ignored Kubernetes RBAC Group subjects.",
		},
		[]string{"source", "reason"},
	)

	GroupLookupTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "group_lookup_total",
			Help:      "Total number of Microsoft Entra group lookup attempts.",
		},
		[]string{"result"},
	)

	AssignmentOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "assignment_operations_total",
			Help:      "Total number of Microsoft Entra assignment operations.",
		},
		[]string{"operation", "result"},
	)

	AzureRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "azure_requests_total",
			Help:      "Total number of Microsoft Graph requests by operation and result.",
		},
		[]string{"operation", "result"},
	)

	AzureRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "azure_request_duration_seconds",
			Help:      "Duration of Microsoft Graph requests.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	AuthFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "auth_failures_total",
			Help:      "Total number of authentication or authorization failures.",
		},
		[]string{"phase"},
	)

	GroupCacheEntries = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "group_cache_entries",
			Help:      "Number of Microsoft Entra groups currently cached by object ID.",
		},
	)

	GroupCacheRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "group_cache_requests_total",
			Help:      "Total number of group cache requests.",
		},
		[]string{"result"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal,
		ReconcileDuration,
		LastSuccessfulReconcileTimestamp,
		ReconcileGroupsDesired,
		ReconcileGroupsActual,
		ReconcileGroupsEnsured,
		ReconcileGroupsToRemove,
		GroupCandidatesTotal,
		InvalidGroupSubjectsTotal,
		GroupLookupTotal,
		AssignmentOperationsTotal,
		AzureRequestsTotal,
		AzureRequestDuration,
		AuthFailuresTotal,
		GroupCacheEntries,
		GroupCacheRequestsTotal,
	)
}

func ObserveReconcile(controller string, start time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	} else {
		LastSuccessfulReconcileTimestamp.WithLabelValues(controller).Set(float64(time.Now().Unix()))
	}

	ReconcileTotal.WithLabelValues(controller, result).Inc()
	ReconcileDuration.WithLabelValues(controller).Observe(time.Since(start).Seconds())
}

func ObserveAzureRequest(operation string, start time.Time, err error) string {
	result := ClassifyAzureError(err)
	AzureRequestsTotal.WithLabelValues(operation, result).Inc()
	AzureRequestDuration.WithLabelValues(operation).Observe(time.Since(start).Seconds())
	if result == "auth_error" || result == "permission_error" {
		AuthFailuresTotal.WithLabelValues("graph_request").Inc()
	}
	return result
}

func ClassifyAzureError(err error) string {
	if err == nil {
		return "success"
	}

	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) {
		switch responseErr.StatusCode {
		case 401:
			return "auth_error"
		case 403:
			return "permission_error"
		case 404:
			return "not_found"
		case 409:
			return "conflict"
		case 429:
			return "rate_limited"
		default:
			return "error"
		}
	}

	errMsg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(errMsg, "unauthorized") || strings.Contains(errMsg, "authentication"):
		return "auth_error"
	case strings.Contains(errMsg, "forbidden") || strings.Contains(errMsg, "authorization") || strings.Contains(errMsg, "permission"):
		return "permission_error"
	case strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "does not exist"):
		return "not_found"
	case strings.Contains(errMsg, "too many requests") || strings.Contains(errMsg, "rate limit") || strings.Contains(errMsg, "throttl"):
		return "rate_limited"
	case strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "conflict"):
		return "conflict"
	default:
		return "error"
	}
}
