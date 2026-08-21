package e2e_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type e2eConfig struct {
	tenantID                  string
	clusterAppClientID        string
	argocdAppClientID         string
	userUPN                   string
	userPassword              string
	clusterRoleBindingGroupID string
	roleBindingGroupID        string
	argocdConfigmapGroupID    string
	argocdAppprojectGroupID   string
	namespace                 string
	maxAttempts               int
	sleepSeconds              time.Duration

	kubeClient client.Client
}

func loadConfig() e2eConfig {
	cfg := e2eConfig{
		tenantID:                  requireEnv("AZURE_TENANT_ID"),
		clusterAppClientID:        requireEnv("CLUSTER_APP_CLIENT_ID"),
		argocdAppClientID:         os.Getenv("ARGOCD_APP_CLIENT_ID"),
		userUPN:                   requireEnv("E2E_TEST_USER_UPN"),
		userPassword:              firstNonEmpty(os.Getenv("E2E_TEST_USER_PASSWORD"), readPasswordFile(os.Getenv("E2E_TEST_USER_PASSWORD_FILE"))),
		clusterRoleBindingGroupID: requireEnv("TEST_GROUP_CRB_ID"),
		roleBindingGroupID:        requireEnv("TEST_GROUP_RB_ID"),
		argocdConfigmapGroupID:    os.Getenv("TEST_GROUP_ARGOCD_CONFIGMAP_ID"),
		argocdAppprojectGroupID:   os.Getenv("TEST_GROUP_ARGOCD_APPPROJECT_ID"),
		namespace:                 firstNonEmpty(os.Getenv("E2E_NS"), "test-e2e"),
		maxAttempts:               envInt("MAX_ATTEMPTS", 30),
		sleepSeconds:              envDurationSeconds("SLEEP_SECONDS", 10),
		kubeClient:                newKubeClient(),
	}

	Expect(cfg.userPassword).NotTo(BeEmpty(), "E2E_TEST_USER_PASSWORD must be set or E2E_TEST_USER_PASSWORD_FILE must exist")
	Expect(cfg.kubeClient).NotTo(BeNil())
	return cfg
}

func (c e2eConfig) timeout() time.Duration {
	return c.sleepSeconds * time.Duration(c.maxAttempts)
}

func (c e2eConfig) pollInterval() time.Duration {
	return c.sleepSeconds
}

func readPasswordFile(path string) string {
	if path == "" {
		return ""
	}
	content, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

func requireEnv(name string) string {
	value := os.Getenv(name)
	Expect(value).NotTo(BeEmpty(), "%s must be set before running task e2e", name)
	return value
}

func envInt(name string, defaultValue int) int {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	Expect(err).NotTo(HaveOccurred(), "%s must be an integer", name)
	return parsed
}

func envDurationSeconds(name string, defaultValue int) time.Duration {
	return time.Duration(envInt(name, defaultValue)) * time.Second
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

const (
	clusterRoleBindingName = "test-crb-binding"
	roleBindingName        = "test-rb-binding"
	controllerNamespace    = "azure-k8s-role-assigner"
	controllerDeployment   = "azure-k8s-role-assigner"
)

// dumpDiagnostics emits controller logs and cluster resources to the Ginkgo
// writer when a spec fails, using the Go Kubernetes clients (no kubectl).
func dumpDiagnostics(ctx context.Context, cfg e2eConfig) {
	By("collecting e2e diagnostics")

	typed := newTypedClient()

	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := cfg.kubeClient.List(ctx, bindings); err == nil {
		fmt.Fprintf(GinkgoWriter, "\n--- clusterrolebindings ---\n")
		for i := range bindings.Items {
			fmt.Fprintf(GinkgoWriter, "%s -> %s\n", bindings.Items[i].Name, bindings.Items[i].Subjects)
		}
	}

	podList, err := typed.CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + controllerDeployment,
	})
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "\n(no controller pods found: %v)\n", err)
		return
	}
	tail := int64(200)
	for i := range podList.Items {
		pod := &podList.Items[i]
		logs := typed.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tail})
		stream, err := logs.Stream(ctx)
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "\n--- %s logs: %v ---\n", pod.Name, err)
			continue
		}
		var buf strings.Builder
		if _, err := io.Copy(&buf, stream); err != nil {
			fmt.Fprintf(GinkgoWriter, "\n(reading %s logs: %v)\n", pod.Name, err)
		}
		stream.Close()
		fmt.Fprintf(GinkgoWriter, "\n--- %s logs (tail 200) ---\n%s\n", pod.Name, buf.String())
	}
}
