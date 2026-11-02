package deployment

import (
	"encoding/json"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/test"
	runtime "ocm.software/open-component-model/bindings/go/runtime"
)

var _ = Describe("OCMDeployment Controller", Ordered, func() {
	var (
		namespace *corev1.Namespace
		dep       *v1alpha1.OCMDeployment
	)

	BeforeEach(func(ctx SpecContext) {
		nsName := test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
		namespace = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(k8sClient.Delete(ctx, namespace)).To(Or(Succeed(), WithTransform(client.IgnoreNotFound, Succeed())))
		})
	})

	Context("when an OCMDeployment is created with an unreachable source", func() {
		BeforeEach(func(ctx SpecContext) {
			repoSpec, err := json.Marshal(map[string]any{
				"type": "OCIRegistry/v1",
				"url":  "oci://localhost:19999/nonexistent",
			})
			Expect(err).NotTo(HaveOccurred())

			dep = &v1alpha1.OCMDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-deployment",
					Namespace: namespace.Name,
				},
				Spec: v1alpha1.OCMDeploymentSpec{
					Source: v1alpha1.ComponentSource{
						RepositorySpec: &apiextensionsv1.JSON{Raw: repoSpec},
						Component:      "ocm.software/test/podinfo",
						Semver:         ">=6.0.0",
						Interval:       metav1.Duration{Duration: time.Minute},
					},
					DeployResource: v1alpha1.OCMResourceRef{
						ByReference: v1alpha1.ResourceReference{
							Resource: runtime.Identity{"name": "chart"},
						},
					},
					Deployer: v1alpha1.DeployerEngineFluxCD,
					Interval: metav1.Duration{Duration: time.Minute},
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(k8sClient.Delete(ctx, dep)).To(Or(Succeed(), WithTransform(client.IgnoreNotFound, Succeed())))
			})
		})

		It("creates child Repository and Component CRs owned by the OCMDeployment", func(ctx SpecContext) {
			By("waiting for a child Repository to appear")
			repo := &v1alpha1.Repository{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), repo)).To(Succeed())
			}, "30s", "500ms").Should(Succeed())

			Expect(repo.GetOwnerReferences()).To(ContainElement(
				HaveField("Name", dep.Name),
			))

			By("waiting for a child Component to appear")
			comp := &v1alpha1.Component{}
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), comp)).To(Succeed())
			}, "30s", "500ms").Should(Succeed())

			Expect(comp.GetOwnerReferences()).To(ContainElement(
				HaveField("Name", dep.Name),
			))
		})

		It("sets NotReady on the OCMDeployment once child resolution stalls", func(ctx SpecContext) {
			// The resolver will attempt OCM and fail; expect either ResolutionInProgress
			// (deploy resource not yet ready) or ResourceIsNotAvailable (resolution stalled).
			test.WaitForNotReadyObject(ctx, k8sClient, dep, v1alpha1.ResolutionInProgress)
		})
	})

	Context("when Suspend is set to true", func() {
		BeforeEach(func(ctx SpecContext) {
			repoSpec, err := json.Marshal(map[string]any{
				"type": "OCIRegistry/v1",
				"url":  "oci://localhost:19999/nonexistent",
			})
			Expect(err).NotTo(HaveOccurred())

			dep = &v1alpha1.OCMDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "suspended-deployment",
					Namespace: namespace.Name,
				},
				Spec: v1alpha1.OCMDeploymentSpec{
					Source: v1alpha1.ComponentSource{
						RepositorySpec: &apiextensionsv1.JSON{Raw: repoSpec},
						Component:      "ocm.software/test/podinfo",
						Semver:         ">=6.0.0",
						Interval:       metav1.Duration{Duration: time.Minute},
					},
					DeployResource: v1alpha1.OCMResourceRef{
						ByReference: v1alpha1.ResourceReference{
							Resource: runtime.Identity{"name": "chart"},
						},
					},
					Deployer: v1alpha1.DeployerEngineFluxCD,
					Interval: metav1.Duration{Duration: time.Minute},
					Suspend:  true,
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				Expect(k8sClient.Delete(ctx, dep)).To(Or(Succeed(), WithTransform(client.IgnoreNotFound, Succeed())))
			})
		})

		It("does not create child CRs while suspended", func(ctx SpecContext) {
			// Give the controller a chance to act.
			Consistently(func(g Gomega) {
				repo := &v1alpha1.Repository{}
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(dep), repo)).
					To(MatchError(ContainSubstring("not found")))
			}, "5s", "500ms").Should(Succeed())
		})
	})
})
