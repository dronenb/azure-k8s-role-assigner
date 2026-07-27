package e2e_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var e2eClusterCreated bool

var _ = Describe("End-to-end role assignment", Ordered, func() {
	BeforeAll(func(ctx SpecContext) {
		preflight()

		By("downloading optional Key Vault inputs")
		if os.Getenv("KEY_VAULT_NAME") != "" {
			runTask(ctx, "e2e:download-key")
			runTask(ctx, "e2e:download-user-password")
		}

		By("provisioning Entra resources")
		runTask(ctx, "e2e:infra-up")
		DeferCleanup(func(ctx SpecContext) {
			if os.Getenv("E2E_DEBUG_SKIP_CLEANUP") == "true" {
				return
			}
			runTask(ctx, "e2e:infra-down")
		})

		By("creating the kind cluster")
		runTask(ctx, "e2e:cluster-up")
		e2eClusterCreated = true
		DeferCleanup(func(ctx SpecContext) {
			if os.Getenv("E2E_DEBUG_SKIP_CLEANUP") == "true" {
				return
			}
			runTask(ctx, "e2e:cluster-down")
		})

		By("building and loading the controller image")
		runTask(ctx, "e2e:build-image")
		runTask(ctx, "e2e:load-image")

		By("deploying the controller")
		runTask(ctx, "e2e:deploy")

		By("creating RBAC bindings")
		runTask(ctx, "e2e:create-rbac")
		DeferCleanup(func(ctx SpecContext) {
			if os.Getenv("E2E_DEBUG_SKIP_CLEANUP") == "true" {
				return
			}
			runTask(ctx, "e2e:cleanup")
		})
	})

	JustAfterEach(func(ctx SpecContext) {
		if CurrentSpecReport().Failed() && e2eClusterCreated {
			dumpDiagnostics(ctx)
		}
	})

	It("includes groups from RoleBinding and ClusterRoleBinding subjects", func(ctx SpecContext) {
		cfg := loadConfig()

		Eventually(func(g Gomega) {
			groups, err := fetchTokenGroups(ctx, cfg)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(groups).To(ContainElement(cfg.clusterRoleBindingGroupID), "ClusterRoleBinding group should be present")
			g.Expect(groups).To(ContainElement(cfg.roleBindingGroupID), "RoleBinding group should be present")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())
	})

	It("removes the RoleBinding group after the binding is deleted", func(ctx SpecContext) {
		cfg := loadConfig()

		run(ctx, "kubectl", "delete", "rolebinding", "test-rb-binding", "-n", cfg.namespace, "--ignore-not-found")

		Eventually(func(g Gomega) {
			groups, err := fetchTokenGroups(ctx, cfg)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(groups).NotTo(ContainElement(cfg.roleBindingGroupID), "deleted RoleBinding group should be absent")
			g.Expect(groups).To(ContainElement(cfg.clusterRoleBindingGroupID), "ClusterRoleBinding group should remain present")
		}, cfg.timeout(), cfg.pollInterval()).Should(Succeed())
	})
})

func preflight() {
	requireEnv("OIDC_ISSUER_URL")
	requireEnv("TEST_GROUP_CRB_ID")
	requireEnv("TEST_GROUP_RB_ID")
	requireEnv("E2E_TEST_USER_UPN")

	if os.Getenv("E2E_TEST_USER_PASSWORD") == "" && !fileHasContent(os.Getenv("E2E_TEST_USER_PASSWORD_FILE")) {
		requireEnv("KEY_VAULT_NAME")
		requireEnv("E2E_TEST_USER_PASSWORD_SECRET_NAME")
	}

	if !fileHasContent(firstNonEmpty(os.Getenv("SA_KEY_FILE"), filepath.Join(".e2e", "sa-signing.key"))) {
		requireEnv("KEY_VAULT_NAME")
		requireEnv("SIGNING_KEY_SECRET_NAME")
	}
}

func requireEnv(name string) {
	Expect(os.Getenv(name)).NotTo(BeEmpty(), "%s must be set before running task e2e", name)
}

type e2eConfig struct {
	tenantID                  string
	clusterAppClientID        string
	userUPN                   string
	userPassword              string
	clusterRoleBindingGroupID string
	roleBindingGroupID        string
	namespace                 string
	maxAttempts               int
	sleepSeconds              int
}

func loadConfig() e2eConfig {
	outputs := tofuOutputs()
	cfg := e2eConfig{
		tenantID:                  firstNonEmpty(os.Getenv("AZURE_TENANT_ID"), outputs["AZURE_TENANT_ID"]),
		clusterAppClientID:        firstNonEmpty(os.Getenv("CLUSTER_APP_CLIENT_ID"), outputs["CLUSTER_APP_CLIENT_ID"]),
		userUPN:                   os.Getenv("E2E_TEST_USER_UPN"),
		userPassword:              firstNonEmpty(os.Getenv("E2E_TEST_USER_PASSWORD"), readPasswordFile(os.Getenv("E2E_TEST_USER_PASSWORD_FILE"))),
		clusterRoleBindingGroupID: os.Getenv("TEST_GROUP_CRB_ID"),
		roleBindingGroupID:        os.Getenv("TEST_GROUP_RB_ID"),
		namespace:                 firstNonEmpty(os.Getenv("E2E_NS"), "test-e2e"),
		maxAttempts:               envInt("MAX_ATTEMPTS", 30),
		sleepSeconds:              envInt("SLEEP_SECONDS", 10),
	}

	Expect(cfg.tenantID).NotTo(BeEmpty(), "AZURE_TENANT_ID must be set or available from task e2e:infra-output")
	Expect(cfg.clusterAppClientID).NotTo(BeEmpty(), "CLUSTER_APP_CLIENT_ID must be available from task e2e:infra-output")
	Expect(cfg.userUPN).NotTo(BeEmpty(), "E2E_TEST_USER_UPN must be set")
	Expect(cfg.userPassword).NotTo(BeEmpty(), "E2E_TEST_USER_PASSWORD must be set or E2E_TEST_USER_PASSWORD_FILE must exist")
	Expect(cfg.clusterRoleBindingGroupID).NotTo(BeEmpty(), "TEST_GROUP_CRB_ID must be set")
	Expect(cfg.roleBindingGroupID).NotTo(BeEmpty(), "TEST_GROUP_RB_ID must be set")

	return cfg
}

func (c e2eConfig) timeout() time.Duration {
	return time.Duration(c.maxAttempts*c.sleepSeconds) * time.Second
}

func (c e2eConfig) pollInterval() time.Duration {
	return time.Duration(c.sleepSeconds) * time.Second
}

func tofuOutputs() map[string]string {
	cmd := exec.Command("task", "e2e:infra-output")
	output, err := cmd.Output()
	Expect(err).NotTo(HaveOccurred(), "task e2e:infra-output must succeed")

	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func runTask(ctx SpecContext, taskName string) string {
	return run(ctx, "task", taskName)
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

func fileHasContent(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(filepath.Clean(path))
	return err == nil && info.Size() > 0
}

func fetchTokenGroups(ctx SpecContext, cfg e2eConfig) ([]string, error) {
	if err := clearKubeloginCache(); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, "kubelogin", "get-token",
		"--login", "ropc",
		"--server-id", cfg.clusterAppClientID,
		"--client-id", cfg.clusterAppClientID,
		"--tenant-id", cfg.tenantID,
		"--username", cfg.userUPN,
		"--password", cfg.userPassword,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("kubelogin get-token failed: %w\n%s", err, output.String())
	}

	token, err := extractAccessToken(output.String())
	if err != nil {
		return nil, err
	}
	return decodeGroups(token)
}

func clearKubeloginCache() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(home, ".kube", "cache", "kubelogin"),
		filepath.Join(home, ".kube", "caches", "kubelogin"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func extractAccessToken(output string) (string, error) {
	var tokenOutput struct {
		Token  string `json:"token"`
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &tokenOutput); err != nil {
		return "", fmt.Errorf("kubelogin output should be JSON: %w: %s", err, output)
	}

	token := firstNonEmpty(tokenOutput.Status.Token, tokenOutput.Token)
	if token == "" {
		return "", fmt.Errorf("kubelogin output should include a token: %s", output)
	}
	return token, nil
}

func decodeGroups(token string) ([]string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("access token should be a JWT")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return nil, fmt.Errorf("JWT payload should be base64url encoded: %w", err)
	}

	var claims struct {
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("JWT payload should be JSON: %w: %s", err, string(payload))
	}
	return claims.Groups, nil
}

func run(ctx SpecContext, name string, args ...string) string {
	cmd := exec.CommandContext(ctx, name, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	Expect(err).NotTo(HaveOccurred(), "%s %s failed:\n%s", name, strings.Join(args, " "), output.String())
	return output.String()
}

func dumpDiagnostics(ctx SpecContext) {
	_, _ = GinkgoWriter.Write([]byte("\n--- e2e diagnostics ---\n"))
	commands := [][]string{
		{"kubectl", "--context", "kind-e2e", "get", "all", "-A"},
		{"kubectl", "--context", "kind-e2e", "get", "rolebinding,clusterrolebinding", "-A"},
		{"kubectl", "--context", "kind-e2e", "logs", "-n", "azure-k8s-role-assigner", "deployment/azure-k8s-role-assigner", "--tail=200"},
	}
	for _, command := range commands {
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		output, err := cmd.CombinedOutput()
		_, _ = fmt.Fprintf(GinkgoWriter, "\n$ %s\n%s", strings.Join(command, " "), string(output))
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "command failed: %v\n", err)
		}
	}
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
