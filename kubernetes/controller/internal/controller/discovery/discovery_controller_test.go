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

var _ = Describe("Discovery Controller", func() {
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

			nestedComponentName1 := "ocm.software/nested-component-1"
			nestedComponentName2 := "ocm.software/nested-component-2"
			nestedComponentName3 := "ocm.software/nested-component-3"
			nestedComponentName4 := "ocm.software/nested-component-4"

			nestedNestedComponentName1 := "ocm.software/nested-nested-component-1"
			nestedNestedComponentName2 := "ocm.software/nested-nested-component-2"

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
										Name:    "nested-component-1",
										Version: componentVersion,
									},
								},
								Component: nestedComponentName1,
							},
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "nested-component-2",
										Version: componentVersion,
									},
								},
								Component: nestedComponentName2,
							},
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "nested-component-3",
										Version: componentVersion,
									},
								},
								Component: nestedComponentName3,
							},
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "nested-component-4",
										Version: componentVersion,
									},
								},
								Component: nestedComponentName4,
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedComponentName1,
								Version: componentVersion,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "resource-a",
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
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedComponentName2,
								Version: componentVersion,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "resource-b",
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
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedComponentName3,
								Version: componentVersion,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "resource-c",
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
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedComponentName4,
								Version: componentVersion,
							},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "nested-nested-component-1",
										Version: componentVersion,
									},
								},
								Component: nestedNestedComponentName1,
							},
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "nested-nested-component-2",
										Version: componentVersion,
									},
								},
								Component: nestedNestedComponentName2,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "resource-d",
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
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedNestedComponentName1,
								Version: componentVersion,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "resource-a-1",
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
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{
								Name:    nestedNestedComponentName2,
								Version: componentVersion,
							},
						},
						Resources: []descruntime.Resource{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{
										Name:    "resource-a-2",
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
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{
						Name: componentObj.GetName(),
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("checking that the discovery has been reconciled successfully")
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})
		})
	})
})

func mustMarshalJSON(v any) []byte {
	raw, err := json.Marshal(v)
	Expect(err).ToNot(HaveOccurred())
	return raw
}
