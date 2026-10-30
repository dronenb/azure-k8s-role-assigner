//revive:disable:dot-imports
package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const argoCDCRDName = "argocds.argoproj.io"

var (
	argoCDGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1beta1", Kind: "ArgoCD"}
	crdGVK    = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
)

// This spec is destructive: it removes the ArgoCD CRD from the cluster. It is
// labeled so `task e2e` runs it last, after every other spec has passed.
var _ = Describe("Argo CD CRD missing", Ordered, Label("destructive"), func() {
	var cfg e2eConfig

	BeforeAll(func(_ SpecContext) {
		cfg = loadConfig()
	})

	JustAfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() {
			dumpDiagnostics(ctx, cfg)
		}
	})

	It("removes the ArgoCD resource and its CRD", func(ctx SpecContext) {
		argoCD := &unstructured.Unstructured{}
		argoCD.SetGroupVersionKind(argoCDGVK)
		argoCD.SetNamespace("argocd")
		argoCD.SetName("e2e-argocd")
		Expect(client.IgnoreNotFound(cfg.kubeClient.Delete(ctx, argoCD))).To(Succeed())

		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(crdGVK)
		crd.SetName(argoCDCRDName)
		Expect(client.IgnoreNotFound(cfg.kubeClient.Delete(ctx, crd))).To(Succeed())

		Eventually(func() bool {
			check := &unstructured.Unstructured{}
			check.SetGroupVersionKind(crdGVK)
			return apierrors.IsNotFound(cfg.kubeClient.Get(ctx, types.NamespacedName{Name: argoCDCRDName}, check))
		}, cfg.timeout(), cfg.pollInterval()).Should(BeTrue(), "ArgoCD CRD should be deleted")
	})

	It("starts the controller without the ArgoCD CRD", func(ctx SpecContext) {
		deploy := &appsv1.Deployment{}
		key := types.NamespacedName{Namespace: controllerNamespace, Name: controllerDeployment}
		Expect(cfg.kubeClient.Get(ctx, key, deploy)).To(Succeed())
		patch := client.MergeFrom(deploy.DeepCopy())
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}
		deploy.Spec.Template.Annotations["e2e.dronenb.io/restartedAt"] = time.Now().Format(time.RFC3339)
		Expect(cfg.kubeClient.Patch(ctx, deploy, patch)).To(Succeed())

		Eventually(func(g Gomega) {
			d := &appsv1.Deployment{}
			g.Expect(cfg.kubeClient.Get(ctx, key, d)).To(Succeed())
			g.Expect(d.Status.ObservedGeneration).To(BeNumerically(">=", d.Generation))
			g.Expect(d.Status.UpdatedReplicas).To(Equal(*d.Spec.Replicas))
			g.Expect(d.Status.AvailableReplicas).To(Equal(*d.Spec.Replicas))
			g.Expect(d.Status.Replicas).To(Equal(*d.Spec.Replicas), "old pods should be gone")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())

		Consistently(func(g Gomega) {
			pods := &corev1.PodList{}
			g.Expect(cfg.kubeClient.List(ctx, pods, client.InNamespace(controllerNamespace),
				client.MatchingLabels{"app.kubernetes.io/name": controllerDeployment})).To(Succeed())
			g.Expect(pods.Items).NotTo(BeEmpty())
			for i := range pods.Items {
				for _, cs := range pods.Items[i].Status.ContainerStatuses {
					g.Expect(cs.Ready).To(BeTrue(), "container %s should be ready", cs.Name)
					g.Expect(cs.RestartCount).To(BeZero(), "container %s should not crash", cs.Name)
				}
			}
		}, 30*time.Second, 5*time.Second).Should(Succeed())
	})

	It("drops groups from the missing ArgoCD source and keeps other sources", func(ctx SpecContext) {
		Eventually(func(g Gomega) {
			groups, err := ropcIDTokenGroups(ctx, cfg, cfg.argocdAppClientID)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(groups).To(ConsistOf(cfg.argocdAppprojectGroupID),
				"only the AppProject group should remain once the ArgoCD CRD is gone")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())
	})
})
