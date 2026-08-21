package e2e_test

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var appProjectGVK = schema.GroupVersionKind{
	Group:   "argoproj.io",
	Version: "v1alpha1",
	Kind:    "AppProject",
}

var _ = Describe("End-to-end Argo CD role assignment", Ordered, func() {
	var cfg e2eConfig

	BeforeAll(func(ctx SpecContext) {
		// ArgoCD install + RBAC is provisioned by the `task e2e` dependency chain
		// (e2e:argocd-up, e2e:create-argocd-rbac). This spec verifies the results
		// entirely through Go-native client calls.
		cfg = loadConfig()
	})

	It("reconciles the Argo CD RBAC sources into the Argo CD Entra token", func(ctx SpecContext) {
		// The ConfigMap-based policy grants role:admin to the ConfigMap group.
		Eventually(func(g Gomega) {
			cm := &corev1.ConfigMap{}
			err := cfg.kubeClient.Get(ctx, types.NamespacedName{Namespace: "argocd", Name: "argocd-rbac-cm"}, cm)
			g.Expect(err).NotTo(HaveOccurred(), "ConfigMap argocd-rbac-cm should exist in namespace argocd")
			policy, ok := cm.Data["policy.csv"]
			g.Expect(ok).To(BeTrue(), "argocd-rbac-cm should define policy.csv")
			g.Expect(policy).To(ContainSubstring(cfg.argocdConfigmapGroupID), "policy.csv should grant the ConfigMap group")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())

		// The AppProject source grants the AppProject group.
		Eventually(func(g Gomega) {
			appProject := &unstructured.Unstructured{}
			appProject.SetGroupVersionKind(appProjectGVK)
			err := cfg.kubeClient.Get(ctx, types.NamespacedName{Namespace: "argocd-shared", Name: "e2e-argocd-groups"}, appProject)
			g.Expect(err).NotTo(HaveOccurred(), "AppProject e2e-argocd-groups should exist in namespace argocd-shared")
			roles, found, err := unstructured.NestedSlice(appProject.Object, "spec", "roles")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue(), "AppProject should define roles")
			rendered, err := json.Marshal(roles)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(rendered)).To(ContainSubstring(cfg.argocdAppprojectGroupID), "AppProject should reference the AppProject group")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())

		// The ArgoCD ID token returns exactly the two shared-target groups.
		Eventually(func(g Gomega) {
			groups, err := ropcIDTokenGroups(ctx, cfg, cfg.argocdAppClientID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(groups).To(ConsistOf(cfg.argocdConfigmapGroupID, cfg.argocdAppprojectGroupID),
				"Argo CD ID token should contain exactly the ConfigMap and AppProject groups")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())
	})
})
