package deployment

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/test"
	"ocm.software/open-component-model/bindings/go/runtime"
)

// readyableChild is any child CR the OCMDeployment creates: a client object
// that also exposes conditions so the spec can drive its readiness.
type readyableChild interface {
	client.Object
	GetConditions() []metav1.Condition
	SetConditions([]metav1.Condition)
}

var _ = Describe("OCMDeployment children", func() {
	It("creates owned children and reports Ready only once they are", func(ctx SpecContext) {
		ns := test.NamespaceForTest(ctx)
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		// No DeferCleanup for the namespace: envtest has no namespace controller, so
		// a deleted namespace stays Terminating forever. Suite teardown reaps it.

		name := "example"
		meta := metav1.ObjectMeta{Name: name, Namespace: ns.GetName()}

		By("creating the OCMDeployment")
		d := &v1alpha1.OCMDeployment{
			ObjectMeta: meta,
			Spec: v1alpha1.OCMDeploymentSpec{
				RepositorySpec:     &apiextensionsv1.JSON{Raw: []byte(`{"type":"OCIRegistry","baseUrl":"http://localhost:5000"}`)},
				Component:          "ocm.software/examples/deployment",
				Semver:             "1.0.0",
				DeployResource:     v1alpha1.ResourceID{ByReference: v1alpha1.ResourceReference{Resource: runtime.Identity{"name": "chart"}}},
				Deployer:           v1alpha1.DeployerEngineFluxCD,
				ServiceAccountName: "deployment-sa",
				Interval:           metav1.Duration{Duration: time.Hour},
			},
		}
		Expect(k8sClient.Create(ctx, d)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { test.DeleteObject(ctx, k8sClient, d) })

		repo := &v1alpha1.Repository{ObjectMeta: meta}
		component := &v1alpha1.Component{ObjectMeta: meta}
		resource := &v1alpha1.Resource{ObjectMeta: meta}

		By("the controller creating three children, each owned by the OCMDeployment")
		for _, child := range []readyableChild{repo, component, resource} {
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(child), child)).To(Succeed())
				owner := metav1.GetControllerOf(child)
				g.Expect(owner).NotTo(BeNil())
				g.Expect(owner.Name).To(Equal(name))
				g.Expect(owner.Kind).To(Equal(v1alpha1.KindOCMDeployment))
			}).WithContext(ctx).Should(Succeed())
		}

		By("the OCMDeployment staying NotReady while the children are not Ready")
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(d), d)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(d.GetConditions(), v1alpha1.ReadyCondition)).To(BeFalse())
		}).WithContext(ctx).WithTimeout(2 * time.Second).Should(Succeed())

		By("driving child readiness directly (the child controllers are not started in this suite)")
		component.Status.Component = v1alpha1.ComponentInfo{
			Component:      d.Spec.Component,
			Version:        "1.0.0",
			RepositorySpec: d.Spec.RepositorySpec,
		}
		for _, child := range []readyableChild{repo, component, resource} {
			markReady(ctx, child)
		}

		By("the OCMDeployment rolling up to Ready with the resolved version and child refs")
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(d), d)).To(Succeed())
			g.Expect(apimeta.IsStatusConditionTrue(d.GetConditions(), v1alpha1.ReadyCondition)).To(BeTrue())
			g.Expect(d.Status.DeployedComponentVersion).To(Equal("1.0.0"))
			g.Expect(d.Status.Deployed).To(HaveLen(3))
		}).WithContext(ctx).Should(Succeed())
	})
})

func markReady(ctx context.Context, child readyableChild) {
	GinkgoHelper()
	conditions := child.GetConditions()
	apimeta.SetStatusCondition(&conditions, metav1.Condition{
		Type:    v1alpha1.ReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  v1alpha1.SucceededReason,
		Message: "marked ready by the test",
	})
	child.SetConditions(conditions)
	Expect(k8sClient.Status().Update(ctx, child)).To(Succeed())
}
