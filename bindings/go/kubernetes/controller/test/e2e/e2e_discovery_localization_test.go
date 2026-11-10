package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/test/utils"
)

const (
	localizationExample = "discovery-localization"
	localizationPrefix  = "ocm.software/ocm-k8s-toolkit/examples/" + localizationExample
	// chartsSubPath is repository B, holding the chart components. It must match
	// the resolver configuration in the example's bootstrap.yaml.
	chartsSubPath = "discovery-localization-charts"
)

// transferAcrossRepositories builds the example's component versions and splits
// them across two repositories: root, rgd and platform go to repository A (the
// registry root), the chart components go to repository B together with copies
// of their resources, so the resolved references point at the local registry.
func transferAcrossRepositories(ctx context.Context, constructorPath, registry string) error {
	ocm := utils.OCMBinary()
	tmpDir := GinkgoT().TempDir()
	ctfDir := filepath.Join(tmpDir, "ctf")

	add := exec.CommandContext(ctx, ocm, "add", "cv", "--repository", ctfDir, "--constructor", constructorPath)
	add.Dir = filepath.Dir(constructorPath)
	if _, err := utils.Run(add); err != nil {
		return fmt.Errorf("could not create ocm components: %w", err)
	}

	uploaderConfig := filepath.Join(tmpDir, "oci-uploader.ocmconfig")
	if err := os.WriteFile(uploaderConfig, []byte(
		"type: generic.config.ocm.software/v1\nconfigurations:\n- type: oci.uploader.transfer.config.ocm.software/v1alpha1\n",
	), 0o600); err != nil {
		return fmt.Errorf("could not write oci uploader ocmconfig: %w", err)
	}

	repoA := registry
	repoB := registry + "/" + chartsSubPath
	for _, t := range []struct {
		component string
		target    string
		copy      bool
	}{
		{localizationPrefix, repoA, false},
		{localizationPrefix + "/rgd", repoA, false},
		{localizationPrefix + "/platform", repoA, false},
		{localizationPrefix + "/chart-a", repoB, true},
		{localizationPrefix + "/chart-b", repoB, true},
	} {
		By(fmt.Sprintf("transferring %s to %s", t.component, t.target))
		args := []string{"transfer", "cv", fmt.Sprintf("ctf::%s//%s:1.0.0", ctfDir, t.component), t.target}
		if t.copy {
			args = append(args, "--copy-resources", "--config", uploaderConfig)
		}
		if _, err := utils.Run(exec.CommandContext(ctx, ocm, args...)); err != nil {
			return fmt.Errorf("could not transfer %s: %w", t.component, err)
		}
	}
	return nil
}

func kubectlGet(ctx context.Context, resource, jsonpath string) string {
	out, err := utils.Run(exec.CommandContext(ctx, "kubectl", "get", resource, "-n", "default", "-o", "jsonpath="+jsonpath))
	Expect(err).NotTo(HaveOccurred())
	return strings.TrimSpace(string(out))
}

// The charts live in a different repository than the component that references
// them. The resolver configuration routes the references there, the Resource
// status exposes the actual (localized) artifact references, and each chart is
// deployed with its image localized to the resolved reference.
var _ = Describe("Discovery Cross-Repository Localization Tests", func() {
	It("should resolve charts from another repository and localize their images from the Resource status", func(ctx SpecContext) {
		exampleDir := filepath.Join(examplesDir, localizationExample)
		_, err := os.Stat(exampleDir)
		Expect(err).NotTo(HaveOccurred(), "example %q must exist", localizationExample)

		Expect(transferAcrossRepositories(ctx, filepath.Join(exampleDir, ComponentConstructor), imageRegistry)).To(Succeed())

		By("bootstrapping the example")
		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Bootstrap))).To(Succeed())

		rgd := "rgd/" + localizationExample
		Expect(utils.WaitForResource(ctx, "create", timeout, rgd)).To(Succeed())
		Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout, rgd)).To(Succeed())

		By("creating an instance of the example")
		Expect(utils.DeployResource(ctx, filepath.Join(exampleDir, Instance))).To(Succeed())

		registryHost := strings.TrimPrefix(strings.TrimPrefix(imageRegistry, "http://"), "https://")

		charts := []struct {
			name   string
			tag    string
			images []string
		}{
			{localizationExample + "-chart-a", "6.9.1", []string{"image-podinfo", "image-podinfo-canary"}},
			{localizationExample + "-chart-b", "6.7.1", []string{"image-podinfo"}},
		}

		for _, c := range charts {
			By("verifying the Resources of " + c.name + " resolve to the localized references")
			resources := append([]string{c.name}, func() []string {
				var names []string
				for _, img := range c.images {
					names = append(names, c.name+"-"+img)
				}
				return names
			}()...)
			var imageRefs []string
			for _, r := range resources {
				res := "resource.delivery.ocm.software/" + r
				Expect(utils.WaitForResource(ctx, "create", timeout, res)).To(Succeed())
				Expect(utils.WaitForResource(ctx, "condition=Ready=true", timeout, res)).To(Succeed())

				ref := kubectlGet(ctx, res, "{.status.additional.reference}")
				Expect(ref).To(HavePrefix(imageRegistry+"/"+chartsSubPath+"/"),
					"Resource %q must expose the reference in repository B, where the resolver found the chart", r)
				Expect(ref).NotTo(ContainSubstring("ghcr.io"),
					"Resource %q must not expose the original upstream reference", r)
				if r != c.name {
					imageRefs = append(imageRefs, ref)
				}
			}

			By("verifying the collected image references of " + c.name)
			collected := kubectlGet(ctx, "configmap/"+c.name+"-images", "{.data.images}")
			Expect(strings.Split(collected, ",")).To(ConsistOf(imageRefs),
				"the ConfigMap must list the resolved reference of every image of %q", c.name)

			By("verifying " + c.name + " runs its localized image")
			dep := "deployment.apps/" + c.name + "-podinfo"
			Expect(utils.WaitForResource(ctx, "create", timeout, dep)).To(Succeed())
			Expect(utils.WaitForResource(ctx, "condition=Available", timeout, dep)).To(Succeed())
			image := kubectlGet(ctx, dep, "{.spec.template.spec.containers[0].image}")
			Expect(image).To(HavePrefix(registryHost+"/"+chartsSubPath+"/"), "Deployment %q must pull its localized image from repository B", dep)
			Expect(image).To(HaveSuffix(":"+c.tag), "Deployment %q must run podinfo %s", dep, c.tag)
		}
	})
})
