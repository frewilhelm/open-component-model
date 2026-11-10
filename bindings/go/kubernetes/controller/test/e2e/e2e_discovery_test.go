package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/test/utils"
)

// Discovery emits one record per matching resource, including the reference
// path to the nested component that owns it. The RGD fans out a verified
// Resource and a FluxCD deployer chain per discovered chart. This test proves
// the whole chain for two distinct charts reached through two reference paths.
var _ = Describe("Discovery Reference Path Tests", func() {
	Context("when generating Resources from a Discovery", func() {
		const exampleName = "discovery-resources"

		var example os.DirEntry
		for _, e := range examples {
			if e.Name() == exampleName {
				example = e
			}
		}

		It("should discover charts and deploy a verified Resource per reference path", func(ctx SpecContext) {
			Expect(example).NotTo(BeNil(), "example %q must exist", exampleName)
			exampleDir := filepath.Join(examplesDir, exampleName)

			By("creating and transferring the component version")
			Expect(utils.PrepareOCMComponent(
				ctx,
				exampleName,
				filepath.Join(exampleDir, ComponentConstructor),
				imageRegistry,
			)).To(Succeed())

			By("bootstrapping the example")
			Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Bootstrap))).To(Succeed())

			By("waiting for the RGD to be created and ready")
			rgd := "rgd/" + exampleName
			Expect(utils.WaitForResource(ctx, "create", timeout, rgd)).To(Succeed())
			Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout, rgd)).To(Succeed())

			By("creating an instance of the example")
			Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Instance))).To(Succeed())

			// Both charts are named <prefix>-<slug>; slug is the reference-path
			// leaf (chart-a / chart-b). The Discovery finds both helm-resource
			// resources, one per nested chart component.
			charts := []string{exampleName + "-chart-a", exampleName + "-chart-b"}

			By("waiting for both generated Resources to be verified (Ready)")
			for _, c := range charts {
				Expect(utils.WaitForResource(ctx, "create", timeout, "resource.delivery.ocm.software/"+c)).To(Succeed())
				Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout, "resource.delivery.ocm.software/"+c)).To(Succeed())
			}

			By("verifying each Resource recorded the reference path to its nested component")
			for _, c := range charts {
				leaf := strings.TrimPrefix(c, exampleName+"-") // chart-a / chart-b
				out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get",
					"resource.delivery.ocm.software/"+c, "-n", "default",
					"-o", "jsonpath={.spec.resource.byReference.referencePath}",
				))
				Expect(err).NotTo(HaveOccurred())
				path := strings.TrimSpace(string(out))
				Expect(path).To(Equal(`[{"name":"platform"},{"name":"` + leaf + `"}]`),
					"Resource %q must carry the two-hop reference path discovered for it", c)
			}

			By("waiting for both podinfo Deployments to be Available")
			for _, c := range charts {
				dep := "deployment.apps/" + c + "-podinfo"
				Expect(utils.WaitForResource(ctx, "create", timeout, dep)).To(Succeed())
				Expect(utils.WaitForResource(ctx, "condition=Available", timeout, dep)).To(Succeed())
			}
		})
	})
})
