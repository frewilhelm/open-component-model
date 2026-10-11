package render

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
)

func helmInput(engine v1alpha1.DeployerEngine) Input {
	return Input{
		Name:      "app",
		Namespace: "apps",
		Deployer:  engine,
		Kind:      ArtifactHelm,
		Source: Source{
			Registry: "localhost:5000", Repository: "stefanprodan/charts/podinfo",
			Tag: "6.9.1", Digest: "sha256:565d310746f1fa4be7f93ba7965bb393153a2d57a15cfe5befc909b790a73f8a",
			Insecure: true,
		},
		TargetNamespace: "podinfo",
		CreateNamespace: true,
		ReleaseName:     "app",
		Interval:        time.Minute,
		Values:          map[string]any{"replicaCount": int64(2), "ui": map[string]any{"color": "#326ce5"}},
	}
}

func kustomizeInput(engine v1alpha1.DeployerEngine) Input {
	return Input{
		Name:      "app",
		Namespace: "apps",
		Deployer:  engine,
		Kind:      ArtifactKustomize,
		Source: Source{
			Registry: "localhost:5000", Repository: "examples/deployment/kustomize",
			Tag: "1.0.0", Digest: "sha256:aaaa", Insecure: true,
		},
		TargetNamespace: "podinfo",
		CreateNamespace: false,
		Interval:        2 * time.Minute,
		Images: []localize.KustomizeImage{
			{Name: "ghcr.io/stefanprodan/podinfo", NewName: "localhost:5000/stefanprodan/podinfo", NewTag: "6.9.1", Digest: "sha256:262578"},
		},
	}
}

func str(t *testing.T, u *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedString(u.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "field %v not found", fields)
	return v
}

func TestFluxHelm(t *testing.T) {
	r := require.New(t)
	out, err := Render(helmInput(v1alpha1.DeployerEngineFluxCD))
	r.NoError(err)
	r.Len(out, 2)

	source := out[0]
	r.Equal("source.toolkit.fluxcd.io/v1", source.GetAPIVersion())
	r.Equal("OCIRepository", source.GetKind())
	r.Equal("app", source.GetName())
	r.Equal("apps", source.GetNamespace())
	r.Equal("oci://localhost:5000/stefanprodan/charts/podinfo", str(t, source, "spec", "url"))
	r.Equal("sha256:565d310746f1fa4be7f93ba7965bb393153a2d57a15cfe5befc909b790a73f8a", str(t, source, "spec", "ref", "digest"))
	insecure, _, _ := unstructured.NestedBool(source.Object, "spec", "insecure")
	r.True(insecure)
	r.Equal(helmChartLayerMediaType, str(t, source, "spec", "layerSelector", "mediaType"))

	release := out[1]
	r.Equal("helm.toolkit.fluxcd.io/v2", release.GetAPIVersion())
	r.Equal("HelmRelease", release.GetKind())
	r.Equal("app", str(t, release, "spec", "releaseName"))
	r.Equal("podinfo", str(t, release, "spec", "targetNamespace"))
	r.Equal("OCIRepository", str(t, release, "spec", "chartRef", "kind"))
	r.Equal("app", str(t, release, "spec", "chartRef", "name"))
	createNS, _, _ := unstructured.NestedBool(release.Object, "spec", "install", "createNamespace")
	r.True(createNS)
	values, found, err := unstructured.NestedMap(release.Object, "spec", "values")
	r.NoError(err)
	r.True(found)
	r.Equal("#326ce5", values["ui"].(map[string]any)["color"])
}

func TestFluxKustomize(t *testing.T) {
	r := require.New(t)
	out, err := Render(kustomizeInput(v1alpha1.DeployerEngineFluxCD))
	r.NoError(err)
	r.Len(out, 2)

	source := out[0]
	r.Equal("OCIRepository", source.GetKind())
	_, found, _ := unstructured.NestedMap(source.Object, "spec", "layerSelector")
	r.False(found, "kustomize source carries no helm layer selector")

	k := out[1]
	r.Equal("kustomize.toolkit.fluxcd.io/v1", k.GetAPIVersion())
	r.Equal("Kustomization", k.GetKind())
	r.Equal("podinfo", str(t, k, "spec", "targetNamespace"))
	r.Equal("OCIRepository", str(t, k, "spec", "sourceRef", "kind"))
	prune, _, _ := unstructured.NestedBool(k.Object, "spec", "prune")
	r.True(prune)
	images, found, err := unstructured.NestedSlice(k.Object, "spec", "images")
	r.NoError(err)
	r.True(found)
	r.Len(images, 1)
	img := images[0].(map[string]any)
	r.Equal("ghcr.io/stefanprodan/podinfo", img["name"])
	r.Equal("localhost:5000/stefanprodan/podinfo", img["newName"])
	r.Equal("6.9.1", img["newTag"])
	r.Equal("sha256:262578", img["digest"])
}

func TestArgoHelm(t *testing.T) {
	r := require.New(t)
	out, err := Render(helmInput(v1alpha1.DeployerEngineArgoCD))
	r.NoError(err)
	r.Len(out, 1)

	app := out[0]
	r.Equal("argoproj.io/v1alpha1", app.GetAPIVersion())
	r.Equal("Application", app.GetKind())
	// OCI Helm: repoURL is the base path, chart is the last segment.
	r.Equal("oci://localhost:5000/stefanprodan/charts", str(t, app, "spec", "source", "repoURL"))
	r.Equal("podinfo", str(t, app, "spec", "source", "chart"))
	r.Equal("6.9.1", str(t, app, "spec", "source", "targetRevision"))
	r.Equal("app", str(t, app, "spec", "source", "helm", "releaseName"))
	r.Equal("podinfo", str(t, app, "spec", "destination", "namespace"))
	valuesObject, found, err := unstructured.NestedMap(app.Object, "spec", "source", "helm", "valuesObject")
	r.NoError(err)
	r.True(found)
	r.Equal("#326ce5", valuesObject["ui"].(map[string]any)["color"])
	syncOpts, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	r.Equal([]string{"CreateNamespace=true"}, syncOpts)
	finalizers := app.GetFinalizers()
	r.Equal([]string{"resources-finalizer.argocd.argoproj.io"}, finalizers)
}

func TestArgoKustomize(t *testing.T) {
	r := require.New(t)
	out, err := Render(kustomizeInput(v1alpha1.DeployerEngineArgoCD))
	r.NoError(err)
	r.Len(out, 1)

	app := out[0]
	r.Equal("Application", app.GetKind())
	r.Equal("oci://localhost:5000/examples/deployment/kustomize", str(t, app, "spec", "source", "repoURL"))
	images, found, err := unstructured.NestedStringSlice(app.Object, "spec", "source", "kustomize", "images")
	r.NoError(err)
	r.True(found)
	r.Equal([]string{"ghcr.io/stefanprodan/podinfo=localhost:5000/stefanprodan/podinfo:6.9.1@sha256:262578"}, images)
	syncOpts, _, _ := unstructured.NestedStringSlice(app.Object, "spec", "syncPolicy", "syncOptions")
	r.Equal([]string{"CreateNamespace=false"}, syncOpts)
}

func TestRenderUnsupportedCombination(t *testing.T) {
	r := require.New(t)
	_, err := Render(Input{Deployer: "Spinnaker", Kind: ArtifactHelm})
	r.ErrorContains(err, "unsupported deployer/artifact combination")
}
