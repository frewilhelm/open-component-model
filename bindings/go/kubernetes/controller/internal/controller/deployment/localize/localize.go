// Package localize matches container images in Helm chart values (or produces
// kustomize image transformer entries) against localization hints and rewrites
// them to their relocated coordinates. It is pure: no Kubernetes types, no I/O.
//
// A hint pairs the ORIGINAL upstream coordinates of an image (the match key,
// snapshotted into the signed ocm.software/localization label before transfer)
// with the CURRENT relocated coordinates (where the image now lives, resolved
// from the OCM resource access at reconcile time). The matcher finds chart
// values that reference the original and substitutes the current.
package localize

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"ocm.software/open-component-model/bindings/go/oci/looseref"
)

// LocalizationLabel is the component-descriptor label --localize-hints writes
// and the controller reads to build hints.
const LocalizationLabel = "ocm.software/localization"

// Image is a container image reference normalized to canonical coordinates:
// a registry (docker.io when unqualified), a repository (with the implicit
// library/ prefix applied for single-segment docker.io images), a tag and an
// optional digest.
type Image struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

// ParseImage parses a reference string and normalizes it.
func ParseImage(ref string) (Image, error) {
	parsed, err := looseref.ParseReference(ref)
	if err != nil {
		return Image{}, fmt.Errorf("parse image reference %q: %w", ref, err)
	}
	digest := ""
	if r := parsed.Reference.Reference; r != "" && r != parsed.Tag {
		digest = r
	}
	return Image{
		Registry:   parsed.Registry,
		Repository: parsed.Repository,
		Tag:        parsed.Tag,
		Digest:     digest,
	}.normalized(), nil
}

// normalized applies docker.io defaulting, index.docker.io equivalence, the
// implicit library/ prefix, and folds a non-host first repository segment that
// looseref mistook for a registry (e.g. "stefanprodan/podinfo") back into the
// repository under docker.io.
func (i Image) normalized() Image {
	if i.Registry != "" && !looksLikeHost(i.Registry) {
		i.Repository = i.Registry + "/" + i.Repository
		i.Registry = ""
	}
	if i.Registry == "" || i.Registry == "index.docker.io" {
		i.Registry = "docker.io"
	}
	if i.Registry == "docker.io" && !strings.Contains(i.Repository, "/") {
		i.Repository = "library/" + i.Repository
	}
	return i
}

// looksLikeHost reports whether s is a registry host rather than a repository
// namespace segment: hosts carry a dot or port colon, or are localhost.
func looksLikeHost(s string) bool {
	return s == "localhost" || strings.ContainsAny(s, ".:")
}

// name is the registry/repository identity without tag or digest.
func (i Image) name() string {
	if i.Registry == "" {
		return i.Repository
	}
	return i.Registry + "/" + i.Repository
}

// Ref renders the full reference. withDigest controls whether a digest, if
// present, is appended.
func (i Image) Ref(withDigest bool) string {
	var b strings.Builder
	b.WriteString(i.name())
	if i.Tag != "" {
		b.WriteString(":")
		b.WriteString(i.Tag)
	}
	if withDigest && i.Digest != "" {
		b.WriteString("@")
		b.WriteString(i.Digest)
	}
	return b.String()
}

// sameIdentity reports whether two images refer to the same artifact for match
// purposes: registry, repository and tag. Digest is not part of the key because
// chart values usually carry only a tag.
func sameIdentity(a, b Image) bool {
	return a.Registry == b.Registry && a.Repository == b.Repository && a.Tag == b.Tag
}

// Hint pairs an image's original coordinates (the match key) with its current
// relocated coordinates (the substitution).
type Hint struct {
	Original Image
	Current  Image
}

// labelValue is the JSON shape of the ocm.software/localization label value.
type labelValue struct {
	Type     string `json:"type"`
	Original struct {
		Registry   string `json:"registry"`
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
	} `json:"original"`
}

// ParseLabel decodes a localization label value into the original image.
func ParseLabel(raw []byte) (Image, error) {
	var lv labelValue
	if err := json.Unmarshal(raw, &lv); err != nil {
		return Image{}, fmt.Errorf("decode %s label: %w", LocalizationLabel, err)
	}
	return Image{
		Registry:   lv.Original.Registry,
		Repository: lv.Original.Repository,
		Tag:        lv.Original.Tag,
		Digest:     lv.Original.Digest,
	}.normalized(), nil
}

// Applied records one substitution the matcher made.
type Applied struct {
	Original     string
	Applied      string
	DigestPinned bool
}

// Helm walks a chart values tree and returns a sparse override tree containing
// only the rewritten image leaves (to be merged over the chart defaults), plus
// the list of substitutions made. Unmatched hints are skipped. A single value
// matched by two hints with differing current coordinates is an error.
func Helm(values map[string]any, hints []Hint) (map[string]any, []Applied, error) {
	var applied []Applied
	ov, err := walk(values, hints, &applied)
	if err != nil {
		return nil, nil, err
	}
	m, _ := ov.(map[string]any)
	return m, applied, nil
}

// walk returns the override for a node, or nil when nothing below it changed.
func walk(node any, hints []Hint, applied *[]Applied) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		if img, shapeHasDigest, ok := imageFromMap(n); ok {
			hint, matched, err := matchHint(img, hints)
			if err != nil {
				return nil, err
			}
			if !matched {
				return nil, nil // image block that matches no hint: leave untouched, do not recurse.
			}
			override := imageOverrideMap(n, hint.Current, shapeHasDigest)
			*applied = append(*applied, appliedFor(hint, shapeHasDigest))
			return override, nil
		}
		out := map[string]any{}
		for k, v := range n {
			childOverride, err := walk(v, hints, applied)
			if err != nil {
				return nil, err
			}
			if childOverride != nil {
				out[k] = childOverride
			}
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	case string:
		img, err := ParseImage(n)
		if err != nil {
			return nil, nil // not a parseable image reference: it is just a string.
		}
		hint, matched, err := matchHint(img, hints)
		if err != nil {
			return nil, err
		}
		if !matched {
			return nil, nil
		}
		shapeHasDigest := img.Digest != ""
		*applied = append(*applied, appliedFor(hint, shapeHasDigest))
		return hint.Current.Ref(shapeHasDigest), nil
	default:
		return nil, nil
	}
}

// matchHint finds the hint whose original matches img. More than one matching
// hint with differing current coordinates is a conflict.
func matchHint(img Image, hints []Hint) (Hint, bool, error) {
	var found Hint
	matched := false
	for _, h := range hints {
		if !sameIdentity(img, h.Original) {
			continue
		}
		if matched && h.Current != found.Current {
			return Hint{}, false, fmt.Errorf("image %q matched by conflicting localizations: %q and %q",
				img.Ref(false), found.Current.Ref(true), h.Current.Ref(true))
		}
		found, matched = h, true
	}
	return found, matched, nil
}

// imageFromMap builds an image from a conventional image map: a repository (or
// image) key, with optional registry, tag and digest keys. It returns whether
// the map carries a digest key, so the substitution can preserve that shape.
func imageFromMap(m map[string]any) (img Image, shapeHasDigest bool, ok bool) {
	repoKey := repositoryKey(m)
	if repoKey == "" {
		return Image{}, false, false
	}
	repo, _ := m[repoKey].(string)
	if repo == "" {
		return Image{}, false, false
	}
	var b strings.Builder
	if reg := stringField(m, "registry"); reg != "" {
		b.WriteString(reg)
		b.WriteString("/")
	}
	b.WriteString(repo)
	if tag := stringField(m, "tag"); tag != "" {
		b.WriteString(":")
		b.WriteString(tag)
	}
	if dig := stringField(m, "digest"); dig != "" {
		b.WriteString("@")
		b.WriteString(dig)
	}
	parsed, err := ParseImage(b.String())
	if err != nil {
		return Image{}, false, false
	}
	_, shapeHasDigest = m["digest"]
	return parsed, shapeHasDigest, true
}

func repositoryKey(m map[string]any) string {
	if _, ok := m["repository"].(string); ok {
		return "repository"
	}
	if _, ok := m["image"].(string); ok {
		return "image"
	}
	return ""
}

// imageOverrideMap produces the sparse override for a matched image map,
// preserving which keys the source used (separate registry vs embedded, image
// vs repository, with or without digest).
func imageOverrideMap(src map[string]any, cur Image, shapeHasDigest bool) map[string]any {
	out := map[string]any{}
	repoKey := repositoryKey(src)
	if _, hasRegistry := src["registry"]; hasRegistry {
		out["registry"] = cur.Registry
		out[repoKey] = cur.Repository
	} else {
		out[repoKey] = cur.name()
	}
	if _, hasTag := src["tag"]; hasTag {
		out["tag"] = cur.Tag
	}
	if shapeHasDigest {
		out["digest"] = cur.Digest
	}
	return out
}

func appliedFor(h Hint, shapeHasDigest bool) Applied {
	pinned := shapeHasDigest && h.Current.Digest != ""
	return Applied{
		Original:     h.Original.Ref(false),
		Applied:      h.Current.Ref(pinned),
		DigestPinned: pinned,
	}
}

// KustomizeImage is one entry for the kustomize images transformer.
type KustomizeImage struct {
	Name    string `json:"name"`
	NewName string `json:"newName,omitempty"`
	NewTag  string `json:"newTag,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

// Kustomize produces image transformer entries from the hints. The transformer
// matches manifests by image name (original registry/repository) and rewrites
// name, tag and digest to the relocated coordinates. Entries key on the
// canonical original name; a manifest that writes a bare docker.io image
// literally (e.g. "redis") needs its literal name added as an alias.
func Kustomize(hints []Hint) []KustomizeImage {
	out := make([]KustomizeImage, 0, len(hints))
	for _, h := range hints {
		out = append(out, KustomizeImage{
			Name:    h.Original.name(),
			NewName: h.Current.name(),
			NewTag:  h.Current.Tag,
			Digest:  h.Current.Digest,
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func stringField(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case int:
		return fmt.Sprintf("%d", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		// yaml/json numbers: render without a trailing .0 for whole values.
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	default:
		return ""
	}
}
