package discovery

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// buildDescriptor is a small helper to keep the specs readable. Access is set
// via a raw JSON payload because that's what real component descriptors carry.
func buildDescriptor(name, version string, resources ...v2.Resource) *v2.Descriptor {
	if resources == nil {
		resources = []v2.Resource{}
	}
	return &v2.Descriptor{
		Component: v2.Component{
			ComponentMeta: v2.ComponentMeta{
				ObjectMeta: v2.ObjectMeta{Name: name, Version: version},
			},
			Provider:  "ocm.software",
			Resources: resources,
			// Non-nil slices keep the JSON round-trip stable for the CEL evaluator.
			Sources:            []v2.Source{},
			References:         []v2.Reference{},
			RepositoryContexts: []*runtime.Raw{},
		},
	}
}

func buildResource(name, version, resType string, accessJSON string) v2.Resource {
	return v2.Resource{
		ElementMeta: v2.ElementMeta{
			ObjectMeta: v2.ObjectMeta{Name: name, Version: version},
		},
		Type:     resType,
		Relation: v2.LocalRelation,
		Access:   &runtime.Raw{Data: []byte(accessJSON)},
	}
}

var _ = Describe("applyExtract", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("resources mode", func() {
		It("projects one entry per resource with resource + component bindings", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					buildResource("res-1", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/foo:1"}`),
					buildResource("res-2", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/bar:1"}`),
				),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByResources: map[string]string{
					"imageRef":     "resource.access.imageReference",
					"resourceName": "resource.name",
					"componentID":  `component.name + "@" + component.version`,
				},
			})
			Expect(err).ToNot(HaveOccurred())

			list, ok := out.([]map[string]any)
			Expect(ok).To(BeTrue())
			Expect(list).To(HaveLen(2))
			Expect(list[0]).To(Equal(map[string]any{
				"imageRef":     "ghcr.io/foo:1",
				"resourceName": "res-1",
				"componentID":  "comp-a@1.0.0",
			}))
			Expect(list[1]["imageRef"]).To(Equal("ghcr.io/bar:1"))
		})

		It("omits fields when an unguarded access misses (tolerant mode)", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					// localBlob access has no imageReference field
					buildResource("res-1", "1.0.0", "blob",
						`{"type":"localBlob","localReference":"sha256:abc"}`),
				),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByResources: map[string]string{
					"imageRef": "resource.access.imageReference",
					"name":     "resource.name",
				},
			})
			Expect(err).ToNot(HaveOccurred())

			list := out.([]map[string]any)
			Expect(list).To(HaveLen(1))
			Expect(list[0]).To(HaveKey("name"))
			Expect(list[0]).ToNot(HaveKey("imageRef"))
		})

		It("omits fields when CEL optional access resolves to null", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					buildResource("res-1", "1.0.0", "blob",
						`{"type":"localBlob","localReference":"sha256:abc"}`),
				),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByResources: map[string]string{
					"imageRef": "resource.access.?imageReference.orValue(null)",
					"name":     "resource.name",
				},
			})
			Expect(err).ToNot(HaveOccurred())

			list := out.([]map[string]any)
			Expect(list).To(HaveLen(1))
			Expect(list[0]).To(HaveKey("name"))
			Expect(list[0]).ToNot(HaveKey("imageRef"))
		})

		It("returns a compile error with the offending field name", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					buildResource("res-1", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/foo:1"}`),
				),
			}

			_, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByResources: map[string]string{
					"broken": "resource.access.((", // syntactically invalid
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`compile field "broken"`))
		})

		It("rejects a bare identifier at compile time (no bare-field shortcut)", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					buildResource("res-1", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/foo:1"}`),
				),
			}

			// Only `resource` and `component` are bound; bare `access` fails
			// at compile time. Guards against accidentally re-enabling the
			// bare-field style.
			_, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByResources: map[string]string{
					"imageRef": "access.imageReference",
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("undeclared reference to 'access'"))
		})

		It("produces no entries when a component has no resources", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-empty", "1.0.0"),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByResources: map[string]string{"name": "resource.name"},
			})
			Expect(err).ToNot(HaveOccurred())
			Expect(out).To(BeNil())
		})
	})

	Describe("components mode", func() {
		It("projects one entry per component", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					buildResource("res-1", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/foo:1"}`),
					buildResource("res-2", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/bar:1"}`),
				),
				buildDescriptor("comp-b", "2.0.0"),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByComponents: map[string]string{
					"name":          "component.name",
					"version":       "component.version",
					"resourceCount": "size(component.resources)",
				},
			})
			Expect(err).ToNot(HaveOccurred())

			list := out.([]map[string]any)
			Expect(list).To(HaveLen(2))
			Expect(list[0]).To(Equal(map[string]any{
				"name":          "comp-a",
				"version":       "1.0.0",
				"resourceCount": int64(2),
			}))
			Expect(list[1]).To(Equal(map[string]any{
				"name":          "comp-b",
				"version":       "2.0.0",
				"resourceCount": int64(0),
			}))
		})

		It("keeps components even when they have zero resources", func() {
			// Guards against the extract.byResources gotcha where zero-resource
			// components silently drop from the output.
			descs := []*v2.Descriptor{
				buildDescriptor("comp-empty", "1.0.0"),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByComponents: map[string]string{"name": "component.name"},
			})
			Expect(err).ToNot(HaveOccurred())

			list := out.([]map[string]any)
			Expect(list).To(HaveLen(1))
			Expect(list[0]["name"]).To(Equal("comp-empty"))
		})

		It("rejects a bare identifier at compile time (no bare-field shortcut)", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0"),
			}

			_, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				ByComponents: map[string]string{
					"name": "name", // bare, should fail
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("undeclared reference to 'name'"))
		})
	})

	Describe("expression mode", func() {
		It("evaluates a CEL expression against the components list", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("comp-a", "1.0.0",
					buildResource("res-1", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/foo:1"}`),
					buildResource("res-2", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/bar:1"}`),
				),
				buildDescriptor("comp-b", "2.0.0",
					buildResource("res-3", "1.0.0", "ociImage",
						`{"type":"ociArtifact","imageReference":"ghcr.io/baz:1"}`),
				),
			}

			out, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				Expression: `
					components
						.map(c, c.resources.map(r, r.access.imageReference))
						.flatten()
				`,
			})
			Expect(err).ToNot(HaveOccurred())

			list, ok := out.([]any)
			Expect(ok).To(BeTrue())
			Expect(list).To(ConsistOf("ghcr.io/foo:1", "ghcr.io/bar:1", "ghcr.io/baz:1"))
		})

		It("rejects an expression referencing the removed `root` binding", func() {
			descs := []*v2.Descriptor{
				buildDescriptor("any", "1.0.0"),
			}

			_, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				Expression: `{"rootName": root.name}`,
			})
			Expect(err).To(HaveOccurred())
		})

		It("returns a compile error for invalid CEL syntax", func() {
			descs := []*v2.Descriptor{buildDescriptor("comp-a", "1.0.0")}
			_, err := applyExtract(ctx, descs, &v1alpha1.Extract{
				Expression: `components.map(c,`, // truncated
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("compile expression"))
		})
	})

	Describe("dispatch", func() {
		It("errors when no mode is set", func() {
			_, err := applyExtract(ctx, nil, &v1alpha1.Extract{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("one of byResources, byComponents, expression"))
		})
	})
})
