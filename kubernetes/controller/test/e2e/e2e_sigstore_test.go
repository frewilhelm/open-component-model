package e2e

import (
	"os"
	"os/exec"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"ocm.software/open-component-model/kubernetes/controller/test/utils"
)

// renovate: datasource=github-releases depName=open-component-model/open-component-model
const sigstoreFixtureVersion = "0.14.0-rc.2"

var _ = Describe("Sigstore E2E Tests", func() {
	testdata := filepath.Join(os.Getenv("PROJECT_DIR"), "test/e2e/testdata/sigstore")

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		utils.DumpLogs("default", "component")
		utils.DumpLogs("ocm-k8s-toolkit-system", "component")
	})

	It("verifies a real GHCR-hosted OCM component version signed with keyless Sigstore", func(ctx SpecContext) {
		By("bootstrapping the sigstore verification example")
		Expect(utils.DeployResource(ctx, filepath.Join(testdata, Bootstrap))).To(Succeed())

		By("waiting for the component to be ready — signature must be verified by the controller")
		Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout,
			"component.delivery.ocm.software/sigstore-component")).To(Succeed())

		By("confirming the controller ran signature verification (not a cache hit)")
		// "verified signature" is logged by workerpool.verifySignatures on every
		// successful enforcing verification. If it is absent the descriptor was
		// served from the LRU cache without re-running the handler — which means
		// the test only proves the status condition, not the code path.
		Eventually(func() (string, error) {
			out, err := utils.Run(exec.CommandContext(ctx,
				"kubectl", "logs",
				"-n", "ocm-k8s-toolkit-system",
				"-l", "app.kubernetes.io/name=ocm-k8s-toolkit",
				"--tail=500",
			))
			return string(out), err
		}, timeout).Should(ContainSubstring(`"verified signature"`))
	})
})
