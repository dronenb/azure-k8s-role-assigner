package e2e_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestE2E(t *testing.T) {
	if os.Getenv("E2E") != "true" {
		t.Skip("set E2E=true to run end-to-end tests")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}
