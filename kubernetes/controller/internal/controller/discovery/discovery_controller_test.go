package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/test"
)

var _ = Describe("Discovery Controller", func() {
	var tempDir string

	BeforeEach(func() {
		tempDir = GinkgoT().TempDir()
	})

	Context("basic reconciliation", func() {
		var componentName, componentObjName, discoveryName string
		var componentVersion string
		repositoryName := "ocm.software/test-repository"

		BeforeEach(func(ctx SpecContext) {
			componentObjName = test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentName = "ocm.software/test-component-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			discoveryName = "test-discovery-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentVersion = "v1.0.0"

			namespace := test.NamespaceForTest(ctx)
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

			DeferCleanup(func(ctx SpecContext) {
				discoveries := &v1alpha1.DiscoveryList{}
				Expect(k8sClient.List(ctx, discoveries, client.InNamespace(namespace.GetName()))).To(Succeed())
				Expect(discoveries.Items).To(BeEmpty(), "make sure all discoveries are deleted and there are no leftovers from the test")
			})
		})

		It("should not reconcile when suspended", func(ctx SpecContext) {
			By("creating a CTF with a simple component")
			ctfPath := filepath.Join(tempDir, "suspended")
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				simpleComponent(componentName, componentVersion, "resource-a"),
			})

			By("mocking a ready component")
			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			By("creating a suspended discovery")
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					Suspend:      true,
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("verifying discovery does not become ready")
			Consistently(func(ctx SpecContext) bool {
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(discoveryObj), discoveryObj)).To(Succeed())
				return discoveryObj.Status.Discovery == nil
			}).WithContext(ctx).Should(BeTrue())
		})

		It("should mark not-ready when component is not available", func(ctx SpecContext) {
			By("creating a discovery referencing a non-existent component")
			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: "non-existent-component"},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("checking that the discovery is not ready")
			test.WaitForNotReadyObject(ctx, k8sClient, discoveryObj, v1alpha1.ResourceIsNotAvailable)
		})

		It("should discover a flat component without references", func(ctx SpecContext) {
			By("creating a CTF with a single component")
			ctfPath := filepath.Join(tempDir, "flat-component")
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				simpleComponent(componentName, componentVersion, "my-image"),
			})

			By("mocking a ready component")
			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
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
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("checking discovery becomes ready and contains the component")
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			By("verifying the discovery status contains the expected component")
			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Component.Name).To(Equal(componentName))
			Expect(result[0].Component.Version).To(Equal(componentVersion))
		})

		It("should discover nested components recursively", func(ctx SpecContext) {
			By("creating a CTF with nested components")
			nestedName1 := "ocm.software/nested-1"
			nestedName2 := "ocm.software/nested-2"

			ctfPath := filepath.Join(tempDir, "nested-components")
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: componentName, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-1", Version: componentVersion},
								},
								Component: nestedName1,
							},
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-2", Version: componentVersion},
								},
								Component: nestedName2,
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				simpleComponent(nestedName1, componentVersion, "image-1"),
				simpleComponent(nestedName2, componentVersion, "chart-1"),
			})

			By("mocking a ready component")
			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
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
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("checking discovery becomes ready")
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			By("verifying discovery contains all components (root + nested)")
			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(3))
			componentNames := extractComponentNames(result)
			Expect(componentNames).To(ContainElements(componentName, nestedName1, nestedName2))
		})

		It("should discover deeply nested components", func(ctx SpecContext) {
			By("creating a CTF with 3-level nesting")
			nestedName := "ocm.software/nested"
			deepNestedName := "ocm.software/deep-nested"

			ctfPath := filepath.Join(tempDir, "deep-nested")
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: componentName, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-nested", Version: componentVersion},
								},
								Component: nestedName,
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: nestedName, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-deep", Version: componentVersion},
								},
								Component: deepNestedName,
							},
						},
						Resources: []descruntime.Resource{
							ociResource("middle-resource", "1.0.0"),
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				simpleComponent(deepNestedName, componentVersion, "leaf-resource"),
			})

			By("mocking a ready component")
			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
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
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("checking discovery becomes ready with all 3 levels")
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(3))
			componentNames := extractComponentNames(result)
			Expect(componentNames).To(ContainElements(componentName, nestedName, deepNestedName))
		})
	})

	Context("discovery fields", func() {
		var componentName, componentObjName, discoveryName string
		var componentVersion string
		repositoryName := "ocm.software/test-repository"

		BeforeEach(func(ctx SpecContext) {
			componentObjName = test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentName = "ocm.software/test-component-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			discoveryName = "test-discovery-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentVersion = "v1.0.0"

			namespace := test.NamespaceForTest(ctx)
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

			DeferCleanup(func(ctx SpecContext) {
				discoveries := &v1alpha1.DiscoveryList{}
				Expect(k8sClient.List(ctx, discoveries, client.InNamespace(namespace.GetName()))).To(Succeed())
				Expect(discoveries.Items).To(BeEmpty())
			})
		})

		It("should return compact output with extracted fields", func(ctx SpecContext) {
			By("creating a CTF")
			ctfPath := filepath.Join(tempDir, "discovery-fields")
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				simpleComponent(componentName, componentVersion, "my-image"),
			})

			By("mocking a ready component")
			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			By("creating a discovery with extract.byResources")
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					Extract: &v1alpha1.Extract{
						ByResources: map[string]string{
							"imageRef": "resource.access.imageReference",
							"type":     "resource.type",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("checking discovery becomes ready")
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			By("verifying compact output structure")
			Expect(discoveryObj.Status.Discovery).NotTo(BeNil())

			var compact []map[string]any
			Expect(json.Unmarshal(discoveryObj.Status.Discovery.Raw, &compact)).To(Succeed())
			Expect(compact).To(HaveLen(1))

			Expect(compact[0]["imageRef"]).To(Equal("ghcr.io/example/my-image:1.0.0"))
			Expect(compact[0]["type"]).To(Equal("ociArtifact"))
		})
	})


	Context("selector combinations", func() {
		var componentName, componentObjName, discoveryName string
		var componentVersion string
		repositoryName := "ocm.software/test-repository"

		var nestedName1, nestedName2 string

		BeforeEach(func(ctx SpecContext) {
			componentObjName = test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentName = "ocm.software/test-component-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			discoveryName = "test-discovery-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentVersion = "v1.0.0"
			nestedName1 = "ocm.software/nested-1-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			nestedName2 = "ocm.software/nested-2-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)

			namespace := test.NamespaceForTest(ctx)
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

			DeferCleanup(func(ctx SpecContext) {
				discoveries := &v1alpha1.DiscoveryList{}
				Expect(k8sClient.List(ctx, discoveries, client.InNamespace(namespace.GetName()))).To(Succeed())
				Expect(discoveries.Items).To(BeEmpty())
			})
		})

		setupComponentTree := func(ctx SpecContext) (*v1alpha1.Component, []byte) {
			ctfPath := filepath.Join(tempDir, test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText))
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: componentName, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-1", Version: componentVersion},
								},
								Component: nestedName1,
							},
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-2", Version: componentVersion},
								},
								Component: nestedName2,
							},
						},
						Resources: []descruntime.Resource{
							ociResource("root-image", "1.0.0"),
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: nestedName1, Version: componentVersion},
						},
						Resources: []descruntime.Resource{
							ociResource("image", "1.0.0"),
							ociResource("chart", "1.0.0"),
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: nestedName2, Version: componentVersion},
						},
						Resources: []descruntime.Resource{
							ociResource("image", "2.0.0"),
							ociResource("docs", "1.0.0"),
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
			})

			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			return componentObj, specData
		}

		It("component selector matching nothing should return ready with an empty discovery array", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ComponentSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"name": "non-existent-component"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			Expect(discoveryObj.Status.Discovery).NotTo(BeNil())
			Expect(string(discoveryObj.Status.Discovery.Raw)).To(Equal("[]"))
			readyCond := apimeta.FindStatusCondition(discoveryObj.GetConditions(), v1alpha1.ReadyCondition)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(v1alpha1.NoComponentsMatchedReason))
		})

		It("reference selector matching nothing should return ready with NoReferencesMatched reason", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ReferenceSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"componentName": "non-existent-reference"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			Expect(discoveryObj.Status.Discovery).NotTo(BeNil())
			Expect(string(discoveryObj.Status.Discovery.Raw)).To(Equal("[]"))
			readyCond := apimeta.FindStatusCondition(discoveryObj.GetConditions(), v1alpha1.ReadyCondition)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal(v1alpha1.NoReferencesMatchedReason))
		})

		It("component selector should show filtered descriptors", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ComponentSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"name": nestedName1},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			result := parseDiscoveryResult(discoveryObj)
			// Only the filtered reference (root excluded)
			Expect(result).To(HaveLen(1))
			names := extractComponentNames(result)
			Expect(names).To(ContainElement(nestedName1))
		})

		It("resource selector should show all components with filtered resources", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ResourceSelector: &v1alpha1.Selector{
						Expression: `identity.name == "image"`,
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			// All components in graph, each with only "image" resources
			result := parseDiscoveryResult(discoveryObj)
			// root + nested-1 + nested-2 (3 total), but root has no "image" so its resources are empty
			Expect(result).To(HaveLen(3))
			// nested-1 and nested-2 have "image" resources
			var totalImageResources int
			for _, desc := range result {
				for _, res := range desc.Component.Resources {
					Expect(res.Name).To(Equal("image"))
					totalImageResources++
				}
			}
			Expect(totalImageResources).To(Equal(2))
		})

		It("both selectors should show filtered components with filtered resources", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ComponentSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"name": nestedName1},
					},
					ResourceSelector: &v1alpha1.Selector{
						Expression: `identity.name == "image"`,
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			// ReferenceSelector prunes ref-2 (unmatched) so nested2 is never
			// fetched; ComponentSelector then keeps only nestedName1 (root
			// has a different component name); ResourceSelector filters to
			// resources named "image".
			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Component.Name).To(Equal(nestedName1))
			Expect(result[0].Component.Resources).To(HaveLen(1))
			Expect(result[0].Component.Resources[0].Name).To(Equal("image"))
		})

		It("component selector should include the root when it matches", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ComponentSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"name": componentName},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Component.Name).To(Equal(componentName))
		})

		It("reference selector by target componentName should filter to the matched subtree", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ReferenceSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"componentName": nestedName1},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			// referenceSelector returns only the matched targets; root is not a
			// reference target and is excluded.
			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(extractComponentNames(result)).To(ConsistOf(nestedName1))
		})

		It("reference selector by local reference name should filter to the matched edge", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ReferenceSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{"name": "ref-1"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(extractComponentNames(result)).To(ConsistOf(nestedName1))
		})

		It("empty referenceSelector is treated as unset and keeps the root", func(ctx SpecContext) {
			componentObj, _ := setupComponentTree(ctx)

			namespace := test.NamespaceForTest(ctx)
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef:      corev1.LocalObjectReference{Name: componentObj.GetName()},
					ReferenceSelector: &v1alpha1.Selector{},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			// An empty selector carries no predicates, so the referenceSelector
			// stage is skipped and the full graph (including the root) is
			// returned. This matches the ADR contract.
			result := parseDiscoveryResult(discoveryObj)
			Expect(extractComponentNames(result)).To(ConsistOf(componentName, nestedName1, nestedName2))
		})

		It("reference selector prunes traversal of unmatched subtrees", func(ctx SpecContext) {
			By("creating a CTF where one grandchild reference is intentionally missing")
			ctfPath := filepath.Join(tempDir, test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText))
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			missingName := "ocm.software/missing-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: componentName, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-good", Version: componentVersion},
								},
								Component: nestedName1,
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: nestedName1, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-broken", Version: componentVersion},
								},
								Component: missingName,
							},
						},
						Resources: []descruntime.Resource{
							ociResource("image", "1.0.0"),
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				// missingName is intentionally not added; resolving it would fail.
			})

			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			By("short-circuiting via a unique-target ReferenceSelector on nestedName1")
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ReferenceSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{
							"componentName": nestedName1,
							"version":       componentVersion,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			// If the short-circuit did not fire, the DAG would enumerate
			// nestedName1's ref-broken and try to resolve missingName, causing
			// reconcile to fail. Reaching Ready proves the target's subtree was
			// never traversed.
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(extractComponentNames(result)).To(ConsistOf(nestedName1))
		})

		It("component selector short-circuits at a uniquely-identified target", func(ctx SpecContext) {
			By("creating a CTF where one grandchild reference is intentionally missing")
			ctfPath := filepath.Join(tempDir, test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText))
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			missingName := "ocm.software/missing-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: componentName, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-good", Version: componentVersion},
								},
								Component: nestedName1,
							},
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				{
					Component: descruntime.Component{
						ComponentMeta: descruntime.ComponentMeta{
							ObjectMeta: descruntime.ObjectMeta{Name: nestedName1, Version: componentVersion},
						},
						References: []descruntime.Reference{
							{
								ElementMeta: descruntime.ElementMeta{
									ObjectMeta: descruntime.ObjectMeta{Name: "ref-broken", Version: componentVersion},
								},
								Component: missingName,
							},
						},
						Resources: []descruntime.Resource{
							ociResource("image", "1.0.0"),
						},
						Provider: descruntime.Provider{Name: "ocm.software"},
					},
				},
				// missingName is intentionally not added; resolving it would fail.
			})

			namespace := test.NamespaceForTest(ctx)
			componentObj := test.MockComponent(ctx, componentObjName, namespace.GetName(), &test.MockComponentOptions{
				Client:   k8sClient,
				Recorder: recorder,
				Info: v1alpha1.ComponentInfo{
					Component:      componentName,
					Version:        componentVersion,
					RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
				},
				Repository: repositoryName,
			})
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			By("short-circuiting via a unique-target ComponentSelector on nestedName1")
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
					ComponentSelector: &v1alpha1.Selector{
						MatchIdentity: map[string]string{
							"name":    nestedName1,
							"version": componentVersion,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})

			// ComponentSelector filters root out (different name) and keeps only
			// the short-circuit target.
			result := parseDiscoveryResult(discoveryObj)
			Expect(result).To(HaveLen(1))
			Expect(result[0].Component.Name).To(Equal(nestedName1))
		})
	})

	Context("component watch triggers", func() {
		var componentName, componentObjName, discoveryName string
		var componentVersion string
		repositoryName := "ocm.software/test-repository"

		BeforeEach(func(ctx SpecContext) {
			componentObjName = test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentName = "ocm.software/test-component-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			discoveryName = "test-discovery-" + test.SanitizeNameForK8s(ctx.SpecReport().LeafNodeText)
			componentVersion = "v1.0.0"

			namespace := test.NamespaceForTest(ctx)
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())

			DeferCleanup(func(ctx SpecContext) {
				discoveries := &v1alpha1.DiscoveryList{}
				Expect(k8sClient.List(ctx, discoveries, client.InNamespace(namespace.GetName()))).To(Succeed())
				Expect(discoveries.Items).To(BeEmpty(), "make sure all discoveries are deleted and there are no leftovers from the test")
			})
		})

		It("should reconcile when a referenced component becomes ready", func(ctx SpecContext) {
			By("creating a CTF")
			ctfPath := filepath.Join(tempDir, "component-watch")
			Expect(os.MkdirAll(ctfPath, 0o777)).To(Succeed())
			_, specData := test.SetupCTFComponentVersionRepository(ctx, ctfPath, []*descruntime.Descriptor{
				simpleComponent(componentName, componentVersion, "resource-a"),
			})

			By("creating a not-ready component (no status)")
			namespace := test.NamespaceForTest(ctx)
			componentObj := &v1alpha1.Component{
				ObjectMeta: metav1.ObjectMeta{
					Name:      componentObjName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.ComponentSpec{
					RepositoryRef: corev1.LocalObjectReference{Name: repositoryName},
					Component:     componentName,
				},
			}
			Expect(k8sClient.Create(ctx, componentObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, componentObj)
			})

			By("creating a discovery referencing the not-yet-ready component")
			discoveryObj := &v1alpha1.Discovery{
				ObjectMeta: metav1.ObjectMeta{
					Name:      discoveryName,
					Namespace: namespace.GetName(),
				},
				Spec: v1alpha1.DiscoverySpec{
					ComponentRef: corev1.LocalObjectReference{Name: componentObj.GetName()},
				},
			}
			Expect(k8sClient.Create(ctx, discoveryObj)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) {
				test.DeleteObject(ctx, k8sClient, discoveryObj)
			})

			By("verifying discovery is not ready")
			test.WaitForNotReadyObject(ctx, k8sClient, discoveryObj, v1alpha1.ResourceIsNotAvailable)

			By("making the component ready")
			old := componentObj.DeepCopy()
			componentObj.Status.Component = v1alpha1.ComponentInfo{
				Component:      componentName,
				Version:        componentVersion,
				RepositorySpec: &apiextensionsv1.JSON{Raw: specData},
			}
			componentObj.Status.Conditions = []metav1.Condition{
				{
					Type:               v1alpha1.ReadyCondition,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
					Message:            "Component is ready",
				},
			}
			componentObj.Status.ObservedGeneration = componentObj.Generation
			Expect(k8sClient.Status().Patch(ctx, componentObj, client.MergeFrom(old))).To(Succeed())

			By("verifying discovery becomes ready")
			test.WaitForReadyObject(ctx, k8sClient, discoveryObj, map[string]any{})
		})
	})
})

func parseDiscoveryResult(discovery *v1alpha1.Discovery) []*v2.Descriptor {
	GinkgoHelper()
	Expect(discovery.Status.Discovery).NotTo(BeNil(), "discovery status should not be nil")

	var result []*v2.Descriptor
	Expect(json.Unmarshal(discovery.Status.Discovery.Raw, &result)).To(Succeed())
	return result
}

func extractComponentNames(descs []*v2.Descriptor) []string {
	names := make([]string, 0, len(descs))
	for _, c := range descs {
		names = append(names, c.Component.Name)
	}
	return names
}

func simpleComponent(name, version, resourceName string) *descruntime.Descriptor {
	return &descruntime.Descriptor{
		Component: descruntime.Component{
			ComponentMeta: descruntime.ComponentMeta{
				ObjectMeta: descruntime.ObjectMeta{Name: name, Version: version},
			},
			Resources: []descruntime.Resource{
				ociResource(resourceName, "1.0.0"),
			},
			Provider: descruntime.Provider{Name: "ocm.software"},
		},
	}
}

func ociResource(name, version string) descruntime.Resource {
	return descruntime.Resource{
		ElementMeta: descruntime.ElementMeta{
			ObjectMeta: descruntime.ObjectMeta{Name: name, Version: version},
		},
		Type:     "ociArtifact",
		Relation: descruntime.ExternalRelation,
		Access: &runtime.Raw{
			Type: runtime.Type{Name: "ociArtifact", Version: "v1"},
			Data: mustMarshalJSON(map[string]any{
				"imageReference": "ghcr.io/example/" + name + ":" + version,
			}),
		},
	}
}

func mustMarshalJSON(v any) []byte {
	raw, err := json.Marshal(v)
	Expect(err).ToNot(HaveOccurred())
	return raw
}
