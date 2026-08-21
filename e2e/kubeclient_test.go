package e2e_test

import (
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// newKubeClientConfig loads the in-cluster or kubeconfig rest.Config, following
// controller-runtime's precedence: KUBECONFIG env > ~/.kube/config > in-cluster.
func newKubeClientConfig() *rest.Config {
	cfg, err := config.GetConfig()
	Expect(err).NotTo(HaveOccurred(), "must be able to load a Kubernetes client config (in-cluster or kubeconfig)")
	return cfg
}

// newKubeClient returns a controller-runtime client for reading and mutating
// Kubernetes resources from the e2e suite. The scheme is populated with the
// built-in core, apps, and RBAC types watched/managed by the controller.
func newKubeClient() client.Client {
	scheme := clientgoscheme.Scheme

	kubeClient, err := client.New(newKubeClientConfig(), client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred(), "controller-runtime client must be constructible")
	Expect(kubeClient).NotTo(BeNil())

	return kubeClient
}

// newTypedClient returns a Kubernetes typed clientset, used for diagnostics
// (e.g. streaming controller pod logs) that are not available through the
// watch-based controller-runtime client.
func newTypedClient() kubernetes.Interface {
	typed, err := kubernetes.NewForConfig(newKubeClientConfig())
	Expect(err).NotTo(HaveOccurred(), "k8s.io/client-go clientset must be constructible")
	Expect(typed).NotTo(BeNil())
	return typed
}
