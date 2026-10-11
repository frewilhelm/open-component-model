// Package render turns resolved deploy inputs into the engine objects a
// deployer applies: Flux (OCIRepository + HelmRelease/Kustomization) or Argo
// (Application). The functions are pure: given the source coordinates, the
// merged Helm values or kustomize image overrides, and the target, they return
// unstructured objects ready to apply. No Kubernetes client, no I/O.
package render

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
)

const (
	fluxSourceAPIVersion    = "source.toolkit.fluxcd.io/v1"
	fluxHelmAPIVersion      = "helm.toolkit.fluxcd.io/v2"
	fluxKustomizeAPIVersion = "kustomize.toolkit.fluxcd.io/v1"
	argoAPIVersion          = "argoproj.io/v1alpha1"

	// helmChartLayerMediaType selects the Helm chart layer in an OCI artifact.
	helmChartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// ArtifactKind is the deploy artifact flavor, derived from the OCM resource type.
type ArtifactKind string

const (
	ArtifactHelm      ArtifactKind = "helm"
	ArtifactKustomize ArtifactKind = "kustomize"
)

// Source is the resolved OCI location of the deploy artifact (chart or kustomize
// layout), as toOCI reports it from the resource access.
type Source struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
	Insecure   bool
}

// Input is everything a render needs. Name/Namespace are the OCMDeployment's;
// the generated objects are created in that namespace and share that name.
type Input struct {
	Name            string
	Namespace       string
	Deployer        v1alpha1.DeployerEngine
	Kind            ArtifactKind
	Source          Source
	TargetNamespace string
	CreateNamespace bool
	ReleaseName     string
	Interval        time.Duration
	Values          map[string]any
	Images          []localize.KustomizeImage
}

// Render dispatches on deployer and artifact kind.
func Render(in Input) ([]*unstructured.Unstructured, error) {
	switch in.Deployer {
	case v1alpha1.DeployerEngineFluxCD:
		switch in.Kind {
		case ArtifactHelm:
			return fluxHelm(in), nil
		case ArtifactKustomize:
			return fluxKustomize(in), nil
		}
	case v1alpha1.DeployerEngineArgoCD:
		switch in.Kind {
		case ArtifactHelm:
			return argoHelm(in), nil
		case ArtifactKustomize:
			return argoKustomize(in), nil
		}
	}
	return nil, fmt.Errorf("unsupported deployer/artifact combination: %q/%q", in.Deployer, in.Kind)
}

// ociURL is the full oci:// url for a Flux source.
func (s Source) ociURL() string {
	return "oci://" + s.Registry + "/" + s.Repository
}

// sourceRef is the digest when present (immutable), else the tag.
func (s Source) ref() map[string]any {
	if s.Digest != "" {
		return map[string]any{"digest": s.Digest}
	}
	return map[string]any{"tag": s.Tag}
}

func obj(apiVersion, kind, name, namespace string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       spec,
	}}
	return u
}

func (in Input) interval() string { return in.Interval.String() }

func fluxOCIRepository(in Input, withHelmLayer bool) *unstructured.Unstructured {
	spec := map[string]any{
		"interval": in.interval(),
		"insecure": in.Source.Insecure,
		"url":      in.Source.ociURL(),
		"ref":      in.Source.ref(),
	}
	if withHelmLayer {
		spec["layerSelector"] = map[string]any{
			"mediaType": helmChartLayerMediaType,
			"operation": "copy",
		}
	}
	return obj(fluxSourceAPIVersion, "OCIRepository", in.Name, in.Namespace, spec)
}

func fluxHelm(in Input) []*unstructured.Unstructured {
	source := fluxOCIRepository(in, true)
	spec := map[string]any{
		"releaseName":     in.ReleaseName,
		"interval":        in.interval(),
		"targetNamespace": in.TargetNamespace,
		"chartRef": map[string]any{
			"kind":      "OCIRepository",
			"name":      in.Name,
			"namespace": in.Namespace,
		},
		"install": map[string]any{"createNamespace": in.CreateNamespace},
	}
	if len(in.Values) > 0 {
		spec["values"] = in.Values
	}
	release := obj(fluxHelmAPIVersion, "HelmRelease", in.Name, in.Namespace, spec)
	return []*unstructured.Unstructured{source, release}
}

func fluxKustomize(in Input) []*unstructured.Unstructured {
	source := fluxOCIRepository(in, false)
	spec := map[string]any{
		"interval":        in.interval(),
		"targetNamespace": in.TargetNamespace,
		"prune":           true,
		"path":            ".",
		"sourceRef": map[string]any{
			"kind": "OCIRepository",
			"name": in.Name,
		},
	}
	if imgs := fluxImages(in.Images); len(imgs) > 0 {
		spec["images"] = imgs
	}
	kustomization := obj(fluxKustomizeAPIVersion, "Kustomization", in.Name, in.Namespace, spec)
	return []*unstructured.Unstructured{source, kustomization}
}

// fluxImages renders the kustomize images transformer entries Flux consumes.
func fluxImages(images []localize.KustomizeImage) []any {
	out := make([]any, 0, len(images))
	for _, img := range images {
		entry := map[string]any{"name": img.Name}
		if img.NewName != "" {
			entry["newName"] = img.NewName
		}
		if img.NewTag != "" {
			entry["newTag"] = img.NewTag
		}
		if img.Digest != "" {
			entry["digest"] = img.Digest
		}
		out = append(out, entry)
	}
	return out
}

func argoApplication(in Input, source map[string]any) *unstructured.Unstructured {
	spec := map[string]any{
		"project": "default",
		"source":  source,
		"destination": map[string]any{
			"server":    "https://kubernetes.default.svc",
			"namespace": in.TargetNamespace,
		},
		"syncPolicy": map[string]any{
			"automated": map[string]any{"prune": true, "selfHeal": true},
			"syncOptions": []any{
				fmt.Sprintf("CreateNamespace=%t", in.CreateNamespace),
			},
		},
	}
	u := obj(argoAPIVersion, "Application", in.Name, in.Namespace, spec)
	_ = unstructured.SetNestedStringSlice(u.Object, []string{"resources-finalizer.argocd.argoproj.io"}, "metadata", "finalizers")
	return u
}

func argoHelm(in Input) []*unstructured.Unstructured {
	base, chart := splitChart(in.Source.Repository)
	helm := map[string]any{"releaseName": in.ReleaseName}
	if len(in.Values) > 0 {
		helm["valuesObject"] = in.Values
	}
	source := map[string]any{
		"repoURL":        "oci://" + in.Source.Registry + "/" + base,
		"chart":          chart,
		"targetRevision": in.Source.Tag,
		"helm":           helm,
	}
	return []*unstructured.Unstructured{argoApplication(in, source)}
}

func argoKustomize(in Input) []*unstructured.Unstructured {
	source := map[string]any{
		"repoURL":        in.Source.ociURL(),
		"targetRevision": in.Source.Tag,
		"path":           ".",
	}
	if imgs := argoImages(in.Images); len(imgs) > 0 {
		source["kustomize"] = map[string]any{"images": imgs}
	}
	return []*unstructured.Unstructured{argoApplication(in, source)}
}

// argoImages renders kustomize image overrides in Argo's string form:
// original=newName:newTag@digest.
func argoImages(images []localize.KustomizeImage) []any {
	out := make([]any, 0, len(images))
	for _, img := range images {
		var b strings.Builder
		b.WriteString(img.Name)
		b.WriteString("=")
		b.WriteString(img.NewName)
		if img.NewTag != "" {
			b.WriteString(":")
			b.WriteString(img.NewTag)
		}
		if img.Digest != "" {
			b.WriteString("@")
			b.WriteString(img.Digest)
		}
		out = append(out, b.String())
	}
	return out
}

// splitChart separates an OCI repository into the base path and the chart name
// (its last segment), as Argo's OCI Helm source expects.
func splitChart(repo string) (base, chart string) {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[:i], repo[i+1:]
	}
	return "", repo
}
