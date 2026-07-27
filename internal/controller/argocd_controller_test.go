package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	argocdNamespace = "argocd"
	argocdRBACName  = "argocd-rbac-cm"
)

func TestExtractGroupsFromArgoCDPolicy(t *testing.T) {
	policy := `# comments are ignored
p, role:admin, applications, *, */*, allow
g, aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa, role:admin
g, role:readonly, role:admin
g, user@example.com, role:admin
g, bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb, role:readonly
g, aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa, role:readonly
malformed
`

	got := extractGroupsFromArgoCDPolicy(policy)

	assert.Equal(t, []string{groupA, groupB}, got)
}

func TestIsArgoCDPolicyCSVKey(t *testing.T) {
	assert.True(t, isArgoCDPolicyCSVKey("policy.csv"))
	assert.True(t, isArgoCDPolicyCSVKey("policy.team-a.csv"))
	assert.False(t, isArgoCDPolicyCSVKey("policy.default"))
	assert.False(t, isArgoCDPolicyCSVKey("scopes"))
}

func TestExtractGroupsFromAppProject(t *testing.T) {
	appProject := appProject("project-a", []string{groupA, "role:admin", groupB}, []string{groupA})

	got := extractGroupsFromAppProject(appProject)

	assert.Equal(t, []string{groupA, groupB}, got)
}

func TestArgoCDReconcile_AssignsConfigMapAndAppProjectGroups(t *testing.T) {
	azm := newFakeAzureManager()
	r := newArgoCDReconciler(
		azm,
		argocdRBACConfigMap("g, "+groupA+", role:admin"),
		appProject("project-a", []string{groupB}),
	)

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.ElementsMatch(t, []string{groupA, groupB}, azm.assignCalls)
	assert.Equal(t, map[string]struct{}{groupA: {}, groupB: {}}, azm.assignedSet())
}

func TestArgoCDReconcile_RemovesStaleAssignments(t *testing.T) {
	azm := newFakeAzureManager(groupA, groupB)
	r := newArgoCDReconciler(azm, argocdRBACConfigMap("g, "+groupA+", role:admin"))

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupB}, azm.removeCalls)
	assert.Equal(t, map[string]struct{}{groupA: {}}, azm.assignedSet())
}

func TestArgoCDReconcile_MissingConfigMapStillUsesAppProjects(t *testing.T) {
	azm := newFakeAzureManager()
	r := newArgoCDReconciler(azm, appProject("project-a", []string{groupA}))

	require.NoError(t, r.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupA}, azm.assignCalls)
}

func TestArgoCDReconcile_UsesConfiguredArgoCDResourceNamespace(t *testing.T) {
	azm := newFakeAzureManager()
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		argocdResource("team-a", "argocd-a"),
		argocdRBACConfigMapInNamespace("team-a", "g, "+groupA+", role:admin"),
		appProjectInNamespace("team-a", "project-a", []string{groupB}),
		argocdResource("team-b", "argocd-b"),
		argocdRBACConfigMapInNamespace("team-b", "g, "+groupC+", role:admin"),
		appProjectInNamespace("team-b", "project-b", []string{groupC}),
	).Build()
	argocdReconciler := &ArgoCDReconciler{
		ResourceNamespace:  "team-a",
		ResourceName:       "argocd-a",
		ConfigMapName:      argocdRBACName,
		AppProjectsEnabled: true,
	}
	state := &StateReconciler{
		Client:               c,
		Scheme:               scheme,
		AzureClient:          azm,
		BuildDesiredGroupIDs: argocdReconciler.BuildArgoCDDesiredGroupIDs,
	}
	argocdReconciler.StateReconciler = state

	require.NoError(t, state.reconcileDesiredState(context.Background()))

	assert.ElementsMatch(t, []string{groupA, groupB}, azm.assignCalls)
	assert.Equal(t, map[string]struct{}{groupA: {}, groupB: {}}, azm.assignedSet())
}

func TestArgoCDReconcile_AppProjectSourceNamespacesDoNotChangeProjectNamespace(t *testing.T) {
	azm := newFakeAzureManager()
	scheme := testScheme()
	controlPlaneProject := appProjectInNamespace("argocd-control", "project-a", []string{groupA})
	require.NoError(t, unstructured.SetNestedStringSlice(controlPlaneProject.Object, []string{"team-*"}, "spec", "sourceNamespaces"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		argocdResource("argocd-control", "argocd"),
		controlPlaneProject,
		appProjectInNamespace("team-a", "project-a", []string{groupB}),
	).Build()
	argocdReconciler := &ArgoCDReconciler{
		ResourceNamespace:  "argocd-control",
		ResourceName:       "argocd",
		ConfigMapName:      argocdRBACName,
		AppProjectsEnabled: true,
	}
	state := &StateReconciler{
		Client:               c,
		Scheme:               scheme,
		AzureClient:          azm,
		BuildDesiredGroupIDs: argocdReconciler.BuildArgoCDDesiredGroupIDs,
	}
	argocdReconciler.StateReconciler = state

	require.NoError(t, state.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupA}, azm.assignCalls)
	assert.Equal(t, map[string]struct{}{groupA: {}}, azm.assignedSet())
}

func TestArgoCDReconcile_MissingArgoCDResourceRemovesAssignments(t *testing.T) {
	azm := newFakeAzureManager(groupA)
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		argocdRBACConfigMapInNamespace("team-a", "g, "+groupA+", role:admin"),
	).Build()
	argocdReconciler := &ArgoCDReconciler{
		ResourceNamespace:  "team-a",
		ResourceName:       "argocd-a",
		ConfigMapName:      argocdRBACName,
		AppProjectsEnabled: true,
	}
	state := &StateReconciler{
		Client:               c,
		Scheme:               scheme,
		AzureClient:          azm,
		BuildDesiredGroupIDs: argocdReconciler.BuildArgoCDDesiredGroupIDs,
	}
	argocdReconciler.StateReconciler = state

	require.NoError(t, state.reconcileDesiredState(context.Background()))

	assert.Equal(t, []string{groupA}, azm.removeCalls)
	assert.Empty(t, azm.assignedSet())
}

func TestKubernetesAndArgoCDTargetsAreIsolated(t *testing.T) {
	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(roleBinding("ns1", "rb-a", groupA), argocdRBACConfigMap("g, "+groupB+", role:admin")).
		Build()

	k8sAzure := newFakeAzureManager(groupA)
	argocdAzure := newFakeAzureManager(groupB)

	k8sState := &StateReconciler{Client: c, Scheme: scheme, AzureClient: k8sAzure}
	argocdReconciler := &ArgoCDReconciler{
		ResourceNamespace:  argocdNamespace,
		ConfigMapName:      argocdRBACName,
		AppProjectsEnabled: true,
	}
	argocdState := &StateReconciler{
		Client:               c,
		Scheme:               scheme,
		AzureClient:          argocdAzure,
		BuildDesiredGroupIDs: argocdReconciler.BuildArgoCDDesiredGroupIDs,
	}
	argocdReconciler.StateReconciler = argocdState

	require.NoError(t, k8sState.reconcileDesiredState(context.Background()))
	require.NoError(t, argocdState.reconcileDesiredState(context.Background()))

	assert.Empty(t, k8sAzure.removeCalls)
	assert.Empty(t, argocdAzure.removeCalls)
	assert.Equal(t, map[string]struct{}{groupA: {}}, k8sAzure.assignedSet())
	assert.Equal(t, map[string]struct{}{groupB: {}}, argocdAzure.assignedSet())
}

func newArgoCDReconciler(azm AzureGroupManager, objs ...client.Object) *StateReconciler {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	argocdReconciler := &ArgoCDReconciler{
		ResourceNamespace:  argocdNamespace,
		ConfigMapName:      argocdRBACName,
		AppProjectsEnabled: true,
	}
	state := &StateReconciler{
		Client:               c,
		Scheme:               scheme,
		AzureClient:          azm,
		BuildDesiredGroupIDs: argocdReconciler.BuildArgoCDDesiredGroupIDs,
	}
	argocdReconciler.StateReconciler = state
	return state
}

func argocdRBACConfigMap(policy string) *corev1.ConfigMap {
	return argocdRBACConfigMapInNamespace(argocdNamespace, policy)
}

func argocdRBACConfigMapInNamespace(namespace, policy string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: argocdRBACName},
		Data:       map[string]string{"policy.csv": policy},
	}
}

func appProject(name string, roleGroups ...[]string) *unstructured.Unstructured {
	return appProjectInNamespace(argocdNamespace, name, roleGroups...)
}

func appProjectInNamespace(namespace, name string, roleGroups ...[]string) *unstructured.Unstructured {
	roles := make([]interface{}, 0, len(roleGroups))
	for i, groups := range roleGroups {
		groupValues := make([]interface{}, 0, len(groups))
		for _, group := range groups {
			groupValues = append(groupValues, group)
		}
		roles = append(roles, map[string]interface{}{
			"name":   string(rune('a' + i)),
			"groups": groupValues,
		})
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"roles": roles,
		},
	}}
	obj.SetGroupVersionKind(appProjectGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}

func argocdResource(namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
	obj.SetGroupVersionKind(argoCDGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	return obj
}
