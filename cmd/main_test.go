package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArgoCDTargetsGroupsInstancesWithSameAzureTarget(t *testing.T) {
	instances := []argoCDInstanceConfig{
		{
			Name:          "platform",
			Resource:      argoCDResourceConfig{Namespace: "argocd-platform", Name: "argocd"},
			RBACConfigMap: argoCDRBACConfigMapConfig{Name: "argocd-rbac-cm"},
			AppProjects:   argoCDAppProjectsConfig{Enabled: true},
			Azure:         argoCDAzureConfig{ServicePrincipals: "sp-b, sp-a", AppRoleID: "role-a"},
		},
		{
			Name:          "tenancy",
			Resource:      argoCDResourceConfig{Namespace: "argocd-tenancy", Name: "argocd"},
			RBACConfigMap: argoCDRBACConfigMapConfig{Name: "argocd-rbac-cm"},
			AppProjects:   argoCDAppProjectsConfig{Enabled: true},
			Azure:         argoCDAzureConfig{ServicePrincipals: "sp-a,sp-b", AppRoleID: "role-a"},
		},
		{
			Name:          "other",
			Resource:      argoCDResourceConfig{Namespace: "argocd-other", Name: "argocd"},
			RBACConfigMap: argoCDRBACConfigMapConfig{Name: "argocd-rbac-cm"},
			AppProjects:   argoCDAppProjectsConfig{Enabled: true},
			Azure:         argoCDAzureConfig{ServicePrincipals: "sp-c", AppRoleID: "role-a"},
		},
	}

	targets := argoCDTargets(instances)

	require.Len(t, targets, 2)
	assert.Equal(t, "platform", targets[0].Name)
	assert.Equal(t, "sp-a,sp-b", targets[0].ServicePrincipals)
	assert.Equal(t, "role-a", targets[0].AppRoleID)
	require.Len(t, targets[0].Sources, 2)
	assert.Equal(t, "argocd-platform", targets[0].Sources[0].ResourceNamespace)
	assert.Equal(t, "argocd-tenancy", targets[0].Sources[1].ResourceNamespace)

	assert.Equal(t, "other", targets[1].Name)
	assert.Equal(t, "sp-c", targets[1].ServicePrincipals)
	require.Len(t, targets[1].Sources, 1)
}

func TestArgoCDInstancesFromEnvAllowsInstanceWithoutArgoCDResourceName(t *testing.T) {
	t.Setenv("ARGOCD_INSTANCES", `[
		{
			"name": "source-only",
			"resource": {"namespace": "argocd-shared"},
			"azure": {"servicePrincipals": "sp-a", "appRoleId": "role-a"}
		}
	]`)

	instances, err := argoCDInstancesFromEnv()

	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, "argocd-shared", instances[0].Resource.Namespace)
	assert.Empty(t, instances[0].Resource.Name)
	assert.Equal(t, "argocd-rbac-cm", instances[0].RBACConfigMap.Name)
}

func TestParseServicePrincipalsNormalizesValues(t *testing.T) {
	assert.Equal(t, []string{"sp-a", "sp-b"}, parseServicePrincipals(" sp-b,sp-a,,sp-a "))
}
