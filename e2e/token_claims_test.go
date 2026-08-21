package e2e_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("End-to-end role assignment", Ordered, func() {
	var cfg e2eConfig

	BeforeAll(func(ctx SpecContext) {
		// Infrastructure (kind, tofu, deploy, RBAC, ArgoCD) is provisioned by the
		// `task e2e` dependency chain, not by the Go suite. This spec only loads
		// configuration and validates the running cluster, following the
		// argocd-operator model of a Go-native verify step.
		cfg = loadConfig()
	})

	JustAfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(ctx, cfg)
		}
	})

	It("reconciles RoleBinding and ClusterRoleBinding with the cluster token", func(ctx SpecContext) {
		// The RoleBinding and ClusterRoleBinding created by `task e2e:create-rbac`
		// reference the static Entra test groups.
		Eventually(func(g Gomega) {
			crb := &rbacv1.ClusterRoleBinding{}
			err := cfg.kubeClient.Get(ctx, types.NamespacedName{Name: clusterRoleBindingName}, crb)
			g.Expect(err).NotTo(HaveOccurred(), "ClusterRoleBinding %s should exist", clusterRoleBindingName)
			g.Expect(subjectGroupIDs(crb.Subjects)).To(ContainElement(cfg.clusterRoleBindingGroupID))

			rb := &rbacv1.RoleBinding{}
			err = cfg.kubeClient.Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: roleBindingName}, rb)
			g.Expect(err).NotTo(HaveOccurred(), "RoleBinding %s should exist in namespace %s", roleBindingName, cfg.namespace)
			g.Expect(subjectGroupIDs(rb.Subjects)).To(ContainElement(cfg.roleBindingGroupID))
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())

		// The controller assigns the referenced groups to the cluster service
		// principal, so the test user's access token contains both group IDs.
		Eventually(func(g Gomega) {
			groups, err := ropcAccessTokenGroups(ctx, cfg, cfg.clusterAppClientID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(groups).To(ContainElement(cfg.clusterRoleBindingGroupID), "ClusterRoleBinding group should be present")
			g.Expect(groups).To(ContainElement(cfg.roleBindingGroupID), "RoleBinding group should be present")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())
	})

	It("removes the RoleBinding group after the binding is deleted", func(ctx SpecContext) {
		rb := &rbacv1.RoleBinding{}
		err := cfg.kubeClient.Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: roleBindingName}, rb)
		if err == nil {
			Expect(cfg.kubeClient.Delete(ctx, rb)).To(Succeed(), "delete RoleBinding %s in namespace %s", roleBindingName, cfg.namespace)
			// Wait for actual deletion so the controller observes removal.
			Eventually(func() bool {
				err := cfg.kubeClient.Get(ctx, types.NamespacedName{Namespace: cfg.namespace, Name: roleBindingName}, &rbacv1.RoleBinding{})
				return apierrors.IsNotFound(err)
			}, cfg.timeout(), cfg.pollInterval()).Should(BeTrue(), "RoleBinding %s should be deleted", roleBindingName)
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "RoleBinding %s should already be absent", roleBindingName)
		}

		// The RoleBinding group is removed from the token while the
		// ClusterRoleBinding group remains.
		Eventually(func(g Gomega) {
			groups, err := ropcAccessTokenGroups(ctx, cfg, cfg.clusterAppClientID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(groups).NotTo(ContainElement(cfg.roleBindingGroupID), "deleted RoleBinding group should be absent")
			g.Expect(groups).To(ContainElement(cfg.clusterRoleBindingGroupID), "ClusterRoleBinding group should remain present")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())
	})
})

// subjectGroupIDs returns the object IDs of subjects of kind "Group".
func subjectGroupIDs(subjects []rbacv1.Subject) []string {
	var ids []string
	for _, s := range subjects {
		if s.Kind == rbacv1.GroupKind {
			ids = append(ids, s.Name)
		}
	}
	return ids
}
