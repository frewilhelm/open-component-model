package render

// render.go builds unstructured engine objects (Flux HelmRelease/OCIRepository/Kustomization,
// Argo Application, Namespace) for OCMDeployment without importing Flux or Argo Go modules.

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
)

// API version / kind constants — no Flux/Argo imports needed.
const (
	fluxSourceAPIVersion    = "source.toolkit.fluxcd.io/v1"
	fluxHelmAPIVersion      = "helm.toolkit.fluxcd.io/v2"
	fluxKustomizeAPIVersion = "kustomize.toolkit.fluxcd.io/v1"
	argoAPIVersion          = "argoproj.io/v1alpha1"
	argoAppFinalizer        = "resources-finalizer.argocd.argoproj.io"

	kindOCIRepository  = "OCIRepository"
	kindHelmRelease    = "HelmRelease"
	kindHelmRepository = "HelmRepository"
	kindKustomization  = "Kustomization"
	kindApplication    = "Application"
	kindNamespace      = "Namespace"
)

// RenderInput carries all the resolved data needed to render engine objects.
type RenderInput struct {
	Dep      *v1alpha1.OCMDeployment
	OCIURL   string // oci://<registry>/<repo>
	Digest   string // sha256:...
	IsHelm   bool
	Insecure bool // true when the OCI registry uses plain HTTP
	// MergedValues is the final Helm values map (after localize + config merge). nil for kustomize.
	MergedValues map[string]any
	// KustomizeImages is the list of image transformer entries. nil for Helm.
	KustomizeImages []localize.KustomizeImage
	// KustomizePatches is the list of pass-through patch entries ({patch, target}) for kustomize. nil for Helm.
	KustomizePatches []map[string]any
	// HelmRepo is true when the deploy resource is a Helm/v1 HTTP-repository access; the render path
	// emits an HTTP Helm source (Flux HelmRepository / Argo HTTP-Helm) instead of an OCIRepository.
	// When set, OCIURL/Digest/Insecure are unused.
	HelmRepo bool
	// HelmRepoURL is the HTTP(S) Helm repository URL. Set only when HelmRepo.
	HelmRepoURL string
	// HelmChartName is the chart name within the repository. Set only when HelmRepo.
	HelmChartName string
	// HelmChartVersion is the resolved chart version. Set only when HelmRepo.
	HelmChartVersion string
}

// Render produces the set of unstructured engine objects for the OCMDeployment.
func Render(input RenderInput) ([]*unstructured.Unstructured, error) {
	dep := input.Dep
	cascading := dep.Spec.DeletionPolicy != v1alpha1.DeletionPolicyOrphan

	var objects []*unstructured.Unstructured

	// Render the target Namespace if createNamespace is requested.
	if ns := targetNamespace(dep); ns != "" {
		if dep.Spec.Namespace != nil && boolPtrTrue(dep.Spec.Namespace.CreateNamespace) {
			objects = append(objects, renderNamespace(ns))
		}
	}

	switch dep.Spec.Deployer {
	case v1alpha1.DeployerEngineFluxCD:
		if input.IsHelm {
			src, rel, err := renderFluxHelm(dep, input, cascading)
			if err != nil {
				return nil, err
			}

			objects = append(objects, src, rel)
		} else {
			src, ks, err := renderFluxKustomize(dep, input, cascading)
			if err != nil {
				return nil, err
			}

			objects = append(objects, src, ks)
		}
	case v1alpha1.DeployerEngineArgoCD:
		app, err := renderArgoApp(dep, input, cascading)
		if err != nil {
			return nil, err
		}

		objects = append(objects, app)
	default:
		return nil, fmt.Errorf("unknown deployer engine: %s", dep.Spec.Deployer)
	}

	return objects, nil
}

// renderNamespace produces a Namespace object as an applyset member.
func renderNamespace(name string) *unstructured.Unstructured {
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind(kindNamespace)
	ns.SetName(name)

	return ns
}

// renderFluxHelm produces a source + HelmRelease for the Flux Helm engine.
// When input.HelmRepo is true it emits a HelmRepository source (HTTP Helm repo); otherwise it emits
// an OCIRepository source (OCI Helm chart).
func renderFluxHelm(dep *v1alpha1.OCMDeployment, input RenderInput, cascading bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	ns := dep.Namespace

	var src *unstructured.Unstructured
	var chartSpec map[string]any

	if input.HelmRepo {
		// --- HTTP Helm repository path ---
		srcSpec := map[string]any{
			"url":      input.HelmRepoURL,
			"interval": dep.Spec.Source.Interval.Duration.String(),
		}
		if dep.Spec.Flux != nil && dep.Spec.Flux.SourceSecretRef != nil {
			srcSpec["secretRef"] = map[string]any{"name": dep.Spec.Flux.SourceSecretRef.Name}
		}

		src = newUnstructured(fluxSourceAPIVersion, kindHelmRepository, ns, dep.Name)
		src.Object["spec"] = srcSpec
		if dep.Spec.Suspend {
			mustSetNestedField(src, true, "spec", "suspend")
		}

		// HelmRelease references the HelmRepository via spec.chart.spec (not chartRef).
		chartSpec = map[string]any{
			"chart":   input.HelmChartName,
			"version": input.HelmChartVersion,
			"sourceRef": map[string]any{
				"kind":      kindHelmRepository,
				"name":      dep.Name,
				"namespace": ns,
			},
		}
	} else {
		// --- OCI repository path ---
		ociRepoName := dep.Name

		srcSpec := map[string]any{
			"url":      input.OCIURL,
			"interval": dep.Spec.Source.Interval.Duration.String(),
			"ref": map[string]any{
				"digest": input.Digest,
			},
		}
		if dep.Spec.Flux != nil && dep.Spec.Flux.SourceSecretRef != nil {
			srcSpec["secretRef"] = map[string]any{"name": dep.Spec.Flux.SourceSecretRef.Name}
		}
		if isInsecureOCI(input) {
			srcSpec["insecure"] = true
		}

		src = newUnstructured(fluxSourceAPIVersion, kindOCIRepository, ns, ociRepoName)
		src.Object["spec"] = srcSpec
		if dep.Spec.Suspend {
			mustSetNestedField(src, true, "spec", "suspend")
		}
		// chartRef is set below via the OCI path — no chartSpec for OCI.
		chartSpec = nil
	}

	// HelmRelease
	releaseName := dep.Spec.ReleaseName
	if releaseName == "" {
		releaseName = dep.Name
	}

	targetNS := targetNamespace(dep)

	relSpec := map[string]any{
		"interval":        dep.Spec.Interval.Duration.String(),
		"releaseName":     releaseName,
		"targetNamespace": targetNS,
	}

	if input.HelmRepo {
		// HTTP Helm: spec.chart.spec references the HelmRepository.
		relSpec["chart"] = map[string]any{
			"spec": chartSpec,
		}
	} else {
		// OCI Helm: spec.chartRef references the OCIRepository.
		relSpec["chartRef"] = map[string]any{
			"kind":      kindOCIRepository,
			"name":      dep.Name,
			"namespace": ns,
		}
	}

	if dep.Spec.Flux != nil && dep.Spec.Flux.ServiceAccountName != "" {
		relSpec["serviceAccountName"] = dep.Spec.Flux.ServiceAccountName
	}
	if len(input.MergedValues) > 0 {
		relSpec["values"] = input.MergedValues
	}

	helmRelease := newUnstructured(fluxHelmAPIVersion, kindHelmRelease, ns, dep.Name)
	helmRelease.Object["spec"] = relSpec
	if dep.Spec.Suspend {
		mustSetNestedField(helmRelease, true, "spec", "suspend")
	}

	_ = cascading // HelmRelease cascade is determined by its GC semantics, not this flag.

	return src, helmRelease, nil
}

// renderFluxKustomize produces an OCIRepository + Kustomization for the Flux Kustomize engine.
func renderFluxKustomize(dep *v1alpha1.OCMDeployment, input RenderInput, cascading bool) (*unstructured.Unstructured, *unstructured.Unstructured, error) {
	ns := dep.Namespace
	ociRepoName := dep.Name

	// OCIRepository
	srcSpec := map[string]any{
		"url":      input.OCIURL,
		"interval": dep.Spec.Source.Interval.Duration.String(),
		"ref": map[string]any{
			"digest": input.Digest,
		},
	}
	if dep.Spec.Flux != nil && dep.Spec.Flux.SourceSecretRef != nil {
		srcSpec["secretRef"] = map[string]any{"name": dep.Spec.Flux.SourceSecretRef.Name}
	}

	if isInsecureOCI(input) {
		srcSpec["insecure"] = true
	}

	src := newUnstructured(fluxSourceAPIVersion, kindOCIRepository, ns, ociRepoName)

	src.Object["spec"] = srcSpec
	if dep.Spec.Suspend {
		mustSetNestedField(src, true, "spec", "suspend")
	}

	// Kustomization
	targetNS := targetNamespace(dep)

	ksSpec := map[string]any{
		"interval": dep.Spec.Interval.Duration.String(),
		"sourceRef": map[string]any{
			"kind":      kindOCIRepository,
			"name":      ociRepoName,
			"namespace": ns,
		},
		"targetNamespace": targetNS,
		"path":            ".",
		"prune":           cascading,
	}
	if dep.Spec.Flux != nil && dep.Spec.Flux.ServiceAccountName != "" {
		ksSpec["serviceAccountName"] = dep.Spec.Flux.ServiceAccountName
	}

	if len(input.KustomizeImages) > 0 {
		ksSpec["images"] = kustomizeImagesToUnstructured(input.KustomizeImages)
	}

	if len(input.KustomizePatches) > 0 {
		ksSpec["patches"] = kustomizePatchesToUnstructured(input.KustomizePatches)
	}

	if !cascading {
		ksSpec["commonMetadata"] = map[string]any{
			"annotations": map[string]any{
				"kustomize.toolkit.fluxcd.io/prune": "disabled",
			},
		}
	}

	ks := newUnstructured(fluxKustomizeAPIVersion, kindKustomization, ns, dep.Name)

	ks.Object["spec"] = ksSpec
	if dep.Spec.Suspend {
		mustSetNestedField(ks, true, "spec", "suspend")
	}

	return src, ks, nil
}

// renderArgoApp produces an Argo CD Application for Helm or Kustomize.
func renderArgoApp(dep *v1alpha1.OCMDeployment, input RenderInput, cascading bool) (*unstructured.Unstructured, error) {
	targetNS := targetNamespace(dep)

	project := "default"
	if dep.Spec.Argo != nil && dep.Spec.Argo.Project != "" {
		project = dep.Spec.Argo.Project
	}

	// Build the source spec.
	var source map[string]any
	switch {
	case input.IsHelm:
		releaseName := dep.Spec.ReleaseName
		if releaseName == "" {
			releaseName = dep.Name
		}
		helmSpec := map[string]any{
			"releaseName": releaseName,
		}
		if len(input.MergedValues) > 0 {
			helmSpec["valuesObject"] = input.MergedValues
		}
		if input.HelmRepo {
			// HTTP Helm repository: use repoURL/chart/targetRevision directly.
			// dep.Spec.Argo.RepoCredentialsSecretRef is operator-managed — Argo resolves repo
			// credentials from a Secret in its own namespace out of band; there is no source-level
			// secret field on the Application spec, so we do not stamp it here.
			source = map[string]any{
				"repoURL":        input.HelmRepoURL,
				"chart":          input.HelmChartName,
				"targetRevision": input.HelmChartVersion,
				"helm":           helmSpec,
			}
		} else {
			// OCI Helm: repoURL = OCI URL, chart = last path segment, targetRevision = digest.
			source = map[string]any{
				"repoURL":        input.OCIURL,
				"chart":          lastPathSegment(input.OCIURL),
				"targetRevision": input.Digest,
				"helm":           helmSpec,
			}
		}
	default:
		// Kustomize via OCI.
		source = map[string]any{
			"repoURL":        input.OCIURL,
			"targetRevision": input.Digest,
			"path":           ".",
		}
		ksMap := map[string]any{}
		if len(input.KustomizeImages) > 0 {
			ksMap["images"] = argoKustomizeImages(input.KustomizeImages)
		}

		if len(input.KustomizePatches) > 0 {
			ksMap["patches"] = kustomizePatchesToUnstructured(input.KustomizePatches)
		}

		if len(ksMap) > 0 {
			source["kustomize"] = ksMap
		}
	}

	appSpec := map[string]any{
		"project": project,
		"source":  source,
		"destination": map[string]any{
			"server":    "https://kubernetes.default.svc",
			"namespace": targetNS,
		},
		"syncPolicy": map[string]any{
			"automated": map[string]any{
				// enabled=false is Argo's suspend equivalent: it skips automated sync while
				// keeping prune/selfHeal configured (matches Flux spec.suspend semantics).
				"enabled":  !dep.Spec.Suspend,
				"prune":    true,
				"selfHeal": true,
			},
			// The target namespace is created and pruned uniformly via the SA-applied applyset
			// Namespace member (gated on spec.namespace.createNamespace), as for Flux — Argo's own
			// syncOptions CreateNamespace is deliberately not used, to keep a single SA-owned creator.
		},
	}

	app := newUnstructured(argoAPIVersion, kindApplication, dep.Namespace, dep.Name)
	app.Object["spec"] = appSpec

	// Apply the resources-finalizer for cascade-delete.
	if cascading {
		app.SetFinalizers([]string{argoAppFinalizer})
	}

	return app, nil
}

// --- helpers ---

func newUnstructured(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetNamespace(namespace)
	u.SetName(name)

	return u
}

func mustSetNestedField(u *unstructured.Unstructured, value any, fields ...string) {
	err := unstructured.SetNestedField(u.Object, value, fields...)
	if err != nil {
		panic(fmt.Sprintf("failed to set nested field %v: %v", fields, err))
	}
}

func targetNamespace(dep *v1alpha1.OCMDeployment) string {
	if dep.Spec.Namespace != nil && dep.Spec.Namespace.TargetNamespace != "" {
		return dep.Spec.Namespace.TargetNamespace
	}

	return dep.Namespace
}

func boolPtrTrue(b *bool) bool {
	return b == nil || *b
}

func isInsecureOCI(input RenderInput) bool {
	return input.Insecure
}

// lastPathSegment returns the last slash-separated segment of a "oci://registry/path/chart" URL.
func lastPathSegment(ociURL string) string {
	s := ociURL
	if idx := len("oci://"); len(s) > idx {
		s = s[idx:]
	}

	if idx := lastSlash(s); idx >= 0 {
		return s[idx+1:]
	}

	return s
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}

	return -1
}

func kustomizeImagesToUnstructured(images []localize.KustomizeImage) []any {
	result := make([]any, 0, len(images))
	for _, img := range images {
		m := map[string]any{"name": img.Name}
		if img.NewName != "" {
			m["newName"] = img.NewName
		}

		if img.NewTag != "" {
			m["newTag"] = img.NewTag
		}

		if img.Digest != "" {
			m["digest"] = img.Digest
		}

		result = append(result, m)
	}

	return result
}

// kustomizePatchesToUnstructured wraps patch entries into the []any type required by
// Unstructured.DeepCopy. Entries parsed by sigs.k8s.io/yaml already have
// map[string]interface{}/[]interface{}/scalar leaves, so a shallow []any wrap suffices.
func kustomizePatchesToUnstructured(patches []map[string]any) []any {
	result := make([]any, 0, len(patches))
	for _, p := range patches {
		result = append(result, p)
	}

	return result
}

func argoKustomizeImages(images []localize.KustomizeImage) []any {
	// Argo kustomize images are in the format "name=newName:newTag@digest".
	result := make([]any, 0, len(images))
	for _, img := range images {
		s := img.Name
		if img.NewName != "" {
			s += "=" + img.NewName
		}

		if img.NewTag != "" {
			s += ":" + img.NewTag
		}

		if img.Digest != "" {
			s += "@" + img.Digest
		}

		result = append(result, s)
	}

	return result
}

// MarshalValues marshals the merged values map to JSON for embedding in the rendered objects.
func MarshalValues(values map[string]any) ([]byte, error) {
	if len(values) == 0 {
		return nil, nil
	}

	return json.Marshal(values)
}

// DeployedObjectReferenceFromUnstructured converts an *unstructured.Unstructured to a DeployedObjectReference.
func DeployedObjectReferenceFromUnstructured(u *unstructured.Unstructured) v1alpha1.DeployedObjectReference {
	return v1alpha1.DeployedObjectReference{
		APIVersion: u.GetAPIVersion(),
		Kind:       u.GetKind(),
		Name:       u.GetName(),
		Namespace:  u.GetNamespace(),
		UID:        u.GetUID(),
	}
}
