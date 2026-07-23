package discovery

import (
	"context"
	"encoding/json"
	"fmt"

	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/selector"
)

// IdentityAttributeComponentName is the identity key under which
// referenceIdentity exposes the *target* component name of a Reference,
// disambiguating it from ElementMeta.Name (the local reference name).
// Spelled as in the OCM wire format (v2.Reference `componentName` JSON tag).
const IdentityAttributeComponentName = "componentName"

// labelsToJSON flattens descriptor labels into a name-to-JSON-value map. The
// value is whatever the label carries: string, number, bool, object, array.
// Selector.MatchLabels only inspects string values; Selector.Expression
// (CEL) can operate on the full structured form.
func labelsToJSON(labels []descruntime.Label) map[string]any {
	m := make(map[string]any, len(labels))
	for _, l := range labels {
		var v any
		if err := json.Unmarshal(l.Value, &v); err == nil {
			m[l.Name] = v
		}
	}
	return m
}

// referenceIdentity returns the identity map exposed to selectors for a
// reference. It combines the element identity (local name, target version,
// extra identity) with the target component name under the well-known
// IdentityAttributeComponentName key. `ref.Component` is the canonical
// target component name and always wins over any `extraIdentity["componentName"]`
// a user may have set, mirroring how ElementMeta.ToIdentity treats `name`
// and `version` (first-class fields overlay extraIdentity).
func referenceIdentity(ref *descruntime.Reference) runtime.Identity {
	id := ref.ToIdentity() // ElementMeta.ToIdentity returns {name, version, ...extraIdentity}
	if ref.Component != "" {
		id[IdentityAttributeComponentName] = ref.Component
	}
	return id
}

// filterByReferenceSelector keeps every descriptor that is the target of at
// least one reference (anywhere in the traversed graph) whose identity
// matches the selector.
//
// The two passes are separate because the selector matches *edges*
// (references) but produces a filter over *vertices* (descriptors). Pass 1
// scans every edge in the graph and records which target vertices satisfy
// the selector. Pass 2 sweeps the vertex set once and keeps only marked
// vertices. A single-pass version would need each target vertex to know its
// incoming edges, which the DAG doesn't track; that's a bigger refactor for
// no measurable win on typical graph sizes.
//
// The root has no incoming reference and is therefore dropped by this stage.
// componentSelector runs on the reduced set this returns, so it cannot
// re-admit the root (or any other descriptor) once dropped here.
func filterByReferenceSelector(ctx context.Context, descs []*descruntime.Descriptor, sel *v1alpha1.Selector) ([]*descruntime.Descriptor, error) {
	matchedTargets := make(map[string]struct{})
	for _, desc := range descs {
		for _, ref := range desc.Component.References {
			m := selector.Matchable{
				Identity: referenceIdentity(&ref),
				Labels:   labelsToJSON(ref.Labels),
			}
			matched, err := selector.Matches(ctx, sel, m)
			if err != nil {
				return nil, fmt.Errorf("evaluating reference selector on %s: %w",
					desc.Component.ToIdentity().String(), err)
			}
			if matched {
				matchedTargets[ref.ToComponentIdentity().String()] = struct{}{}
			}
		}
	}
	var out []*descruntime.Descriptor
	for _, desc := range descs {
		key := desc.Component.ToIdentity().String()
		if _, ok := matchedTargets[key]; ok {
			out = append(out, desc)
		}
	}
	return out, nil
}

// filterByComponentSelector keeps every descriptor whose own identity/labels
// match the selector.
func filterByComponentSelector(ctx context.Context, descs []*descruntime.Descriptor, sel *v1alpha1.Selector) ([]*descruntime.Descriptor, error) {
	var out []*descruntime.Descriptor
	for _, desc := range descs {
		m := selector.Matchable{
			Identity: desc.Component.ToIdentity(),
			Labels:   labelsToJSON(desc.Component.Labels),
		}
		matched, err := selector.Matches(ctx, sel, m)
		if err != nil {
			return nil, fmt.Errorf("evaluating component selector on %s: %w",
				desc.Component.ToIdentity().String(), err)
		}
		if matched {
			out = append(out, desc)
		}
	}
	return out, nil
}

// filterResourcesInPlace prunes each descriptor's Resources slice down to the
// entries whose identity/labels match the selector. Mutates the descriptors
// directly.
func filterResourcesInPlace(ctx context.Context, descs []*descruntime.Descriptor, sel *v1alpha1.Selector) error {
	for _, desc := range descs {
		kept := make([]descruntime.Resource, 0, len(desc.Component.Resources))
		for _, res := range desc.Component.Resources {
			m := selector.Matchable{
				Identity: res.ToIdentity(),
				Labels:   labelsToJSON(res.Labels),
			}
			matched, err := selector.Matches(ctx, sel, m)
			if err != nil {
				return fmt.Errorf("evaluating resource selector on %s: %w",
					desc.Component.ToIdentity().String(), err)
			}
			if matched {
				kept = append(kept, res)
			}
		}
		desc.Component.Resources = kept
	}
	return nil
}

