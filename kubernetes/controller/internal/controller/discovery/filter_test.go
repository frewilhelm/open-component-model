package discovery

import (
	"context"
	"encoding/json"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// buildRuntimeDescriptor is the runtime-package equivalent of the v2 helper
// used in extract_test.go. The filters operate on runtime descriptors.
func buildRuntimeDescriptor(name, version string, resources ...descruntime.Resource) *descruntime.Descriptor {
	return &descruntime.Descriptor{
		Component: descruntime.Component{
			ComponentMeta: descruntime.ComponentMeta{
				ObjectMeta: descruntime.ObjectMeta{Name: name, Version: version},
			},
			Provider:  descruntime.Provider{Name: "ocm.software"},
			Resources: append([]descruntime.Resource{}, resources...),
		},
	}
}

func stringLabelJSON(name, value string) descruntime.Label {
	raw, _ := json.Marshal(value)
	return descruntime.Label{Name: name, Value: raw}
}

var _ = Describe("filterByReferenceSelector", func() {
	makeDescs := func() []*descruntime.Descriptor {
		root := buildRuntimeDescriptor("root", "1.0.0")
		root.Component.References = []descruntime.Reference{
			{
				ElementMeta: descruntime.ElementMeta{
					ObjectMeta: descruntime.ObjectMeta{Name: "ref-a", Version: "2.0.0"},
				},
				Component: "ocm.software/child-a",
			},
			{
				ElementMeta: descruntime.ElementMeta{
					ObjectMeta: descruntime.ObjectMeta{Name: "ref-b", Version: "3.0.0"},
				},
				Component: "ocm.software/child-b",
			},
		}
		return []*descruntime.Descriptor{
			root,
			buildRuntimeDescriptor("ocm.software/child-a", "2.0.0"),
			buildRuntimeDescriptor("ocm.software/child-b", "3.0.0"),
		}
	}

	It("keeps only targets of matching references (root excluded)", func() {
		out, err := filterByReferenceSelector(context.Background(), makeDescs(), &v1alpha1.Selector{
			MatchIdentity: map[string]string{IdentityAttributeComponentName: "ocm.software/child-a"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].Component.Name).To(Equal("ocm.software/child-a"))
	})

	It("filters by the local reference name too", func() {
		out, err := filterByReferenceSelector(context.Background(), makeDescs(), &v1alpha1.Selector{
			MatchIdentity: map[string]string{"name": "ref-b"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].Component.Name).To(Equal("ocm.software/child-b"))
	})

	It("returns nothing when no references match", func() {
		out, err := filterByReferenceSelector(context.Background(), makeDescs(), &v1alpha1.Selector{
			MatchIdentity: map[string]string{IdentityAttributeComponentName: "does-not-exist"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(BeEmpty())
	})

	It("propagates selector evaluation errors with the offending descriptor identity", func() {
		_, err := filterByReferenceSelector(context.Background(), makeDescs(), &v1alpha1.Selector{
			Expression: `semverCheck(identity.version, "::bad::")`,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("evaluating reference selector on"))
	})
})

var _ = Describe("filterByComponentSelector", func() {
	It("keeps components matching identity", func() {
		descs := []*descruntime.Descriptor{
			buildRuntimeDescriptor("keep", "1.0.0"),
			buildRuntimeDescriptor("drop", "1.0.0"),
		}
		out, err := filterByComponentSelector(context.Background(), descs, &v1alpha1.Selector{
			MatchIdentity: map[string]string{descruntime.IdentityAttributeName: "keep"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].Component.Name).To(Equal("keep"))
	})

	It("matches string-valued labels via matchLabels", func() {
		a := buildRuntimeDescriptor("a", "1.0.0")
		a.Component.Labels = []descruntime.Label{stringLabelJSON("env", "prod")}
		b := buildRuntimeDescriptor("b", "1.0.0")
		b.Component.Labels = []descruntime.Label{stringLabelJSON("env", "dev")}

		out, err := filterByComponentSelector(context.Background(), []*descruntime.Descriptor{a, b}, &v1alpha1.Selector{
			MatchLabels: map[string]string{"env": "prod"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(HaveLen(1))
		Expect(out[0].Component.Name).To(Equal("a"))
	})

	It("returns nil when nothing matches", func() {
		descs := []*descruntime.Descriptor{buildRuntimeDescriptor("a", "1.0.0")}
		out, err := filterByComponentSelector(context.Background(), descs, &v1alpha1.Selector{
			MatchIdentity: map[string]string{descruntime.IdentityAttributeName: "b"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(out).To(BeEmpty())
	})
})

var _ = Describe("filterResourcesInPlace", func() {
	mkRes := func(name string) descruntime.Resource {
		return descruntime.Resource{
			ElementMeta: descruntime.ElementMeta{
				ObjectMeta: descruntime.ObjectMeta{Name: name, Version: "1.0.0"},
			},
			Type: "ociImage",
		}
	}

	It("prunes each descriptor's Resources slice in place", func() {
		descs := []*descruntime.Descriptor{
			buildRuntimeDescriptor("c1", "1.0.0", mkRes("keep"), mkRes("drop")),
			buildRuntimeDescriptor("c2", "1.0.0", mkRes("drop"), mkRes("keep")),
		}
		err := filterResourcesInPlace(context.Background(), descs, &v1alpha1.Selector{
			MatchIdentity: map[string]string{descruntime.IdentityAttributeName: "keep"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(descs[0].Component.Resources).To(HaveLen(1))
		Expect(descs[0].Component.Resources[0].Name).To(Equal("keep"))
		Expect(descs[1].Component.Resources).To(HaveLen(1))
		Expect(descs[1].Component.Resources[0].Name).To(Equal("keep"))
	})

	It("leaves an empty slice when no resource matches", func() {
		descs := []*descruntime.Descriptor{
			buildRuntimeDescriptor("c1", "1.0.0", mkRes("a"), mkRes("b")),
		}
		err := filterResourcesInPlace(context.Background(), descs, &v1alpha1.Selector{
			MatchIdentity: map[string]string{descruntime.IdentityAttributeName: "c"},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(descs[0].Component.Resources).To(BeEmpty())
	})
})

var _ = Describe("labelsToJSON", func() {
	It("preserves the JSON value of every label (strings, numbers, objects)", func() {
		numeric, _ := json.Marshal(42)
		object, _ := json.Marshal(map[string]string{"env": "prod"})
		out := labelsToJSON([]descruntime.Label{
			stringLabelJSON("strKey", "yes"),
			{Name: "numeric", Value: numeric},
			{Name: "object", Value: object},
		})
		Expect(out).To(HaveKeyWithValue("strKey", "yes"))
		Expect(out).To(HaveKeyWithValue("numeric", float64(42))) // JSON numbers unmarshal to float64
		Expect(out["object"]).To(HaveKeyWithValue("env", "prod"))
	})
})

var _ = Describe("referenceIdentity", func() {
	It("adds the target component name under IdentityAttributeComponentName", func() {
		ref := &descruntime.Reference{
			ElementMeta: descruntime.ElementMeta{
				ObjectMeta: descruntime.ObjectMeta{Name: "ref", Version: "1.0.0"},
			},
			Component: "ocm.software/target",
		}
		id := referenceIdentity(ref)
		Expect(id[IdentityAttributeComponentName]).To(Equal("ocm.software/target"))
		Expect(id[descruntime.IdentityAttributeName]).To(Equal("ref"))
	})

	It("overlays ref.Component onto extraIdentity['componentName'] (canonical target wins)", func() {
		ref := &descruntime.Reference{
			ElementMeta: descruntime.ElementMeta{
				ObjectMeta:    descruntime.ObjectMeta{Name: "ref", Version: "1.0.0"},
				ExtraIdentity: runtime.Identity{IdentityAttributeComponentName: "user-supplied"},
			},
			Component: "ocm.software/canonical",
		}
		id := referenceIdentity(ref)
		Expect(id[IdentityAttributeComponentName]).To(Equal("ocm.software/canonical"))
	})
})
