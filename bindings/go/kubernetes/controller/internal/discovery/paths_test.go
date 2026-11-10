package discovery

import (
	"testing"

	"github.com/stretchr/testify/require"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/bindings/go/runtime"
)

func key(name, version string) ComponentKey { return ComponentKey{Name: name, Version: version} }

func TestReferencePaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		root  ComponentKey
		descs []*descriptor.Descriptor
		want  map[ComponentKey][]runtime.Identity
	}{
		{
			name:  "root only",
			root:  key("r", "1"),
			descs: []*descriptor.Descriptor{newDescriptor("r", "1")},
			want:  map[ComponentKey][]runtime.Identity{key("r", "1"): {}},
		},
		{
			name: "chain",
			root: key("r", "1"),
			descs: []*descriptor.Descriptor{
				newDescriptor("r", "1", withReferences(newReference("a", "a", "1"))),
				newDescriptor("a", "1", withReferences(newReference("b", "b", "1"))),
				newDescriptor("b", "1"),
			},
			want: map[ComponentKey][]runtime.Identity{
				key("r", "1"): {},
				key("a", "1"): {{"name": "a"}},
				key("b", "1"): {{"name": "a"}, {"name": "b"}},
			},
		},
		{
			name: "diamond shortest wins",
			root: key("r", "1"),
			descs: []*descriptor.Descriptor{
				newDescriptor("r", "1", withReferences(
					newReference("x", "x", "1"),
					newReference("c", "c", "1"),
				)),
				newDescriptor("x", "1", withReferences(newReference("c", "c", "1"))),
				newDescriptor("c", "1"),
			},
			want: map[ComponentKey][]runtime.Identity{
				key("r", "1"): {},
				key("x", "1"): {{"name": "x"}},
				key("c", "1"): {{"name": "c"}},
			},
		},
		{
			name: "tie broken lexicographically",
			root: key("r", "1"),
			descs: []*descriptor.Descriptor{
				newDescriptor("r", "1", withReferences(
					newReference("z", "z", "1"),
					newReference("y", "y", "1"),
				)),
				newDescriptor("z", "1", withReferences(newReference("t", "t", "1"))),
				newDescriptor("y", "1", withReferences(newReference("t", "t", "1"))),
				newDescriptor("t", "1"),
			},
			want: map[ComponentKey][]runtime.Identity{
				key("r", "1"): {},
				key("z", "1"): {{"name": "z"}},
				key("y", "1"): {{"name": "y"}},
				key("t", "1"): {{"name": "y"}, {"name": "t"}},
			},
		},
		{
			name: "extra identity kept",
			root: key("r", "1"),
			descs: []*descriptor.Descriptor{
				newDescriptor("r", "1", withReferences(
					newReference("a", "a", "1", withRefExtras(map[string]string{"platform": "linux"})),
				)),
				newDescriptor("a", "1"),
			},
			want: map[ComponentKey][]runtime.Identity{
				key("r", "1"): {},
				key("a", "1"): {{"name": "a", "platform": "linux"}},
			},
		},
		{
			name: "same name different version keeps version",
			root: key("r", "1"),
			descs: []*descriptor.Descriptor{
				newDescriptor("r", "1", withReferences(
					newReference("a", "a", "1"),
					newReference("a", "a", "2"),
				)),
				newDescriptor("a", "1"),
				newDescriptor("a", "2"),
			},
			want: map[ComponentKey][]runtime.Identity{
				key("r", "1"): {},
				key("a", "1"): {{"name": "a", "version": "1"}},
				key("a", "2"): {{"name": "a", "version": "2"}},
			},
		},
		{
			name: "cycle terminates",
			root: key("r", "1"),
			descs: []*descriptor.Descriptor{
				newDescriptor("r", "1", withReferences(newReference("a", "a", "1"))),
				newDescriptor("a", "1", withReferences(newReference("r", "r", "1"))),
			},
			want: map[ComponentKey][]runtime.Identity{
				key("r", "1"): {},
				key("a", "1"): {{"name": "a"}},
			},
		},
		{
			name:  "root missing",
			root:  key("r", "1"),
			descs: []*descriptor.Descriptor{newDescriptor("a", "1")},
			want:  nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			byKey := make(map[ComponentKey]*descriptor.Descriptor, len(tc.descs))
			for _, d := range tc.descs {
				byKey[ComponentKey{Name: d.Component.Name, Version: d.Component.Version}] = d
			}

			got := referencePaths(tc.root, byKey)
			r.Equal(tc.want, got)

			// Every computed path must be resolvable with the same matching the
			// Resource controller uses, ending at its key's descriptor.
			for target, path := range got {
				current := byKey[tc.root]
				for step, element := range path {
					var matched *descriptor.Reference
					for i := range current.Component.References {
						ref := &current.Component.References[i]
						if element.Clone().Match(ref.ToIdentity(), ocm.IdentityFuncIgnoreVersion()) {
							matched = ref
							break
						}
					}
					r.NotNilf(matched, "%s: path %v step %d (%v) did not match a reference of %s",
						tc.name, path, step, element, current.Component.String())
					current = byKey[ComponentKey{Name: matched.Component, Version: matched.Version}]
					r.NotNil(current)
				}
				r.Equal(ComponentKey{Name: current.Component.Name, Version: current.Component.Version}, target)
			}
		})
	}
}
