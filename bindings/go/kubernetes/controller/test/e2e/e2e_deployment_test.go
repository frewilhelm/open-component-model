package e2e

import (
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/test/utils"
	"ocm.software/open-component-model/bindings/go/oci/looseref"
)

const deploymentExampleDir = "deployment"

// deploymentVariant describes one of the four OCMDeployment engine combinations.
type deploymentVariant struct {
	dir             string // subdirectory under examples/deployment/
	name            string // OCMDeployment name in ocmdeployment.yaml
	targetNamespace string // namespace the workload lands in
	isHelm          bool   // true: assert Helm-specific config merge; false: kustomize
	// createNamespace=false variants need the namespace pre-created by the test.
	preCreateNamespace bool
}

var deploymentVariants = []deploymentVariant{
	{
		dir:             "flux-helm",
		name:            "podinfo-flux-helm",
		targetNamespace: "deployment-flux-helm",
		isHelm:          true,
		// Helm target-namespace RBAC must be applied before Helm install runs; pre-create ensures namespace exists.
		preCreateNamespace: true,
	},
	{
		dir:                "argo-helm",
		name:               "podinfo-argo-helm",
		targetNamespace:    "deployment-argo-helm",
		isHelm:             true,
		preCreateNamespace: true,
	},
	{
		dir:             "flux-kustomize",
		name:            "podinfo-flux-kustomize",
		targetNamespace: "deployment-flux-kustomize",
		isHelm:          false,
		// flux-kustomize sets createNamespace: false; the test must create the namespace first.
		preCreateNamespace: true,
	},
	{
		dir:             "argo-kustomize",
		name:            "podinfo-argo-kustomize",
		targetNamespace: "deployment-argo-kustomize",
		isHelm:          false,
	},
}

var _ = Describe("OCMDeployment", Ordered, func() {
	var deployDir string

	BeforeAll(func(ctx SpecContext) {
		deployDir = filepath.Join(examplesDir, deploymentExampleDir)

		By("preparing OCM component for deployment example")
		Expect(utils.PrepareOCMComponent(
			ctx,
			deploymentExampleDir,
			filepath.Join(deployDir, ComponentConstructor),
			imageRegistry,
		)).To(Succeed())
	})

	AfterEach(func() {
		if !CurrentSpecReport().Failed() {
			return
		}
		// Dump diagnostic output for all relevant resource types.
		utils.DumpLogs("default", "ocmdeployments")
		utils.DumpLogs("default", "helmreleases.helm.toolkit.fluxcd.io")
		utils.DumpLogs("default", "kustomizations.kustomize.toolkit.fluxcd.io")
		utils.DumpLogs("argocd", "applications.argoproj.io")
	})

	for _, v := range deploymentVariants {
		// capture loop variable
		It("should deploy "+v.name+" via "+v.dir, func(ctx SpecContext) {
			variantDir := filepath.Join(deployDir, v.dir)

			if v.preCreateNamespace {
				By("pre-creating target namespace " + v.targetNamespace)
				// kubectl create namespace is idempotent when --ignore-not-found can't be used;
				// ignore the error since "already exists" is acceptable.
				nsCmd := exec.CommandContext(ctx, "kubectl", "create", "namespace", v.targetNamespace) //nolint:gosec
				_, _ = utils.Run(nsCmd)
			}

			By("deploying service account and RBAC for " + v.name)
			Expect(utils.DeployResource(ctx, filepath.Join(variantDir, "serviceaccount.yaml"))).To(Succeed())

			By("deploying OCMDeployment " + v.name)
			Expect(utils.DeployResource(ctx, filepath.Join(variantDir, "ocmdeployment.yaml"))).To(Succeed())

			// Cleanup: delete the OCMDeployment and service account, then remove the target
			// namespace so the next run starts from a clean slate.
			DeferCleanup(func() {
				By("cleaning up OCMDeployment and service account for " + v.name)
				delOCM := exec.CommandContext(ctx, "kubectl", "delete", "-f", filepath.Join(variantDir, "ocmdeployment.yaml"), "--ignore-not-found", "--timeout=120s") //nolint:gosec
				_, _ = utils.Run(delOCM)
				delSA := exec.CommandContext(ctx, "kubectl", "delete", "-f", filepath.Join(variantDir, "serviceaccount.yaml"), "--ignore-not-found", "--timeout=30s") //nolint:gosec
				_, _ = utils.Run(delSA)

				// Wait briefly for applyset pruning to propagate before removing the namespace.
				time.Sleep(5 * time.Second)

				By("deleting target namespace " + v.targetNamespace)
				nsCmd := exec.CommandContext(ctx, "kubectl", "delete", "namespace", v.targetNamespace, "--ignore-not-found", "--timeout=60s") //nolint:gosec
				_, _ = utils.Run(nsCmd)
			})

			By("waiting for OCMDeployment " + v.name + " to be Ready")
			Expect(utils.WaitForResource(ctx,
				"condition=Ready=true", timeout,
				"ocmdeployment/"+v.name,
			)).To(Succeed())

			By("waiting for workload Deployment podinfo to be available in " + v.targetNamespace)
			Expect(utils.WaitForDeploymentAvailable(ctx, "podinfo", v.targetNamespace, timeout)).To(Succeed())

			By("asserting image was relocated off upstream registry")
			// Helm chart labels pods with app.kubernetes.io/name; kustomize uses app.
			podLabelSelector := "app.kubernetes.io/name=podinfo"
			if !v.isHelm {
				podLabelSelector = "app=podinfo"
			}
			image, err := utils.GetResourceField(ctx,
				"pod -l "+podLabelSelector+" -n "+v.targetNamespace,
				"{.items[0].spec.containers[0].image}",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(image).NotTo(BeEmpty())

			parsed, parseErr := looseref.ParseReference(image)
			Expect(parseErr).NotTo(HaveOccurred(),
				"container image %q should be a valid OCI reference", image)
			Expect(parsed.Registry).NotTo(
				BeElementOf("ghcr.io", "docker.io", "index.docker.io"),
				"image %q should be relocated away from upstream registries", image,
			)

			By("asserting status.appliedLocalizations contains the podinfo image")
			localizations, err := utils.GetResourceField(ctx,
				"ocmdeployment/"+v.name,
				`{.status.appliedLocalizations[*].matchKey}`,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(localizations).To(ContainSubstring("ghcr.io/stefanprodan/podinfo"),
				"appliedLocalizations should record the original podinfo image reference")

			By("asserting central config message reached the pod (config/patch chain)")
			// Both engines inject PODINFO_UI_MESSAGE: Helm via the config merge chain,
			// kustomize via the central-patches pass-through patch.
			envVal, err := utils.GetResourceField(ctx,
				"pod -l "+podLabelSelector+" -n "+v.targetNamespace,
				`{.items[0].spec.containers[0].env[?(@.name=="PODINFO_UI_MESSAGE")].value}`,
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(envVal).To(Equal("Hello from the central config component"),
				"central config should have propagated through the config/patch chain")

			if v.isHelm {
				By("asserting redis image was localized")
				Expect(localizations).To(ContainSubstring("redis"),
					"appliedLocalizations should include the redis image (enabled via local-config)")

				By("asserting busybox (unused-image) was NOT localized")
				Expect(localizations).NotTo(ContainSubstring("busybox"),
					"busybox is labelled but not referenced in chart values — should be absent from appliedLocalizations")
			} else {
				By("asserting local-patches scaled the Deployment to replicas: 2")
				replicas, err := utils.GetResourceField(ctx,
					"deployment/podinfo -n "+v.targetNamespace,
					`{.spec.replicas}`,
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(replicas).To(Equal("2"),
					"local-patches should have scaled the Deployment to 2 replicas")

				By("asserting inline config patch added the pod-template label")
				label, err := utils.GetResourceField(ctx,
					"deployment/podinfo -n "+v.targetNamespace,
					`{.spec.template.metadata.labels.config\.ocm\.software/applied}`,
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(label).To(Equal("inline"),
					"inline spec.config patch should have added the config.ocm.software/applied label")
			}
		})
	}
})
