package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"ocm.software/open-component-model/kubernetes/controller/test/utils"
)

const (
	DeploymentManifest = "deployment.yaml"
)

var _ = Describe("controller", func() {
	Context("deployment examples", func() {
		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}

			utils.DumpLogs("ocm-k8s-toolkit-system", "ocmdeployments.delivery.ocm.software")
		})

		for _, example := range examples {
			if !strings.HasPrefix(example.Name(), "deployment-") {
				continue
			}
			fInfo, err := os.Stat(filepath.Join(examplesDir, example.Name()))
			Expect(err).NotTo(HaveOccurred())
			if !fInfo.IsDir() {
				continue
			}

			reqFiles := []string{ComponentConstructor, DeploymentManifest}

			It("should deploy the example "+example.Name(), func(ctx SpecContext) {
				By("validating the example directory " + example.Name())
				var files []string
				Expect(filepath.WalkDir(
					filepath.Join(examplesDir, example.Name()),
					func(path string, d os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if d.IsDir() {
							return nil
						}
						files = append(files, d.Name())
						return nil
					})).To(Succeed())

				Expect(files).To(ContainElements(reqFiles), "required files %s not found in example directory %q", reqFiles, example.Name())

				By("creating and transferring a component version for " + example.Name())
				signingKey := ""
				if slices.Contains(files, PrivateKey) {
					signingKey = filepath.Join(examplesDir, example.Name(), PrivateKey)
				}
				Expect(utils.PrepareOCMComponent(
					ctx,
					example.Name(),
					filepath.Join(examplesDir, example.Name(), ComponentConstructor),
					imageRegistry,
					signingKey,
				)).To(Succeed())

				By("deploying the OCMDeployment CR")
				Expect(utils.DeployResource(ctx, filepath.Join(examplesDir, example.Name(), DeploymentManifest))).To(Succeed())

				By("waiting for the OCMDeployment CR to become ready")
				Expect(utils.WaitForResource(
					ctx, "condition=Ready=true",
					timeout,
					"ocmdeployments.delivery.ocm.software/"+example.Name(),
				)).To(Succeed())

				By("validating the example")
				name := "deployment.apps/" + example.Name() + "-podinfo"
				Expect(utils.WaitForResource(ctx, "create", timeout, name)).To(Succeed())
				Expect(utils.WaitForResource(ctx, "condition=Available", timeout, name)).To(Succeed())
				Expect(utils.WaitForResource(
					ctx, "condition=Ready=true",
					timeout,
					"pod", "-l", "app.kubernetes.io/name="+example.Name()+"-podinfo",
				)).To(Succeed())

				// Check for configuration and localization
				if strings.HasSuffix(example.Name(), "-configuration-localization") {
					By("validating the localization")
					Expect(utils.CompareResourceField(ctx,
						"pod -l app.kubernetes.io/name="+example.Name()+"-podinfo",
						"'{.items[0].spec.containers[0].image}'",
						strings.TrimLeft(imageRegistry, "http://")+"/stefanprodan/podinfo:6.9.1",
					)).To(Succeed())
					By("validating the configuration")
					Expect(utils.CompareResourceField(ctx,
						"pod -l app.kubernetes.io/name="+example.Name()+"-podinfo",
						"'{.items[0].spec.containers[0].env[?(@.name==\"PODINFO_UI_MESSAGE\")].value}'",
						example.Name(),
					)).To(Succeed())
				}
			})
		}
	})
})
