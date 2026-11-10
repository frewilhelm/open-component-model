package discovery

import (
	"slices"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
)

// referencePaths returns, for every component reachable from root in byKey,
// the shortest component reference path from root, usable verbatim as a
// Resource's spec.resource.byReference.referencePath. Root maps to an empty,
// non-nil path. Among equally short paths the lexicographically smallest
// sequence of runtime.Identity.String() values wins. A root missing from
// byKey yields nil.
func referencePaths(root ComponentKey, byKey map[ComponentKey]*descriptor.Descriptor) map[ComponentKey][]runtime.Identity {
	if d, ok := byKey[root]; !ok || d == nil {
		return nil
	}

	paths := map[ComponentKey][]runtime.Identity{root: {}}
	frontier := []ComponentKey{root}

	for len(frontier) > 0 {
		next := map[ComponentKey][]runtime.Identity{}
		for _, parent := range frontier {
			parentDesc := byKey[parent]
			for i := range parentDesc.Component.References {
				ref := &parentDesc.Component.References[i]
				child := ComponentKey{Name: ref.Component, Version: ref.Version}
				// A child already in paths has an equal or shorter path from an
				// earlier level; this also breaks cycles.
				if _, done := paths[child]; done {
					continue
				}
				if d, ok := byKey[child]; !ok || d == nil {
					continue
				}
				candidate := append(slices.Clone(paths[parent]), pathElement(parentDesc, i))
				if existing, ok := next[child]; !ok || lessPath(candidate, existing) {
					next[child] = candidate
				}
			}
		}
		for child, path := range next {
			paths[child] = path
		}
		frontier = frontier[:0]
		for child := range next {
			frontier = append(frontier, child)
		}
	}

	return paths
}

// pathElement returns the reference identity of parent.Component.References[i]
// for a reference path: name and extraIdentity, plus version only when another
// reference of parent has the same identity without version. A versionless
// element keeps matching when the root moves to a new version; the version is
// kept only where findMatchingReference would otherwise pick the first of
// several same-named references.
func pathElement(parent *descriptor.Descriptor, i int) runtime.Identity {
	id := parent.Component.References[i].ToIdentity()
	withoutVersion := id.Clone()
	delete(withoutVersion, descriptor.IdentityAttributeVersion)

	want := withoutVersion.String()
	for j := range parent.Component.References {
		if j == i {
			continue
		}
		other := parent.Component.References[j].ToIdentity()
		delete(other, descriptor.IdentityAttributeVersion)
		if other.String() == want {
			return id
		}
	}
	return withoutVersion
}

// lessPath orders equally long paths by their element String() values.
func lessPath(a, b []runtime.Identity) bool {
	for k := range a {
		as, bs := a[k].String(), b[k].String()
		if as != bs {
			return as < bs
		}
	}
	return false
}
