// Package merge assembles the final Helm values from the layered inputs the
// OCMDeployment controller gathers: chart defaults, the localization overrides,
// the config resources (in listed order) and the inline config. Later layers
// win on overlapping leaves and arrays are replaced wholesale.
//
// Localized image coordinates are protected: once localization has written an
// image leaf (repository, registry, tag, digest), a later config layer may not
// set that leaf on its own, because pairing a user-supplied tag with a
// relocated repository yields an image nobody signed. A config layer may still
// redefine the whole image (by providing the repository/image key itself), the
// acknowledged escape hatch, or leave it untouched. Setting unrelated sibling
// keys is unaffected.
package merge

import (
	"fmt"
	"sort"
	"strings"
)

// Values assembles the merged values. chartDefaults is the base, localization is
// the sparse image override tree from the localize package, and configLayers are
// the config-resource and inline layers in increasing precedence.
func Values(chartDefaults, localization map[string]any, configLayers ...map[string]any) (map[string]any, error) {
	result := deepCopyMap(chartDefaults)

	// Localization merges into the chart defaults with no restriction: it is the
	// trusted layer that establishes the protected image leaves.
	if err := mergeInto(result, localization, nil, nil); err != nil {
		return nil, err
	}

	protected := map[string]bool{}
	collectLeaves(localization, nil, protected)

	for _, layer := range configLayers {
		if err := mergeInto(result, layer, nil, protected); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// mergeInto deep-merges src into dst. prefix is the path of dst within the tree;
// protected is the set of localized leaf paths that a non-localization layer may
// not partially override.
func mergeInto(dst, src map[string]any, prefix []string, protected map[string]bool) error {
	redefinesImage := hasImageKey(src)
	for k, sv := range src {
		path := append(prefix[:len(prefix):len(prefix)], k)
		if protected[strings.Join(path, ".")] && !redefinesImage {
			return fmt.Errorf("config partially overrides localized image leaf %q; "+
				"redefine the whole image (set its repository) or leave the leaf untouched",
				strings.Join(path, "."))
		}

		dv, exists := dst[k]
		dvMap, dIsMap := dv.(map[string]any)
		svMap, sIsMap := sv.(map[string]any)
		if exists && dIsMap && sIsMap {
			if err := mergeInto(dvMap, svMap, path, protected); err != nil {
				return err
			}
			continue
		}
		dst[k] = deepCopyValue(sv)
	}
	return nil
}

// hasImageKey reports whether a map redefines an image, i.e. carries the
// repository or image key that fixes the image identity.
func hasImageKey(m map[string]any) bool {
	if _, ok := m["repository"]; ok {
		return true
	}
	_, ok := m["image"]
	return ok
}

// collectLeaves records the path of every scalar leaf in the localization
// override. Those paths are the localized image coordinates to protect.
func collectLeaves(m map[string]any, prefix []string, out map[string]bool) {
	for k, v := range m {
		path := append(prefix[:len(prefix):len(prefix)], k)
		if child, ok := v.(map[string]any); ok {
			collectLeaves(child, path, out)
			continue
		}
		out[strings.Join(path, ".")] = true
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyValue(e)
		}
		return out
	default:
		return t
	}
}

// SortedLeaves returns the protected leaf paths for a localization override,
// sorted. Exposed for tests and diagnostics.
func SortedLeaves(localization map[string]any) []string {
	set := map[string]bool{}
	collectLeaves(localization, nil, set)
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
