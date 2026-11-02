package deployment

// pipeline.go wires the reconcile pipeline stages together.
// Each stage is implemented in its own file; this file only holds
// shared types used across stages.

import (
	"bytes"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/render"
	ocmruntime "ocm.software/open-component-model/bindings/go/runtime"
)

// deploySource is the pipeline carrier: the resolved render input plus the raw Helm values that only
// the localize stage reads. download produces it (and caches it by digest); renderEngine copies the
// embedded RenderInput and fills the per-reconcile fields before handing it to render.Render.
type deploySource struct {
	render.RenderInput
	valuesYAML []byte // raw Helm values.yaml; nil for kustomize
}

// localizeResult carries the output of the localization step.
type localizeResult struct {
	// AppliedLocalizations is the list of substitutions performed.
	AppliedLocalizations []v1alpha1.AppliedLocalization
	// mergedValues is the Helm values tree after localization substitutions
	// (ready for config merge). nil for kustomize.
	mergedValues map[string]any
	// kustomizeImages is the list of image transformer entries for kustomize.
	kustomizeImages []localize.KustomizeImage
	// kustomizePatches are the engine patch entries ({patch, target}) collected from
	// config/configResources for a kustomize deploy. nil for Helm.
	kustomizePatches []map[string]any
	// imageBlockPaths are the values-tree paths of image blocks that localization rewrote;
	// config merge treats these subtrees atomically (Helm only).
	imageBlockPaths [][]string
}

// resStatusToDescriptorResource converts a Resource's status info back into a
// descriptor.Resource suitable for passing to a resource plugin.
func resStatusToDescriptorResource(res *v1alpha1.Resource) *descriptor.Resource {
	if res.Status.Resource == nil {
		return &descriptor.Resource{}
	}

	ri := res.Status.Resource

	var dig *descriptor.Digest
	if ri.Digest != nil {
		dig = descriptor.ConvertFromV2Digest(ri.Digest)
	}

	// Decode the access spec from the raw JSON stored in the Resource status.
	accessRaw := &ocmruntime.Raw{}

	var access ocmruntime.Typed = accessRaw
	err := ocmruntime.NewScheme(ocmruntime.WithAllowUnknown()).Decode(
		bytes.NewReader(ri.Access.Raw), accessRaw,
	)
	if err != nil {
		// Fall back to an empty Raw so the resource plugin can report a clearer error downstream.
		access = accessRaw
	}

	return &descriptor.Resource{
		Name:    ri.Name,
		Version: ri.Version,
		Access:  access,
		Digest:  dig,
	}
}

// renderEngine renders the engine objects for the OCMDeployment.
func (r *Reconciler) renderEngine(dep *v1alpha1.OCMDeployment, deployRes *v1alpha1.Resource, source *deploySource, locResult *localizeResult) ([]*unstructured.Unstructured, error) {
	setApplySetGVK(dep)

	in := source.RenderInput // copy; the cached *deploySource is digest-keyed and shared across objects, so it stays read-only
	in.Dep = dep
	in.MergedValues = locResult.mergedValues
	in.KustomizeImages = locResult.kustomizeImages
	in.KustomizePatches = locResult.kustomizePatches

	return render.Render(in)
}
