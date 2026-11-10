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

			// Each chart is named <prefix>-<slug>; slug is the reference-path leaf
			// (chart-a / chart-b). The two nested chart components carry different
			// podinfo versions, so the reference paths differ and the deployed
			// container images differ. image is the podinfo tag each version pins.
			charts := []struct {
				name  string
				leaf  string
				image string
			}{
				{exampleName + "-chart-a", "chart-a", "6.9.1"},
				{exampleName + "-chart-b", "chart-b", "6.7.1"},
			}

			By("waiting for both generated Resources to be verified (Ready)")
			for _, c := range charts {
				Expect(utils.WaitForResource(ctx, "create", timeout, "resource.delivery.ocm.software/"+c.name)).To(Succeed())
				Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout, "resource.delivery.ocm.software/"+c.name)).To(Succeed())
			}

			By("verifying each Resource recorded the reference path to its nested component")
			for _, c := range charts {
				out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get",
					"resource.delivery.ocm.software/"+c.name, "-n", "default",
					"-o", "jsonpath={.spec.resource.byReference.referencePath}",
				))
				Expect(err).NotTo(HaveOccurred())
				path := strings.TrimSpace(string(out))
				Expect(path).To(Equal(`[{"name":"platform"},{"name":"`+c.leaf+`"}]`),
					"Resource %q must carry the two-hop reference path discovered for it", c.name)
			}

			By("waiting for both podinfo Deployments to be Available with their distinct images")
			for _, c := range charts {
				dep := "deployment.apps/" + c.name + "-podinfo"
				Expect(utils.WaitForResource(ctx, "create", timeout, dep)).To(Succeed())
				Expect(utils.WaitForResource(ctx, "condition=Available", timeout, dep)).To(Succeed())

				out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get", dep, "-n", "default",
					"-o", "jsonpath={.spec.template.spec.containers[0].image}"))
				Expect(err).NotTo(HaveOccurred())
				image := strings.TrimSpace(string(out))
				Expect(image).To(HaveSuffix(":"+c.image),
					"Deployment %q must run the podinfo image its chart version pins", dep)
			}
		})
	})
})
