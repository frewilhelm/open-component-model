package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"

	_ "embed"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/test"
)

var _ = Describe("Resource Controller", func() {
	var tempDir string

	BeforeEach(func() {
		tempDir = GinkgoT().TempDir()
	})

	Context("discovery controller", func() {
		var componentName, componentObjName, discoveryName string
		var componentVersion string
		repositoryName := "ocm.software/test-repository"

		BeforeEach(func(ctx SpecContext) {
			componentObjName = test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentName = "ocm.software/test-component-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			discoveryName = "test-resource-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentVersion = "v1.0.0"

			namespace := test.NamespaceForTest(ctx)

			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

			DeferCleanup(func(ctx SpecContext) {
				resources := &v1alpha1.ResourceList{}
				Expect(k8sClient.List(ctx, resources, client.InNamespace(namespace.GetName()))).To(Succeed())
				Expect(resources.Items).To(BeEmpty(), "make sure all resources are deleted and there are no leftovers from the test")
			})
		})

		It("reconcile a nested component by reference path", func(ctx SpecContext) {
			By("creating a CTF")
			ctfName := "nested-component"
			nestedComponentName := "ocm.software/nested-component"
			nestedComponentReference := "some-reference"
			ctfPath := filepath.Join(tempDir, ctfName)
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    componentName,
								Version: componentVersion,
							},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    nestedComponentReference,
										Version: componentVersion,
									},
								},
								Component: nestedComponentName,
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedComponentName,
								Version: componentVersion,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    discoveryName,
										Version: "1.0.0",
									},
								},
								Type:     "ociArtifact",
								Relation: descruntime.ExternalRelation,
								Access: &runtime.Raw{
									Type: runtime.Type{
										Name:    "ociArtifact",
										Version: "v1",
									},
									Data: mustMarshalJSON(map[string]any{
										"imageReference": "ghcr.io/open-component-model/ocm/ocm.software/ocmcli/ocmcli-image:0.23.0",
									}),
								},
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
			})

			By("mocking a component")
			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(
				ctx,
				componentObjName,
				namespace.GetName(),
				&test.MockComponentOptions{
					Client:   k8sClient,
					Recorder: recorder,
					Info: v1alpha1.ComponentInfo{
						Component:      componentName,
						Version:        componentVersion,
						RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
					},
					Repository: repositoryName,
				},
			)
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			By("creating a discovery")
			resourceObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.ResourceSpec{
					ComponentRef: corev1.LocalObjectReference{
						Name: componentObj.GetName(),
					},
					Resource: v1alpha1.ResourceID{
						ByReference: v1alpha1.ResourceReference{
							Resource:      runtime.Identity{"name": discoveryName},
							ReferencePath: []runtime.Identity{{"name": nestedComponentReference}},
						},
					},
					AdditionalStatusFields: &apiextensionsv1.JSON{Raw: mustMarshalJSON(map[string]any{
						"reference": "resource.access.toOCI().reference",
					})},
				},
			}
			Expect(k8sClient.Create(ctx, resourceObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, resourceObj)
			})
		})
	})
})

func mustMarshalJSON(v any) []byte {
	raw, err := json.Marshal(v)
	Expect(err).ToNot(HaveOccurred())
	return raw
}
