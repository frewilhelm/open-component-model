package deployment

// config.go implements the config-merge pipeline stage for OCMDeployment.

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/yaml"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/status"
)

// mergeConfig downloads config resources, then deep-merges them + inline config on top of the
// localized Helm values.
func (r *Reconciler) mergeConfig(ctx context.Context, dep *v1alpha1.OCMDeployment, comp *v1alpha1.Component, source *deploySource, locResult *localizeResult) error {
	if !source.IsHelm {
		patches, err := r.collectKustomizePatches(ctx, dep, comp)
		if err != nil {
			return err
		}

		locResult.kustomizePatches = patches

		return nil
	}

	var configLayers [][]byte

	for i, ref := range dep.Spec.ConfigResources {
		configRes, err := r.ensureConfigResource(ctx, dep, comp, i, ref)
		if err != nil {
			return fmt.Errorf("config resource %d: %w", i, err)
		}

		if configRes == nil {
			// Not yet ready — requeue implicitly.
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResolutionInProgress, fmt.Sprintf("config resource %d not yet ready", i))

			return fmt.Errorf("config resource %d not yet ready", i)
		}

		blob, err := r.downloadBlob(ctx, dep, configRes)
		if err != nil {
			return fmt.Errorf("config resource %d download: %w", i, err)
		}

		configLayers = append(configLayers, blob)
	}

	var inlineConfig []byte
	if dep.Spec.Config != nil {
		inlineConfig = dep.Spec.Config.Raw
	}

	merged, err := localize.MergeHelmConfig(locResult.mergedValues, configLayers, inlineConfig, locResult.imageBlockPaths)
	if err != nil {
		return fmt.Errorf("failed to merge helm config: %w", err)
	}

	locResult.mergedValues = merged

	return nil
}

// collectKustomizePatches downloads all config resources and the inline config blob,
// parses each as {"patches": [{patch, target}, …]}, and returns the ordered list:
// configResources entries first (spec order), inline config entries last.
// A source with content but no valid "patches" list is rejected as UnsupportedConfiguration.
func (r *Reconciler) collectKustomizePatches(ctx context.Context, dep *v1alpha1.OCMDeployment, comp *v1alpha1.Component) ([]map[string]any, error) {
	if len(dep.Spec.ConfigResources) == 0 && dep.Spec.Config == nil {
		return nil, nil
	}

	type patchDoc struct {
		Patches []map[string]any `json:"patches"`
	}

	var result []map[string]any

	for i, ref := range dep.Spec.ConfigResources {
		configRes, err := r.ensureConfigResource(ctx, dep, comp, i, ref)
		if err != nil {
			return nil, fmt.Errorf("config resource %d: %w", i, err)
		}

		if configRes == nil {
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResolutionInProgress, fmt.Sprintf("config resource %d not yet ready", i))

			return nil, fmt.Errorf("config resource %d not yet ready", i)
		}

		blob, err := r.downloadBlob(ctx, dep, configRes)
		if err != nil {
			return nil, fmt.Errorf("config resource %d download: %w", i, err)
		}

		var doc patchDoc
		if err := yaml.Unmarshal(blob, &doc); err != nil {
			msg := fmt.Sprintf("config resource %d is not valid YAML", i)
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.OCMDeploymentUnsupportedConfigurationReason, msg)

			return nil, fmt.Errorf("%s: %w", msg, err)
		}
		if len(doc.Patches) == 0 {
			msg := fmt.Sprintf("config resource %d must contain a 'patches' list", i)
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.OCMDeploymentUnsupportedConfigurationReason, msg)

			return nil, errors.New(msg)
		}

		result = append(result, doc.Patches...)
	}

	if dep.Spec.Config != nil {
		var doc patchDoc
		if err := yaml.Unmarshal(dep.Spec.Config.Raw, &doc); err != nil {
			msg := "spec.config is not valid YAML for kustomize deployments"
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.OCMDeploymentUnsupportedConfigurationReason, msg)

			return nil, fmt.Errorf("%s: %w", msg, err)
		}
		if len(doc.Patches) == 0 {
			msg := "spec.config must contain a 'patches' list for kustomize deployments"
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.OCMDeploymentUnsupportedConfigurationReason, msg)

			return nil, errors.New(msg)
		}

		result = append(result, doc.Patches...)
	}

	return result, nil
}
