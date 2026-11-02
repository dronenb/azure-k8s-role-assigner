package controller

import (
	"context"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// captureLogs returns a context whose logger writes to an in-memory observer,
// plus the observed logs. allowAll enables every level, including V(1)/V(2).
// The logger goes through the context because ctrl.SetLogger only takes effect
// once per process.
func captureLogs(t *testing.T, allowAll bool) (context.Context, *observer.ObservedLogs) {
	t.Helper()

	var enabler zapcore.LevelEnabler = zapcore.InfoLevel
	if allowAll {
		enabler = zap.LevelEnablerFunc(func(zapcore.Level) bool { return true })
	}
	core, logs := observer.New(enabler)
	ctx := log.IntoContext(context.Background(), zapr.NewLogger(zap.New(core)))
	return ctx, logs
}

func logMessages(logs *observer.ObservedLogs) string {
	out := ""
	for _, entry := range logs.AllUntimed() {
		out += entry.Message + "\n"
	}
	return out
}

func TestReconcileDesiredState_InSyncPassLogsNothingAtInfo(t *testing.T) {
	ctx, logs := captureLogs(t, false)
	azm := newFakeAzureManager(groupA)
	r := newStateReconciler(azm, clusterRoleBinding("crb-a", groupA))

	require.NoError(t, r.reconcileDesiredState(ctx))

	assert.Empty(t, logMessages(logs), "an in-sync pass must not log at Info")
}

func TestReconcileDesiredState_LogsDeltaAtInfo(t *testing.T) {
	ctx, logs := captureLogs(t, false)
	azm := newFakeAzureManager(groupB)
	r := newStateReconciler(azm, clusterRoleBinding("crb-a", groupA))

	require.NoError(t, r.reconcileDesiredState(ctx))

	messages := logMessages(logs)
	assert.Contains(t, messages, "Assigned group to service principal")
	assert.Contains(t, messages, "Removed group no longer referenced by any binding from service principal")
	assert.NotContains(t, messages, "Found group reference in binding", "per-binding detail belongs to V(2)")
	assert.NotContains(t, messages, "Reconciled group assignments", "the per-pass summary is V(1)")
}

func TestReconcileDesiredState_VerboseLogsBindingDetail(t *testing.T) {
	ctx, logs := captureLogs(t, true)
	azm := newFakeAzureManager(groupA)
	r := newStateReconciler(azm, clusterRoleBinding("crb-a", groupA))

	require.NoError(t, r.reconcileDesiredState(ctx))

	messages := logMessages(logs)
	assert.Contains(t, messages, "Found group reference in binding")
	assert.Contains(t, messages, "Ignoring group because it is already assigned")
	assert.Contains(t, messages, "Reconciled group assignments", "the per-pass summary is V(1)")
}
