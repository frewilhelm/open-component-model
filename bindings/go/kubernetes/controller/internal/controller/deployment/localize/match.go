package localize

// match.go implements Helm values.yaml localization: walk the values tree,
// match OCI image references against the signed localization labels, and
// substitute the relocated coordinates.

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"sigs.k8s.io/yaml"

	localizationv1alpha1 "ocm.software/open-component-model/bindings/go/descriptor/runtime/labels/localization/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
)

// ImageLocation is the resolved location of a localized image resource.
type ImageLocation struct {
	// Registry + repository of the relocated image (e.g. "localhost:5000/charts/podinfo").
	Repository string
	// Tag of the relocated image (may be empty if only a digest is available).
	Tag string
	// Digest of the relocated image (sha256:...).
	Digest string
	// MatchKey is the canonical normalized form of the original image reference.
	MatchKey string
	// LabelValue is the label that produced this location.
	LabelValue *localizationv1alpha1.Value
}

// HelmResult carries the substituted values map and recorded substitutions.
type HelmResult struct {
	// Values is the Helm values map after substitutions.
	Values map[string]any
	// AppliedLocalizations records each substitution performed.
	AppliedLocalizations []v1alpha1.AppliedLocalization
	// ImageBlockPaths are the values-tree paths (from the root) of every image-block map a
	// localization override rewrote. Config merge treats these subtrees atomically: a config
	// layer either replaces the whole block or (for a partial override) is skipped.
	ImageBlockPaths [][]string
}

// ApplyHelmLocalization walks the provided rawValuesYAML, matches each image entry
// against the supplied ImageLocations (from the signed localization labels), substitutes
// the relocated reference, and returns the updated values map plus a record of every
// substitution performed.
//
// Matching uses normalised OCI references (default registry docker.io, implicit library/ prefix).
// Conflicts (one value entry matched by two labels resolving to different locations) are
// returned as an error.
func ApplyHelmLocalization(rawValuesYAML []byte, locations []ImageLocation) (*HelmResult, error) {
	if len(rawValuesYAML) == 0 || len(locations) == 0 {
		return &HelmResult{}, nil
	}

	// Parse values.yaml into a generic tree.
	var values map[string]any
	err := yaml.Unmarshal(rawValuesYAML, &values)
	if err != nil {
		return nil, fmt.Errorf("failed to parse values.yaml: %w", err)
	}

	result := &HelmResult{Values: values}

	// Build a normalised-matchkey → location index.
	index := make(map[string]ImageLocation, len(locations))
	for _, loc := range locations {
		index[loc.MatchKey] = loc
	}

	// Walk the values tree and substitute matching image references.
	err = walkValues(values, nil, index, result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// CollectValuesMatchKeys walks rawValuesYAML and returns the set of normalized match-keys
// (registry/repository, canonical form) of every image reference it finds — scalars and image
// blocks, using the same recognition and normalization as ApplyHelmLocalization. It performs no
// substitution and needs no ImageLocations. The deployment controller uses it to create image
// Resource CRs only for labels whose match-key actually appears in the chart values.
func CollectValuesMatchKeys(rawValuesYAML []byte) (map[string]struct{}, error) {
	keys := make(map[string]struct{})
	if len(rawValuesYAML) == 0 {
		return keys, nil
	}

	var values map[string]any
	if err := yaml.Unmarshal(rawValuesYAML, &values); err != nil {
		return nil, fmt.Errorf("failed to parse values.yaml: %w", err)
	}

	collectMatchKeys(values, keys)

	return keys, nil
}

// collectMatchKeys mirrors walkValues' traversal but only records normalized match-keys.
func collectMatchKeys(node map[string]any, keys map[string]struct{}) {
	for _, v := range node {
		switch typed := v.(type) {
		case map[string]any:
			if isImageBlock(typed) {
				if fullRef := reconstructRef(typed); fullRef != "" {
					if parsed, err := parseNormalized(fullRef); err == nil {
						keys[normalizedMatchKey(parsed)] = struct{}{}

						continue
					}
				}
				// Not a resolvable single image spec — recurse like walkValues does.
				collectMatchKeys(typed, keys)
			} else {
				collectMatchKeys(typed, keys)
			}
		case string:
			if parsed, err := parseNormalized(typed); err == nil && looksLikeOCIRef(typed) {
				keys[normalizedMatchKey(parsed)] = struct{}{}
			}
		}
	}
}

// walkValues recursively walks the values map looking for image fields to substitute.
// path is the dotted key path from the values root to node (nil at the root).
func walkValues(node map[string]any, path []string, index map[string]ImageLocation, result *HelmResult) error {
	for k, v := range node {
		childPath := append(append([]string{}, path...), k)
		switch typed := v.(type) {
		case map[string]any:
			// Recurse into nested maps.
			// Also check if this map looks like an image block: has "repository" (or "image") key.
			if isImageBlock(typed) {
				err := substituteImageBlock(node, k, childPath, typed, index, result)
				if err != nil {
					return err
				}
			} else {
				err := walkValues(typed, childPath, index, result)
				if err != nil {
					return err
				}
			}
		case string:
			// Check if this scalar is an OCI reference.
			if parsed, err := parseNormalized(typed); err == nil && looksLikeOCIRef(typed) {
				err := substituteScalar(node, k, typed, parsed, index, result)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// substituteScalar substitutes a scalar string value that is an OCI reference.
func substituteScalar(parent map[string]any, key, original string, parsed reference.Named, index map[string]ImageLocation, result *HelmResult) error {
	matchKey := normalizedMatchKey(parsed)

	loc, ok := index[matchKey]
	if !ok {
		return nil
	}

	// Check for conflict: same matchKey already applied with a different target.
	// applied.MatchKey is stored as the raw reference, so normalize it before comparing.
	for _, applied := range result.AppliedLocalizations {
		appliedParsed, perr := parseNormalized(applied.MatchKey)
		if perr != nil {
			continue
		}
		if normalizedMatchKey(appliedParsed) == matchKey && applied.Applied != buildRef(loc) {
			return fmt.Errorf("localization conflict: value at key %q matches label %q but two labels resolve to different locations: %q vs %q",
				key, matchKey, applied.Applied, buildRef(loc))
		}
	}

	newRef := buildRef(loc)
	parent[key] = newRef
	result.AppliedLocalizations = append(result.AppliedLocalizations, v1alpha1.AppliedLocalization{
		MatchKey:     original,
		Applied:      newRef,
		DigestPinned: loc.Digest != "",
	})

	return nil
}

// substituteImageBlock substitutes an image-block map ({"repository": "...", "tag": "..."}).
// path is the values-tree path to this block (used to record the atomic subtree on a match).
func substituteImageBlock(parent map[string]any, key string, path []string, block map[string]any, index map[string]ImageLocation, result *HelmResult) error {
	// Reconstruct the full reference from the block for matching.
	fullRef := reconstructRef(block)
	if fullRef == "" {
		// Not enough info to form a reference — recurse into nested maps inside the block.
		return walkValues(block, path, index, result)
	}

	parsed, err := parseNormalized(fullRef)
	if err != nil {
		return walkValues(block, path, index, result)
	}

	matchKey := normalizedMatchKey(parsed)

	loc, ok := index[matchKey]
	if !ok {
		// The block is a single image spec that matched no label; do not recurse into its
		// children (a nested repository/image key here is part of this spec, not a new image).
		return nil
	}

	// Conflict check: normalize applied.MatchKey (raw ref) before comparing.
	for _, applied := range result.AppliedLocalizations {
		appliedParsed, perr := parseNormalized(applied.MatchKey)
		if perr != nil {
			continue
		}
		if normalizedMatchKey(appliedParsed) == matchKey && applied.Applied != buildRef(loc) {
			return fmt.Errorf("localization conflict: image block at key %q matches label %q but resolves to different locations", key, matchKey)
		}
	}

	// Rewrite the image block fields.
	newBlock := rewriteImageBlock(block, loc)
	parent[key] = newBlock

	result.AppliedLocalizations = append(result.AppliedLocalizations, v1alpha1.AppliedLocalization{
		MatchKey:     fullRef,
		Applied:      buildRef(loc),
		DigestPinned: loc.Digest != "",
	})
	result.ImageBlockPaths = append(result.ImageBlockPaths, path)

	return nil
}

// BuildKustomizeImages produces the kustomize images: transformer list from
// the supplied image locations. Each entry has the original reference as Name
// and the relocated registry/repo/tag/digest as the new values.
func BuildKustomizeImages(locations []ImageLocation) []KustomizeImage {
	images := make([]KustomizeImage, 0, len(locations))
	for _, loc := range locations {
		img := KustomizeImage{
			Name:    loc.MatchKey,
			NewName: loc.Repository,
			NewTag:  loc.Tag,
			Digest:  loc.Digest,
		}
		images = append(images, img)
	}

	sort.Slice(images, func(i, j int) bool { return images[i].Name < images[j].Name })

	return images
}

// KustomizeImage is one entry in a kustomize images: transformer list.
type KustomizeImage struct {
	Name    string
	NewName string
	NewTag  string
	Digest  string
}

// isImageBlock reports whether a map looks like a Helm image block.
// A block must have at least a "repository" or "image" key.
func isImageBlock(m map[string]any) bool {
	_, hasRepo := m["repository"]
	_, hasImage := m["image"]

	return hasRepo || hasImage
}

// reconstructRef builds a full OCI reference string from an image block map.
// Handles {"registry": "ghcr.io", "repository": "stefanprodan/podinfo", "tag": "6.9.1"}
// and the flat form {"image": "ghcr.io/stefanprodan/podinfo:6.9.1"} or the bare-name
// form {"image": "redis:7.0.7"} (docker.io/library/redis).
func reconstructRef(block map[string]any) string {
	// The "image" key is a combined reference alias for "repository"; accept it whenever it
	// looks like an OCI reference (registry/repo, or a bare name carrying a tag or digest).
	if img, ok := block["image"].(string); ok && looksLikeOCIRef(img) {
		return img
	}

	repo, _ := block["repository"].(string)
	if repo == "" {
		return ""
	}

	registry, _ := block["registry"].(string)
	tag, _ := block["tag"].(string)
	digest, _ := block["digest"].(string)

	var ref string
	if registry != "" {
		ref = registry + "/" + repo
	} else {
		ref = repo
	}

	if tag != "" {
		ref += ":" + tag
	}

	if digest != "" {
		ref += "@" + digest
	}

	return ref
}

// rewriteImageBlock produces a new image-block map with the relocated fields filled in.
// It preserves any keys in the original that are not registry/repository/tag/digest.
//
// Two Helm image-block conventions exist:
//   - Split:    {"registry": "ghcr.io", "repository": "stefanprodan/podinfo", "tag": "6.9.1"}
//   - Combined: {"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"} (no registry key)
//
// The rewrite preserves the original convention: if the original had no "registry" key, the
// relocated registry+path is written back into "repository" as a single combined string so that
// chart templates of the form `{{ .Values.image.repository }}:{{ .Values.image.tag }}` work
// correctly. If the original had a "registry" key, the two fields are written separately.
func rewriteImageBlock(original map[string]any, loc ImageLocation) map[string]any {
	out := make(map[string]any, len(original))
	maps.Copy(out, original)

	// If the combined reference lived in the "image" key (its alias for "repository"), write the
	// relocated reference back into "image" so the chart template keeps reading the right key, and
	// do not introduce a competing "repository" key.
	if img, ok := original["image"].(string); ok && looksLikeOCIRef(img) {
		if _, hasRepo := original["repository"]; !hasRepo {
			out["image"] = buildRef(loc)

			return out
		}
	}

	_, hadRegistry := original["registry"]
	if hadRegistry {
		// Split form: write registry and path-only repository separately.
		reg, path := splitRegistryAndPath(loc.Repository)
		if reg != "" {
			out["registry"] = reg
		} else {
			delete(out, "registry")
		}

		out["repository"] = path
	} else {
		// Combined form: keep registry+path together in repository.
		out["repository"] = loc.Repository
	}

	if loc.Tag != "" {
		out["tag"] = loc.Tag
	} else {
		delete(out, "tag")
	}

	if loc.Digest != "" {
		if _, hasDigest := original["digest"]; hasDigest {
			out["digest"] = loc.Digest
		}
	} else {
		delete(out, "digest")
	}

	return out
}

// splitRegistryAndPath splits "registry:5000/path/to/image" into ("registry:5000", "path/to/image").
func splitRegistryAndPath(fullRepo string) (registry, path string) {
	parts := strings.SplitN(fullRepo, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}

	return "", fullRepo
}

// parseNormalized parses and normalizes an OCI reference using distribution/reference.
func parseNormalized(ref string) (reference.Named, error) {
	return reference.ParseNormalizedNamed(ref)
}

// normalizedMatchKey returns the canonical string form of a reference for matching.
// Uses the registry/repository (no tag/digest) as the key so label and value can be
// compared registry-level without the tag being part of the key logic.
func normalizedMatchKey(named reference.Named) string {
	return reference.FamiliarName(named)
}

// buildRef constructs the final image reference string from an ImageLocation.
func buildRef(loc ImageLocation) string {
	ref := loc.Repository
	if loc.Tag != "" {
		ref += ":" + loc.Tag
	}

	if loc.Digest != "" {
		ref += "@" + loc.Digest
	}

	return ref
}

// looksLikeOCIRef reports whether a string looks like it could be an OCI reference.
// Accepts registry/repo form, or a bare name carrying a tag or digest (redis:7.0.7, app@sha256:...).
// URL strings (http/https) are always excluded.
func looksLikeOCIRef(s string) bool {
	if strings.HasPrefix(s, "http") {
		return false
	}
	// registry/repo form, or a bare name carrying a tag or digest.
	return strings.Contains(s, "/") || strings.Contains(s, ":") || strings.Contains(s, "@")
}

// NormalizeMatchKey normalizes a localization label MatchKey into canonical form
// for comparison with normalizedMatchKey(parseNormalized(valueRef)).
func NormalizeMatchKey(lv *localizationv1alpha1.Value) (string, error) {
	raw := buildMatchKeyRef(lv)

	named, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return "", fmt.Errorf("failed to normalize match key %q: %w", raw, err)
	}

	return reference.FamiliarName(named), nil
}

func buildMatchKeyRef(lv *localizationv1alpha1.Value) string {
	mk := lv.MatchKey

	var ref string
	if mk.Registry != "" {
		ref = mk.Registry + "/" + mk.Repository
	} else {
		ref = mk.Repository
	}

	if mk.Tag != "" {
		ref += ":" + mk.Tag
	}

	if mk.Digest != "" {
		ref += "@" + mk.Digest
	}

	return ref
}

// ImageLocationsFromLabels decodes the localization label from each image resource's
// status labels and builds the ImageLocation index.
// resolvedRef is the current location of the image (registry + repo from Resource status).
func ImageLocationsFromLabels(labels []v1alpha1.Label, resolvedRef string) ([]ImageLocation, error) {
	var locations []ImageLocation

	for _, l := range labels {
		if l.Name != localizationv1alpha1.LabelName {
			continue
		}

		var lv localizationv1alpha1.Value
		if err := json.Unmarshal(l.Value.Raw, &lv); err != nil {
			return nil, fmt.Errorf("failed to decode localization label: %w", err)
		}

		matchKey, err := NormalizeMatchKey(&lv)
		if err != nil {
			return nil, err
		}

		// Parse resolvedRef to split into registry+repo, tag, digest.
		reg, path, tag, digest := parseResolvedRef(resolvedRef)

		locations = append(locations, ImageLocation{
			Repository: reg + "/" + path,
			Tag:        tag,
			Digest:     lv.MatchKey.Digest, // from the signed label
			MatchKey:   matchKey,
			LabelValue: &lv,
		})
		_ = digest // digest from resolvedRef; the signed label digest takes precedence
	}

	return locations, nil
}

// parseResolvedRef splits "registry:5000/path/image:tag@sha256:abc" into components.
func parseResolvedRef(ref string) (registry, path, tag, digest string) {
	// Split digest.
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		digest = ref[idx+1:]
		ref = ref[:idx]
	}
	// Split tag.
	if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		if lastSlash := strings.LastIndex(ref, "/"); idx > lastSlash {
			tag = ref[idx+1:]
			ref = ref[:idx]
		}
	}
	// Split registry from path.
	if idx := strings.Index(ref, "/"); idx >= 0 {
		registry = ref[:idx]
		path = ref[idx+1:]
	} else {
		path = ref
	}

	return
}
