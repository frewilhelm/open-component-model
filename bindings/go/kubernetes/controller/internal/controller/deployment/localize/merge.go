package localize

// merge.go implements the Helm values deep-merge for OCMDeployment.
// Precedence (highest last): chart defaults → localization overrides → config resources → inline config.
// Kustomize: config/configResources are not supported and return UnsupportedConfiguration.

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// DeepMerge deep-merges src into dst (copy on write), where src overrides dst on conflicting keys.
// Maps are merged recursively; all other types (arrays, scalars) replace the dst value.
// An image block written by localization is treated as an atomic value: if the config provides
// only a subset of the image block keys, the whole block is replaced (config wins fully over loc).
func DeepMerge(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = make(map[string]any)
	}

	out := make(map[string]any, len(dst))
	maps.Copy(out, dst)

	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := out[k].(map[string]any); ok {
				out[k] = DeepMerge(dstMap, srcMap)

				continue
			}
		}
		// Scalar, array, or type mismatch: src replaces dst.
		out[k] = v
	}

	return out
}

// MergeHelmConfig merges the config layers on top of the localized values in precedence order.
// configLayers are the config resource blobs (in order, lowest-precedence first).
// inlineConfig is the inline spec.config (highest precedence).
// imageBlockPaths are the values-tree paths of image blocks that localization rewrote; at those
// paths the merge is atomic (see deepMergeAtomic).
func MergeHelmConfig(localizedValues map[string]any, configLayers [][]byte, inlineConfigJSON []byte, imageBlockPaths [][]string) (map[string]any, error) {
	result := localizedValues
	if result == nil {
		result = make(map[string]any)
	}

	atomic := newPathSet(imageBlockPaths)

	// Merge config resource layers.
	for i, layer := range configLayers {
		if len(layer) == 0 {
			continue
		}

		var layerMap map[string]any
		err := unmarshalYAMLOrJSON(layer, &layerMap)
		if err != nil {
			return nil, fmt.Errorf("config resource layer %d: failed to unmarshal: %w", i, err)
		}

		result = deepMergeAtomic(result, layerMap, atomic, nil)
	}

	// Merge inline config (highest precedence).
	if len(inlineConfigJSON) > 0 {
		var inline map[string]any
		err := json.Unmarshal(inlineConfigJSON, &inline)
		if err != nil {
			return nil, fmt.Errorf("inline config: failed to unmarshal: %w", err)
		}

		result = deepMergeAtomic(result, inline, atomic, nil)
	}

	return result, nil
}

// pathSet is a set of values-tree paths that must be merged atomically.
type pathSet map[string]struct{}

func newPathSet(paths [][]string) pathSet {
	s := make(pathSet, len(paths))
	for _, p := range paths {
		s[pathKey(p)] = struct{}{}
	}

	return s
}

func pathKey(path []string) string {
	return strings.Join(path, "\x00")
}

// deepMergeAtomic is DeepMerge with one exception: when a source key lands on a recorded
// image-block path, the subtree is treated atomically. A config override that provides the block's
// identity key (a map with "repository" or "image") replaces the whole localized block; a partial
// override (e.g. only "tag") is skipped, because applying it would split a localized reference into
// an inconsistent one. Everywhere else behaves like DeepMerge (maps deep-merge, others replace).
func deepMergeAtomic(dst, src map[string]any, atomic pathSet, prefix []string) map[string]any {
	if dst == nil {
		dst = make(map[string]any)
	}

	out := make(map[string]any, len(dst))
	maps.Copy(out, dst)

	for k, v := range src {
		keyPath := append(append([]string{}, prefix...), k)
		if _, isAtomic := atomic[pathKey(keyPath)]; isAtomic {
			srcMap, srcIsMap := v.(map[string]any)
			if srcIsMap && isImageBlock(srcMap) {
				// Whole-block override: config wins fully over the localized block.
				out[k] = v
			}
			// Partial override of an atomic image block: skip (keep the localized block).
			continue
		}

		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := out[k].(map[string]any); ok {
				out[k] = deepMergeAtomic(dstMap, srcMap, atomic, keyPath)

				continue
			}
		}
		// Scalar, array, or type mismatch: src replaces dst.
		out[k] = v
	}

	return out
}

// unmarshalYAMLOrJSON unmarshals YAML or JSON bytes into a map.
// Both YAML and JSON are accepted; YAML is a superset of JSON so the yaml parser handles both.
func unmarshalYAMLOrJSON(data []byte, out any) error {
	// Use encoding/json for pure-JSON to preserve number precision; fall back to YAML.
	err := json.Unmarshal(data, out)
	if err == nil {
		return nil
	}
	// Try YAML (handles YAML-only constructs and also valid JSON).
	// Import yaml via the standard sigs.k8s.io/yaml which converts to JSON internally.
	// We use a two-step: YAML → JSON → map.
	return unmarshalYAML(data, out)
}
