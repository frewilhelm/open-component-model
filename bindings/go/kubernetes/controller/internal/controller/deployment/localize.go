package deployment

// localize.go implements the localization pipeline stage for OCMDeployment.

import (
	"context"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	localizationv1alpha1 "ocm.software/open-component-model/bindings/go/descriptor/runtime/labels/localization/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/resolution"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/status"
	ociaccess "ocm.software/open-component-model/bindings/go/oci/spec/access"
	ocmruntime "ocm.software/open-component-model/bindings/go/runtime"
)

// localize runs the localization step: for Helm, walk values.yaml and substitute image
// references matching the signed localization labels on image resources.
// For kustomize, build the images transformer list.
// Config/configResources for kustomize are handled in mergeConfig (collectKustomizePatches).
func (r *Reconciler) localize(ctx context.Context, dep *v1alpha1.OCMDeployment, source *deploySource) (*localizeResult, error) {
	logger := log.FromContext(ctx)

	// Collect image resources from the deploy resource's status labels.
	comp, err := r.getChildComponent(ctx, dep)
	if err != nil || comp == nil {
		return &localizeResult{}, nil
	}

	// For Helm, pre-filter labelled images to those actually referenced in values.yaml so
	// unreferenced labelled images do not spawn a Resource CR + verification. The controller is
	// the authoritative Helm matcher, so this cannot diverge from what gets localized. Kustomize
	// matches by name engine-side, so no filter (nil) — see ADR "Open questions".
	var valueMatchKeys map[string]struct{}
	if source.IsHelm {
		valueMatchKeys, err = localize.CollectValuesMatchKeys(source.valuesYAML)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.GetOCMResourceFailedReason, err.Error())

			return nil, err
		}
	}

	imageLocations, err := r.collectImageLocations(ctx, dep, comp, valueMatchKeys)
	if err != nil {
		return nil, err
	}

	if len(imageLocations) == 0 {
		logger.Info("no localization labels found, skipping localization")

		return &localizeResult{}, nil
	}

	if source.IsHelm {
		helmResult, err := localize.ApplyHelmLocalization(source.valuesYAML, imageLocations)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.GetOCMResourceFailedReason, err.Error())

			return nil, err
		}

		return &localizeResult{
			AppliedLocalizations: helmResult.AppliedLocalizations,
			mergedValues:         helmResult.Values,
			imageBlockPaths:      helmResult.ImageBlockPaths,
		}, nil
	}

	// Kustomize: build images transformer list.
	kustomizeImages := localize.BuildKustomizeImages(imageLocations)

	var appliedLocs []v1alpha1.AppliedLocalization
	for _, img := range kustomizeImages {
		appliedLocs = append(appliedLocs, v1alpha1.AppliedLocalization{
			MatchKey:     img.Name,
			Applied:      img.NewName + ":" + img.NewTag,
			DigestPinned: img.Digest != "",
		})
	}

	return &localizeResult{
		AppliedLocalizations: appliedLocs,
		kustomizeImages:      kustomizeImages,
	}, nil
}

// collectImageLocations enumerates ociImage-typed resources in the root component version
// that carry a localization label, ensures a child Resource CR for each, and — once those
// CRs are Ready — returns the ImageLocation built from their resolved (relocated) access ref.
//
// When valueMatchKeys is non-nil (Helm), a labelled image is skipped unless its normalized
// match-key is present in that set, so only images referenced in values.yaml get a CR. A nil
// set (kustomize) disables the filter: every labelled image gets a CR (see ADR "Open questions").
//
// Image resources live in the root component, never in a referenced sub-component, so no
// referencePath is needed. Sub-components are only used for config resources (see mergeConfig).
func (r *Reconciler) collectImageLocations(ctx context.Context, dep *v1alpha1.OCMDeployment, comp *v1alpha1.Component, valueMatchKeys map[string]struct{}) ([]localize.ImageLocation, error) {
	if comp.Status.Component.RepositorySpec == nil {
		return nil, nil
	}

	// --- 1. Fetch the root component descriptor via the resolver. ---
	cfg, err := r.getConfiguration(ctx, dep)
	if err != nil {
		return nil, fmt.Errorf("get OCM config for image enumeration: %w", err)
	}

	pm, err := r.PluginManagerFor(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("plugin manager for image enumeration: %w", err)
	}

	repoSpec := &ocmruntime.Raw{}
	if err := repoSpec.UnmarshalJSON(comp.Status.Component.RepositorySpec.Raw); err != nil {
		return nil, fmt.Errorf("decode repository spec for image enumeration: %w", err)
	}

	desc, err := r.Resolver.GetComponentVersion(ctx, &resolution.Options{
		RepositorySpec: repoSpec,
		Configuration:  cfg,
		PluginManager:  pm,
	}, resolution.Verification{},
		comp.Status.Component.Component,
		comp.Status.Component.Version)
	if err != nil {
		return nil, fmt.Errorf("get component version for image enumeration: %w", err)
	}
	logger := log.FromContext(ctx)
	logger.Info("collectImageLocations: fetched component descriptor",
		"component", comp.Status.Component.Component,
		"version", comp.Status.Component.Version,
		"resourceCount", len(desc.Component.Resources))

	// --- 2. Find ociImage resources in the root component that carry the localization label. ---
	type imageEntry struct {
		resourceName string
		lv           localizationv1alpha1.Value
	}

	var entries []imageEntry

	for _, res := range desc.Component.Resources {
		resType, parseErr := ocmruntime.TypeFromString(res.Type)
		if parseErr != nil || !ociaccess.IsOCIImageType(resType) {
			continue
		}

		for _, lbl := range res.Labels {
			if lbl.Name != localizationv1alpha1.LabelName {
				continue
			}

			var lv localizationv1alpha1.Value
			if err := json.Unmarshal(lbl.Value, &lv); err != nil {
				continue
			}

			// Usage filter (Helm): skip labels whose match-key does not appear in values.yaml,
			// so unreferenced labelled images do not spawn a Resource CR + verification.
			// valueMatchKeys is nil for kustomize (name-matching happens engine-side) — no filter.
			if valueMatchKeys != nil {
				key, kerr := localize.NormalizeMatchKey(&lv)
				if _, present := valueMatchKeys[key]; kerr != nil || !present {
					break
				}
			}

			entries = append(entries, imageEntry{resourceName: res.Name, lv: lv})

			break // only one localization label per resource
		}
	}
	logger.Info("collectImageLocations: found image entries", "count", len(entries))

	// Prune stale image Resource CRs before the early-return so that a version change
	// that drops all image resources still cleans up (desiredCount=0 deletes all).
	pruneErr := r.pruneImageResourceCRs(ctx, dep, len(entries))
	if pruneErr != nil {
		return nil, fmt.Errorf("pruning stale image resource CRs: %w", pruneErr)
	}

	if len(entries) == 0 {
		return nil, nil
	}

	// --- 3. Ensure a child Resource CR for each image resource; collect resolved refs. ---
	var locations []localize.ImageLocation
	allReady := true

	for i, entry := range entries {
		crName := fmt.Sprintf("%s-img-%d", dep.Name, i)
		ref := v1alpha1.OCMResourceRef{
			ByReference: v1alpha1.ResourceReference{
				Resource: ocmruntime.Identity{"name": entry.resourceName},
				// No ReferencePath — image resources are in the root component.
			},
			VerificationPolicy: dep.Spec.DeployResource.VerificationPolicy,
		}

		imgRes, err := r.ensureResourceCR(ctx, dep, comp, crName, ref)
		if err != nil {
			return nil, fmt.Errorf("ensure image resource CR %s: %w", crName, err)
		}

		if imgRes == nil {
			// CR created or not yet ready — need another reconcile.
			allReady = false

			continue
		}

		// Build the ImageLocation from the resolved (relocated) access ref in status.
		resolvedRef := extractImageRef(imgRes)
		if resolvedRef == "" {
			allReady = false

			continue
		}

		reg, path, tag, resolvedDigest := parseRefComponents(resolvedRef)

		resolvedRepo := reg + "/" + path
		if reg == "" {
			resolvedRepo = path
		}

		matchKey, err := localize.NormalizeMatchKey(&entry.lv)
		if err != nil {
			continue
		}

		// Prefer the post-transfer digest from the resolved Resource status; fall back
		// to the label MatchKey.Digest (may be empty for tag-only references until
		// transfer resolves it).
		digest := resolvedDigest
		if digest == "" {
			digest = entry.lv.MatchKey.Digest
		}

		locations = append(locations, localize.ImageLocation{
			Repository: resolvedRepo,
			Tag:        tag,
			Digest:     digest,
			MatchKey:   matchKey,
			LabelValue: &entry.lv,
		})
		logger.Info("collectImageLocations: resolved image location",
			"resource", entry.resourceName,
			"resolvedRef", resolvedRef,
			"matchKey", matchKey,
			"repository", resolvedRepo,
			"tag", tag)
	}

	if !allReady {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResolutionInProgress, "waiting for image resource CRs to resolve")

		return nil, fmt.Errorf("image resource CRs not yet ready")
	}

	return locations, nil
}
