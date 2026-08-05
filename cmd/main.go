// Package main starts the azure-k8s-role-assigner controller manager.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/dronenb/azure-k8s-role-assigner/internal/azure"
	"github.com/dronenb/azure-k8s-role-assigner/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var resyncPeriod time.Duration
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.DurationVar(&resyncPeriod, "resync-period", 10*time.Minute,
		"Interval at which a full-state reconcile is requeued when no binding events occur. "+
			"This ensures Azure assignments for deleted bindings are eventually removed even without watch events.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "azure-k8s-role-assigner.dronenb.github.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Initialize Azure client
	ctx := ctrl.SetupSignalHandler()
	azureClient, err := azure.NewClient(ctx)
	if err != nil {
		setupLog.Error(err, "unable to create Azure client")
		os.Exit(1)
	}
	setupLog.Info("Azure client initialized successfully")

	// Shared state reconciler used by both binding controllers so that
	// full-state convergence is serialized across them.
	stateReconciler := &controller.StateReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		AzureClient: azureClient,
	}

	// Setup RoleBinding controller
	if err = (&controller.RoleBindingReconciler{
		StateReconciler: stateReconciler,
		ResyncPeriod:    resyncPeriod,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RoleBinding")
		os.Exit(1)
	}

	// Setup ClusterRoleBinding controller
	if err = (&controller.ClusterRoleBindingReconciler{
		StateReconciler: stateReconciler,
		ResyncPeriod:    resyncPeriod,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ClusterRoleBinding")
		os.Exit(1)
	}

	if strings.EqualFold(os.Getenv("ARGOCD_ENABLED"), "true") {
		argocdInstances, err := argoCDInstancesFromEnv()
		if err != nil {
			setupLog.Error(err, "unable to parse Argo CD configuration")
			os.Exit(1)
		}

		for _, target := range argoCDTargets(argocdInstances) {
			argocdAzureClient, err := newArgoCDAzureClient(ctx, target.ServicePrincipals, target.AppRoleID)
			if err != nil {
				setupLog.Error(err, "unable to create Argo CD Azure client", "target", target.Name)
				os.Exit(1)
			}

			argocdReconciler := &controller.ArgoCDReconciler{
				Name:         target.Name,
				Sources:      target.Sources,
				ResyncPeriod: resyncPeriod,
			}
			argocdStateReconciler := &controller.StateReconciler{
				Client:               mgr.GetClient(),
				Scheme:               mgr.GetScheme(),
				AzureClient:          argocdAzureClient,
				BuildDesiredGroupIDs: argocdReconciler.BuildArgoCDDesiredGroupIDs,
			}
			argocdReconciler.StateReconciler = argocdStateReconciler

			if err = argocdReconciler.SetupWithManager(mgr); err != nil {
				setupLog.Error(err, "unable to create controller", "controller", "ArgoCD", "target", target.Name)
				os.Exit(1)
			}
			setupLog.Info("Argo CD reconciliation enabled", "target", target.Name, "sources", len(target.Sources))
		}
	}

	// Add health checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type argoCDInstanceConfig struct {
	Name           string                    `json:"name"`
	Resource       argoCDResourceConfig      `json:"resource"`
	RBACConfigMap  argoCDRBACConfigMapConfig `json:"rbacConfigMap"`
	AppProjects    argoCDAppProjectsConfig   `json:"appProjects"`
	AppProjectsSet bool                      `json:"-"`
	Azure          argoCDAzureConfig         `json:"azure"`
}

type argoCDResourceConfig struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type argoCDRBACConfigMapConfig struct {
	Name string `json:"name"`
}

type argoCDAppProjectsConfig struct {
	Enabled bool `json:"enabled"`
}

type argoCDAzureConfig struct {
	ServicePrincipals string `json:"servicePrincipals"`
	AppRoleID         string `json:"appRoleId"`
}

type argoCDTargetConfig struct {
	Name              string
	ServicePrincipals string
	AppRoleID         string
	Sources           []controller.ArgoCDSource
}

func argoCDInstancesFromEnv() ([]argoCDInstanceConfig, error) {
	instancesJSON := strings.TrimSpace(os.Getenv("ARGOCD_INSTANCES"))
	if instancesJSON == "" {
		return []argoCDInstanceConfig{{
			Name: "default",
			Resource: argoCDResourceConfig{
				Namespace: envOrDefault("ARGOCD_RESOURCE_NAMESPACE", envOrDefault("ARGOCD_RBAC_CONFIGMAP_NAMESPACE", "argocd")),
				Name:      os.Getenv("ARGOCD_RESOURCE_NAME"),
			},
			RBACConfigMap: argoCDRBACConfigMapConfig{
				Name: envOrDefault("ARGOCD_RBAC_CONFIGMAP_NAME", "argocd-rbac-cm"),
			},
			AppProjects: argoCDAppProjectsConfig{Enabled: !strings.EqualFold(os.Getenv("ARGOCD_APPPROJECTS_ENABLED"), "false")},
			Azure: argoCDAzureConfig{
				ServicePrincipals: os.Getenv("ARGOCD_AZURE_SERVICE_PRINCIPALS"),
				AppRoleID:         os.Getenv("ARGOCD_AZURE_APP_ROLE_ID"),
			},
		}}, nil
	}

	var instances []argoCDInstanceConfig
	if err := json.Unmarshal([]byte(instancesJSON), &instances); err != nil {
		return nil, fmt.Errorf("failed to parse ARGOCD_INSTANCES JSON: %w", err)
	}
	if len(instances) == 0 {
		return nil, fmt.Errorf("ARGOCD_INSTANCES must contain at least one instance")
	}

	seenNames := map[string]struct{}{}
	for i := range instances {
		instance := &instances[i]
		if instance.Name == "" {
			instance.Name = fmt.Sprintf("instance-%d", i)
		}
		if _, exists := seenNames[instance.Name]; exists {
			return nil, fmt.Errorf("duplicate Argo CD instance name %q", instance.Name)
		}
		seenNames[instance.Name] = struct{}{}

		if instance.Resource.Namespace == "" {
			return nil, fmt.Errorf("Argo CD instance %q resource.namespace must be set", instance.Name)
		}
		if instance.RBACConfigMap.Name == "" {
			instance.RBACConfigMap.Name = "argocd-rbac-cm"
		}
		if !instance.AppProjectsSet {
			instance.AppProjects.Enabled = true
		}
	}

	return instances, nil
}

func (c *argoCDInstanceConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name          string                    `json:"name"`
		Resource      argoCDResourceConfig      `json:"resource"`
		RBACConfigMap argoCDRBACConfigMapConfig `json:"rbacConfigMap"`
		AppProjects   *argoCDAppProjectsConfig  `json:"appProjects"`
		Azure         argoCDAzureConfig         `json:"azure"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.Name = raw.Name
	c.Resource = raw.Resource
	c.RBACConfigMap = raw.RBACConfigMap
	c.Azure = raw.Azure
	if raw.AppProjects != nil {
		c.AppProjects = *raw.AppProjects
		c.AppProjectsSet = true
	} else {
		c.AppProjects = argoCDAppProjectsConfig{}
		c.AppProjectsSet = false
	}
	return nil
}

func newArgoCDAzureClient(ctx context.Context, servicePrincipalsEnv, appRoleID string) (*azure.Client, error) {
	servicePrincipals := parseServicePrincipals(servicePrincipalsEnv)
	if len(servicePrincipals) == 0 {
		return nil, fmt.Errorf("Argo CD servicePrincipals is required")
	}

	appRoleID = strings.TrimSpace(appRoleID)
	if appRoleID == "" {
		return nil, fmt.Errorf("Argo CD appRoleId is required")
	}

	return azure.NewClientForTarget(ctx, servicePrincipals, appRoleID)
}

func argoCDTargets(instances []argoCDInstanceConfig) []argoCDTargetConfig {
	targetsByKey := map[string]*argoCDTargetConfig{}
	keys := []string{}
	for _, instance := range instances {
		servicePrincipals := strings.Join(parseServicePrincipals(instance.Azure.ServicePrincipals), ",")
		appRoleID := strings.TrimSpace(instance.Azure.AppRoleID)
		key := servicePrincipals + "|" + appRoleID
		target, exists := targetsByKey[key]
		if !exists {
			target = &argoCDTargetConfig{
				Name:              instance.Name,
				ServicePrincipals: servicePrincipals,
				AppRoleID:         appRoleID,
			}
			targetsByKey[key] = target
			keys = append(keys, key)
		}
		target.Sources = append(target.Sources, controller.ArgoCDSource{
			ResourceNamespace:  instance.Resource.Namespace,
			ResourceName:       instance.Resource.Name,
			ConfigMapName:      instance.RBACConfigMap.Name,
			AppProjectsEnabled: instance.AppProjects.Enabled,
		})
	}

	targets := make([]argoCDTargetConfig, 0, len(keys))
	for _, key := range keys {
		targets = append(targets, *targetsByKey[key])
	}
	return targets
}

func parseServicePrincipals(value string) []string {
	seen := map[string]struct{}{}
	servicePrincipals := []string{}
	for _, servicePrincipal := range strings.Split(value, ",") {
		servicePrincipal = strings.TrimSpace(servicePrincipal)
		if servicePrincipal == "" {
			continue
		}
		if _, exists := seen[servicePrincipal]; exists {
			continue
		}
		seen[servicePrincipal] = struct{}{}
		servicePrincipals = append(servicePrincipals, servicePrincipal)
	}
	sort.Strings(servicePrincipals)
	return servicePrincipals
}

func envOrDefault(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}
