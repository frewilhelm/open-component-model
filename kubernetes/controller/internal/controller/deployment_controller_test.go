package controller

import (
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	deliveryv1alpha1 "ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

var _ = Describe("OCMDeployment Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default", // TODO(user):Modify as needed
		}
		deployment := &deliveryv1alpha1.OCMDeployment{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind OCMDeployment")
			err := k8sClient.Get(ctx, typeNamespacedName, deployment)
			if err != nil && errors.IsNotFound(err) {
				resource := &deliveryv1alpha1.OCMDeployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: "default",
					},
					Spec: deliveryv1alpha1.OCMDeploymentSpec{
						Resources: []deliveryv1alpha1.OCMDeploymentResource{
							{
								ID: "testResource",
								Template: func() apiextensionsv1.JSON {
									b, _ := json.Marshal(map[string]any{
										"apiVersion": "v1",
										"kind":       "ConfigMap",
										"metadata": map[string]any{
											"name": "test-configmap",
										},
									})
									return apiextensionsv1.JSON{Raw: b}
								}(),
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &deliveryv1alpha1.OCMDeployment{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance OCMDeployment")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &DeploymentReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			// TODO(user): Add more specific assertions depending on your controller's reconciliation logic.
			// Example: If you expect a certain status condition after reconciliation, verify it here.
		})
	})
})
