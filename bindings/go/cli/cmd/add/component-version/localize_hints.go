package componentversion

// localize_hints.go implements the --localize-hints post-processing callback.
// It is invoked via constructor.ComponentConstructionCallbacks.OnEndResourceConstruct
// after each resource is constructed.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	localizationv1alpha1 "ocm.software/open-component-model/bindings/go/descriptor/runtime/labels/localization/v1alpha1"
	"ocm.software/open-component-model/bindings/go/oci/looseref"
	ociaccessv1 "ocm.software/open-component-model/bindings/go/oci/spec/access/v1"
)

// ociImageTypeNames is the set of access type name prefixes that identify OCI image resources.
var ociImageTypeNames = []string{
	ociaccessv1.OCIImageType, // "OCIImage"
	ociaccessv1.LegacyType,   // "ociArtifact"
	ociaccessv1.LegacyType2,  // "ociRegistry"
	ociaccessv1.LegacyType3,  // "ociImage"
}

// attachLocalizationLabel is the OnEndResourceConstruct callback used when --localize-hints is set.
// For each resource with an OCI image access type that does not already carry the
// ocm.software/localization label, it computes the image coordinates from the access spec
// and attaches the signed label.
func attachLocalizationLabel(ctx context.Context, res *descriptor.Resource, constructErr error) error {
	if constructErr != nil || res == nil {
		// Don't modify a resource that failed to construct.
		return constructErr
	}

	if res.Access == nil {
		return nil
	}

	// Only act on OCI image access types.
	typeName := strings.SplitN(res.Access.GetType().GetName(), "/", 2)[0]
	if !isOCIImageType(typeName) {
		return nil
	}

	// Skip resources that already carry the label (manual authoring wins).
	for _, lbl := range res.Labels {
		if lbl.Name == localizationv1alpha1.LabelName {
			return nil
		}
	}

	// Extract the image reference from the access spec.
	imageRef, err := extractOCIImageRef(res)
	if err != nil || imageRef == "" {
		slog.WarnContext(ctx, "skipping localization label: could not extract OCI image reference",
			slog.String("resource", res.Name),
			slog.String("accessType", res.Access.GetType().GetName()))
		return nil
	}

	mk, err := parseImageRefToMatchKey(imageRef)
	if err != nil {
		slog.WarnContext(ctx, "skipping localization label: could not parse OCI image reference",
			slog.String("resource", res.Name),
			slog.String("imageRef", imageRef),
			slog.String("error", err.Error()))
		return nil
	}

	lv := localizationv1alpha1.Value{
		Type:     "ociImage",
		MatchKey: mk,
	}

	raw, err := json.Marshal(lv)
	if err != nil {
		return nil // best-effort
	}

	lbl := descriptor.Label{
		Name:    localizationv1alpha1.LabelName,
		Version: localizationv1alpha1.Version,
		Value:   json.RawMessage(raw),
		Signing: true, // signed label so the controller can trust the coordinates
	}
	res.Labels = append(res.Labels, lbl)
	return nil
}

// isOCIImageType reports whether a type name (without version) is an OCI image type.
func isOCIImageType(name string) bool {
	for _, t := range ociImageTypeNames {
		if strings.EqualFold(name, t) {
			return true
		}
	}
	return false
}

// extractOCIImageRef returns the imageReference from the resource's access spec.
func extractOCIImageRef(res *descriptor.Resource) (string, error) {
	var acc struct {
		ImageReference string `json:"imageReference"`
	}
	raw, err := json.Marshal(res.Access)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &acc); err != nil {
		return "", err
	}
	return acc.ImageReference, nil
}

// parseImageRefToMatchKey parses an OCI image reference into a MatchKey using the canonical
// looseref parser, which handles all valid OCI reference forms including bare names, tag-only,
// digest-only, and tag+digest combinations.
// Input: "ghcr.io/stefanprodan/podinfo:6.9.1@sha256:abc"
// Output: MatchKey{Registry:"ghcr.io", Repository:"stefanprodan/podinfo", Tag:"6.9.1", Digest:"sha256:abc"}
func parseImageRefToMatchKey(ref string) (localizationv1alpha1.MatchKey, error) {
	parsed, err := looseref.ParseReference(ref)
	if err != nil {
		return localizationv1alpha1.MatchKey{}, err
	}

	var digest string
	if strings.HasPrefix(parsed.Reference.Reference, "sha") {
		digest = parsed.Reference.Reference
	}

	return localizationv1alpha1.MatchKey{
		Registry:   parsed.Registry,
		Repository: parsed.Repository,
		Tag:        parsed.Tag,
		Digest:     digest,
	}, nil
}
