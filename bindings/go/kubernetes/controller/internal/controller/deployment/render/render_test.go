package render

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/localize"
)

const (
	testOCIURL   = "oci://localhost:5000/stefanprodan/charts/podinfo"
	testDigest   = "sha256:565d310746f1fa4be7f93ba7965bb393153a2d57a15cfe5befc909b790a73f8a"
	testTargetNS = "podinfo"
)

// makeDep builds a minimal OCMDeployment for render tests.
func makeDep(engine v1alpha1.DeployerEngine, ns, name string, opts ...func(*v1alpha1.OCMDeployment)) *v1alpha1.OCMDeployment {
	dep := &v1alpha1.OCMDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.OCMDeploymentSpec{
			Source: v1alpha1.ComponentSource{
				RepositorySpec: &apiextensionsv1.JSON{Raw: []byte(`{"type":"OCIRegistry","baseUrl":"https://ghcr.io"}`)},
				Component:      "ocm.software/examples/deployment",
				Semver:         "1.0.0",
				Interval:       metav1.Duration{Duration: time.Minute},
			},
			DeployResource: v1alpha1.OCMResourceRef{
				ByReference: v1alpha1.ResourceReference{
					Resource: map[string]string{"name": "chart"},
				},
			},
			Deployer: engine,
			Namespace: &v1alpha1.NamespaceOptions{
				TargetNamespace: testTargetNS,
				CreateNamespace: new(false),
			},
			ReleaseName:    name,
			Interval:       metav1.Duration{Duration: time.Minute},
			DeletionPolicy: v1alpha1.DeletionPolicyBackground,
		},
	}
	for _, opt := range opts {
		opt(dep)
	}
	return dep
}

func withCreateNamespace(dep *v1alpha1.OCMDeployment) {
	dep.Spec.Namespace.CreateNamespace = new(true)
}

func withSuspend(dep *v1alpha1.OCMDeployment) {
	dep.Spec.Suspend = true
}

func withFluxSecretRef(name string) func(*v1alpha1.OCMDeployment) {
	return func(dep *v1alpha1.OCMDeployment) {
		dep.Spec.Flux = &v1alpha1.FluxOptions{
			SourceSecretRef: &corev1.LocalObjectReference{Name: name},
		}
	}
}

// str is a helper to read a nested string from an Unstructured object.
func str(t *testing.T, u *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedString(u.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "field %v not found in %s", fields, u.GetKind())
	return v
}

func TestRenderFluxHelm(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:          dep,
		OCIURL:       testOCIURL,
		Digest:       testDigest,
		IsHelm:       true,
		Insecure:     true,
		MergedValues: map[string]any{"ui": map[string]any{"color": "#326ce5"}},
	})
	r.NoError(err)
	r.Len(out, 2, "flux helm: OCIRepository + HelmRelease")

	src := out[0]
	r.Equal("source.toolkit.fluxcd.io/v1", src.GetAPIVersion())
	r.Equal("OCIRepository", src.GetKind())
	r.Equal("app", src.GetName())
	r.Equal("apps", src.GetNamespace())
	r.Equal(testOCIURL, str(t, src, "spec", "url"))
	// Regression guard: digest must be sha256:... not a bare hex.
	r.Equal(testDigest, str(t, src, "spec", "ref", "digest"))
	insecure, _, _ := unstructured.NestedBool(src.Object, "spec", "insecure")
	r.True(insecure, "Insecure=true must set spec.insecure")
	// No layerSelector assertion: render.go does not currently set it.

	release := out[1]
	r.Equal("helm.toolkit.fluxcd.io/v2", release.GetAPIVersion())
	r.Equal("HelmRelease", release.GetKind())
	r.Equal(testTargetNS, str(t, release, "spec", "targetNamespace"))
	r.Equal("OCIRepository", str(t, release, "spec", "chartRef", "kind"))
	r.Equal("app", str(t, release, "spec", "chartRef", "name"))
	color, _, _ := unstructured.NestedString(release.Object, "spec", "values", "ui", "color")
	r.Equal("#326ce5", color)
}

func TestRenderFluxHelmInsecureFalse(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:      dep,
		OCIURL:   testOCIURL,
		Digest:   testDigest,
		IsHelm:   true,
		Insecure: false,
	})
	r.NoError(err)
	r.Len(out, 2)
	src := out[0]
	insecure, found, _ := unstructured.NestedBool(src.Object, "spec", "insecure")
	r.True(!found || !insecure, "Insecure=false must not set spec.insecure=true")
}

func TestRenderFluxKustomize(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: "oci://localhost:5000/examples/deployment/kustomize",
		Digest: "sha256:aaaa",
		IsHelm: false,
		KustomizeImages: []localize.KustomizeImage{
			{Name: "ghcr.io/stefanprodan/podinfo", NewName: "localhost:5000/stefanprodan/podinfo", NewTag: "6.9.1", Digest: "sha256:262578"},
		},
	})
	r.NoError(err)
	r.Len(out, 2, "flux kustomize: OCIRepository + Kustomization")

	src := out[0]
	r.Equal("OCIRepository", src.GetKind())
	// No helm layer selector for kustomize.
	_, found, _ := unstructured.NestedMap(src.Object, "spec", "layerSelector")
	r.False(found, "kustomize OCIRepository must not have layerSelector")

	ks := out[1]
	r.Equal("kustomize.toolkit.fluxcd.io/v1", ks.GetAPIVersion())
	r.Equal("Kustomization", ks.GetKind())
	r.Equal(testTargetNS, str(t, ks, "spec", "targetNamespace"))
	r.Equal("OCIRepository", str(t, ks, "spec", "sourceRef", "kind"))
	prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune")
	r.True(prune)
	// spec.images is []any (DeepCopy-safe); each element is map[string]any.
	imgsRaw, _, _ := unstructured.NestedSlice(ks.Object, "spec", "images")
	r.Len(imgsRaw, 1)
	img, ok := imgsRaw[0].(map[string]any)
	r.True(ok, "image entry must be map[string]any")
	r.Equal("ghcr.io/stefanprodan/podinfo", img["name"])
	r.Equal("localhost:5000/stefanprodan/podinfo", img["newName"])
	r.Equal("6.9.1", img["newTag"])
	r.Equal("sha256:262578", img["digest"])
}

func TestRenderFluxKustomizeCreateNamespace(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app", withCreateNamespace)
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: "oci://localhost:5000/examples/kustomize",
		Digest: "sha256:aaaa",
		IsHelm: false,
	})
	r.NoError(err)
	r.Len(out, 3, "createNamespace=true renders Namespace + OCIRepository + Kustomization")
	r.Equal("Namespace", out[0].GetKind())
	r.Equal(testTargetNS, out[0].GetName())
}

func TestRenderArgoHelm(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:          dep,
		OCIURL:       testOCIURL,
		Digest:       testDigest,
		IsHelm:       true,
		MergedValues: map[string]any{"ui": map[string]any{"color": "#326ce5"}},
	})
	r.NoError(err)
	r.Len(out, 1, "argo helm: one Application")

	app := out[0]
	r.Equal("argoproj.io/v1alpha1", app.GetAPIVersion())
	r.Equal("Application", app.GetKind())
	r.Equal("apps", app.GetNamespace(), "Application lives in the OCMDeployment namespace")
	r.Equal(testOCIURL, str(t, app, "spec", "source", "repoURL"))
	// Argo OCI Helm: chart = last segment of repo path ("podinfo").
	r.NotEmpty(str(t, app, "spec", "source", "chart"))
	// targetRevision = digest for OCI Helm.
	r.Equal(testDigest, str(t, app, "spec", "source", "targetRevision"))
	r.Equal(testTargetNS, str(t, app, "spec", "destination", "namespace"))
	color, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "valuesObject", "ui", "color")
	r.Equal("#326ce5", color)
	// releaseName defaults to the OCMDeployment name when spec.releaseName is empty.
	releaseName, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "releaseName")
	r.Equal("app", releaseName)
	// Argo auto-sync requires both prune and selfHeal so drift is corrected automatically;
	// enabled is true when the OCMDeployment is not suspended.
	automated, _, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	r.Equal(true, automated["enabled"])
	r.Equal(true, automated["prune"])
	r.Equal(true, automated["selfHeal"])
	r.Contains(app.GetFinalizers(), "resources-finalizer.argocd.argoproj.io")
}

func TestRenderArgoKustomize(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: "oci://localhost:5000/examples/deployment/kustomize",
		Digest: "sha256:aaaa",
		IsHelm: false,
		KustomizeImages: []localize.KustomizeImage{
			{Name: "ghcr.io/stefanprodan/podinfo", NewName: "localhost:5000/stefanprodan/podinfo", NewTag: "6.9.1", Digest: "sha256:262578"},
		},
	})
	r.NoError(err)
	r.Len(out, 1, "argo kustomize: one Application")

	app := out[0]
	r.Equal("Application", app.GetKind())
	r.Equal("oci://localhost:5000/examples/deployment/kustomize", str(t, app, "spec", "source", "repoURL"))
	// Argo kustomize images are stored as []any (DeepCopy-safe); each element is a string.
	imagesRaw, _, _ := unstructured.NestedSlice(app.Object, "spec", "source", "kustomize", "images")
	r.Len(imagesRaw, 1)
	r.Equal("ghcr.io/stefanprodan/podinfo=localhost:5000/stefanprodan/podinfo:6.9.1@sha256:262578", imagesRaw[0])
}

func TestRenderUnsupportedCombination(t *testing.T) {
	r := require.New(t)
	dep := makeDep("Spinnaker", "apps", "app")
	_, err := Render(RenderInput{Dep: dep, IsHelm: true})
	r.Error(err)
	r.ErrorContains(err, "unknown deployer engine")
}

func TestRenderFluxHelmSuspend(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app", withSuspend)
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: testOCIURL,
		Digest: testDigest,
		IsHelm: true,
	})
	r.NoError(err)
	r.Len(out, 2)
	src := out[0]
	suspended, _, _ := unstructured.NestedBool(src.Object, "spec", "suspend")
	r.True(suspended, "Suspend=true must propagate to OCIRepository spec.suspend")
}

func TestRenderArgoHelmSuspend(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app", withSuspend)
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: testOCIURL,
		Digest: testDigest,
		IsHelm: true,
	})
	r.NoError(err)
	r.Len(out, 1)
	app := out[0]
	automated, _, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	// Suspend=true disables automated sync via enabled=false, but keeps prune/selfHeal configured
	// so the policy is restored intact on resume (Argo's suspend equivalent).
	r.Equal(false, automated["enabled"], "Suspend=true must set syncPolicy.automated.enabled=false")
	r.Equal(true, automated["prune"])
	r.Equal(true, automated["selfHeal"])
}

func TestRenderFluxHelmWithSourceSecretRef(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app", withFluxSecretRef("my-creds"))
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: testOCIURL,
		Digest: testDigest,
		IsHelm: true,
	})
	r.NoError(err)
	r.Len(out, 2)
	src := out[0]
	secretName, found, err := unstructured.NestedString(src.Object, "spec", "secretRef", "name")
	r.NoError(err)
	r.True(found, "sourceSecretRef must appear in OCIRepository spec.secretRef")
	r.Equal("my-creds", secretName)
}

func TestRenderArgoCreateNamespaceDynamic(t *testing.T) {
	hasNamespace := func(out []*unstructured.Unstructured) bool {
		for _, o := range out {
			if o.GetKind() == "Namespace" {
				return true
			}
		}
		return false
	}
	findApp := func(t *testing.T, out []*unstructured.Unstructured) *unstructured.Unstructured {
		t.Helper()
		for _, o := range out {
			if o.GetKind() == "Application" {
				return o
			}
		}
		t.Fatal("no Application object in render output")
		return nil
	}
	// The namespace is created uniformly via the SA-applied applyset Namespace member (as for Flux),
	// never via Argo's syncOptions CreateNamespace — which must not appear on the Application.
	argoHasNoCreateNamespaceSyncOption := func(t *testing.T, app *unstructured.Unstructured) {
		t.Helper()
		r := require.New(t)
		syncOpts, _, _ := unstructured.NestedSlice(app.Object, "spec", "syncPolicy", "syncOptions")
		for _, o := range syncOpts {
			s, _ := o.(string)
			r.NotContains(s, "CreateNamespace", "Argo Application must not carry a CreateNamespace syncOption")
		}
	}

	t.Run("createNamespace=true renders a Namespace applyset member", func(t *testing.T) {
		r := require.New(t)
		dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app", withCreateNamespace)
		out, err := Render(RenderInput{
			Dep:    dep,
			OCIURL: testOCIURL,
			Digest: testDigest,
			IsHelm: true,
		})
		r.NoError(err)
		r.True(hasNamespace(out), "createNamespace=true must render a Namespace applyset member")
		argoHasNoCreateNamespaceSyncOption(t, findApp(t, out))
	})

	t.Run("createNamespace=false renders no Namespace member", func(t *testing.T) {
		r := require.New(t)
		dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app") // default: new(false)
		out, err := Render(RenderInput{
			Dep:    dep,
			OCIURL: testOCIURL,
			Digest: testDigest,
			IsHelm: true,
		})
		r.NoError(err)
		r.False(hasNamespace(out), "createNamespace=false must not render a Namespace member")
		argoHasNoCreateNamespaceSyncOption(t, findApp(t, out))
	})
}

func TestRenderFluxHelmRepo(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:              dep,
		IsHelm:           true,
		HelmRepo:         true,
		HelmRepoURL:      "https://charts.example.com",
		HelmChartName:    "podinfo",
		HelmChartVersion: "6.9.1",
		MergedValues:     map[string]any{"ui": map[string]any{"color": "#326ce5"}},
	})
	r.NoError(err)
	r.Len(out, 2, "flux helm repo: HelmRepository + HelmRelease")

	src := out[0]
	r.Equal("source.toolkit.fluxcd.io/v1", src.GetAPIVersion())
	r.Equal("HelmRepository", src.GetKind())
	r.Equal("app", src.GetName())
	r.Equal("apps", src.GetNamespace())
	r.Equal("https://charts.example.com", str(t, src, "spec", "url"))
	// HelmRepository must NOT have spec.ref.digest.
	_, found, _ := unstructured.NestedMap(src.Object, "spec", "ref")
	r.False(found, "HelmRepository must not have spec.ref")

	release := out[1]
	r.Equal("helm.toolkit.fluxcd.io/v2", release.GetAPIVersion())
	r.Equal("HelmRelease", release.GetKind())
	r.Equal(testTargetNS, str(t, release, "spec", "targetNamespace"))
	// HTTP Helm: spec.chart.spec (not chartRef).
	_, chartRefFound, _ := unstructured.NestedMap(release.Object, "spec", "chartRef")
	r.False(chartRefFound, "HelmRepo HelmRelease must not have spec.chartRef")
	r.Equal("podinfo", str(t, release, "spec", "chart", "spec", "chart"))
	r.Equal("6.9.1", str(t, release, "spec", "chart", "spec", "version"))
	r.Equal("HelmRepository", str(t, release, "spec", "chart", "spec", "sourceRef", "kind"))
	r.Equal("app", str(t, release, "spec", "chart", "spec", "sourceRef", "name"))
	r.Equal("apps", str(t, release, "spec", "chart", "spec", "sourceRef", "namespace"))
	color, _, _ := unstructured.NestedString(release.Object, "spec", "values", "ui", "color")
	r.Equal("#326ce5", color)
}

func TestRenderFluxHelmRepoWithSourceSecretRef(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app", withFluxSecretRef("helm-creds"))
	out, err := Render(RenderInput{
		Dep:              dep,
		IsHelm:           true,
		HelmRepo:         true,
		HelmRepoURL:      "https://charts.example.com",
		HelmChartName:    "podinfo",
		HelmChartVersion: "6.9.1",
	})
	r.NoError(err)
	r.Len(out, 2)
	src := out[0]
	r.Equal("HelmRepository", src.GetKind())
	secretName, found, err := unstructured.NestedString(src.Object, "spec", "secretRef", "name")
	r.NoError(err)
	r.True(found, "SourceSecretRef must appear in HelmRepository spec.secretRef")
	r.Equal("helm-creds", secretName)
}

func TestRenderArgoHelmRepo(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app")
	out, err := Render(RenderInput{
		Dep:              dep,
		IsHelm:           true,
		HelmRepo:         true,
		HelmRepoURL:      "https://charts.example.com",
		HelmChartName:    "podinfo",
		HelmChartVersion: "6.9.1",
		MergedValues:     map[string]any{"ui": map[string]any{"color": "#326ce5"}},
	})
	r.NoError(err)
	r.Len(out, 1, "argo helm repo: one Application")

	app := out[0]
	r.Equal("argoproj.io/v1alpha1", app.GetAPIVersion())
	r.Equal("Application", app.GetKind())
	r.Equal("apps", app.GetNamespace())
	r.Equal("https://charts.example.com", str(t, app, "spec", "source", "repoURL"))
	r.Equal("podinfo", str(t, app, "spec", "source", "chart"))
	r.Equal("6.9.1", str(t, app, "spec", "source", "targetRevision"))
	color, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "valuesObject", "ui", "color")
	r.Equal("#326ce5", color)
	releaseName, _, _ := unstructured.NestedString(app.Object, "spec", "source", "helm", "releaseName")
	r.Equal("app", releaseName)
	r.Equal(testTargetNS, str(t, app, "spec", "destination", "namespace"))
	// Auto-sync.
	automated, _, _ := unstructured.NestedMap(app.Object, "spec", "syncPolicy", "automated")
	r.Equal(true, automated["prune"])
	r.Equal(true, automated["selfHeal"])
	r.Contains(app.GetFinalizers(), "resources-finalizer.argocd.argoproj.io")
}

func TestRenderFluxKustomizePatches(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app")
	patches := []map[string]any{
		{
			"patch":  "- op: add\n  path: /metadata/labels/env\n  value: staging",
			"target": map[string]any{"kind": "Deployment", "name": ".*"},
		},
	}
	out, err := Render(RenderInput{
		Dep:              dep,
		OCIURL:           "oci://localhost:5000/examples/deployment/kustomize",
		Digest:           "sha256:bbbb",
		IsHelm:           false,
		KustomizePatches: patches,
	})
	r.NoError(err)
	r.Len(out, 2, "flux kustomize: OCIRepository + Kustomization")

	ks := out[1]
	r.Equal("Kustomization", ks.GetKind())
	// patches must appear under spec.patches as a []any slice.
	patchesRaw, found, err := unstructured.NestedSlice(ks.Object, "spec", "patches")
	r.NoError(err)
	r.True(found, "spec.patches must be present when KustomizePatches are supplied")
	r.Len(patchesRaw, 1)
	entry, ok := patchesRaw[0].(map[string]any)
	r.True(ok, "patch entry must be map[string]any")
	r.Equal("- op: add\n  path: /metadata/labels/env\n  value: staging", entry["patch"])
	target, ok := entry["target"].(map[string]any)
	r.True(ok, "patch target must be map[string]any")
	r.Equal("Deployment", target["kind"])
	// No images supplied — images key must be absent.
	_, imagesFound, _ := unstructured.NestedSlice(ks.Object, "spec", "images")
	r.False(imagesFound, "spec.images must be absent when no KustomizeImages supplied")
}

func TestRenderArgoKustomizePatches(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app")
	patches := []map[string]any{
		{
			"patch":  "- op: replace\n  path: /spec/replicas\n  value: 3",
			"target": map[string]any{"kind": "Deployment", "name": "podinfo"},
		},
	}
	// Supply both images and patches to verify they coexist under source.kustomize.
	out, err := Render(RenderInput{
		Dep:    dep,
		OCIURL: "oci://localhost:5000/examples/deployment/kustomize",
		Digest: "sha256:cccc",
		IsHelm: false,
		KustomizeImages: []localize.KustomizeImage{
			{Name: "ghcr.io/stefanprodan/podinfo", NewName: "localhost:5000/podinfo", NewTag: "6.9.1"},
		},
		KustomizePatches: patches,
	})
	r.NoError(err)
	r.Len(out, 1, "argo kustomize: one Application")

	app := out[0]
	r.Equal("Application", app.GetKind())
	// patches under source.kustomize.patches.
	patchesRaw, found, err := unstructured.NestedSlice(app.Object, "spec", "source", "kustomize", "patches")
	r.NoError(err)
	r.True(found, "source.kustomize.patches must be present")
	r.Len(patchesRaw, 1)
	entry, ok := patchesRaw[0].(map[string]any)
	r.True(ok, "patch entry must be map[string]any")
	r.Equal("- op: replace\n  path: /spec/replicas\n  value: 3", entry["patch"])
	// images must coexist under source.kustomize.images.
	imagesRaw, imagesFound, _ := unstructured.NestedSlice(app.Object, "spec", "source", "kustomize", "images")
	r.True(imagesFound, "source.kustomize.images must coexist with patches")
	r.Len(imagesRaw, 1)
}

func TestRenderArgoKustomizePatchesOnly(t *testing.T) {
	r := require.New(t)
	dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app")
	patches := []map[string]any{
		{"patch": "- op: add\n  path: /x\n  value: y", "target": map[string]any{"kind": "ConfigMap"}},
	}
	out, err := Render(RenderInput{
		Dep:              dep,
		OCIURL:           "oci://localhost:5000/examples/deployment/kustomize",
		Digest:           "sha256:dddd",
		IsHelm:           false,
		KustomizePatches: patches,
	})
	r.NoError(err)
	r.Len(out, 1)

	app := out[0]
	patchesRaw, found, _ := unstructured.NestedSlice(app.Object, "spec", "source", "kustomize", "patches")
	r.True(found)
	r.Len(patchesRaw, 1)
	// No images supplied — images key must be absent.
	_, imagesFound, _ := unstructured.NestedSlice(app.Object, "spec", "source", "kustomize", "images")
	r.False(imagesFound, "images must be absent when only patches supplied")
}

// TestRenderFluxKustomizeOrphan verifies that with DeletionPolicyOrphan the Kustomization carries
// the prune:disabled annotation under spec.commonMetadata.annotations, and prune is false.
// The inverse (Background policy) must not set the annotation and must have prune:true.
func TestRenderFluxKustomizeOrphan(t *testing.T) {
	tests := []struct {
		name           string
		policy         v1alpha1.DeletionPolicy
		wantPrune      bool
		wantAnnotation bool
	}{
		{name: "Background cascades", policy: v1alpha1.DeletionPolicyBackground, wantPrune: true, wantAnnotation: false},
		{name: "Foreground cascades", policy: v1alpha1.DeletionPolicyForeground, wantPrune: true, wantAnnotation: false},
		{name: "Orphan disables prune", policy: v1alpha1.DeletionPolicyOrphan, wantPrune: false, wantAnnotation: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			dep := makeDep(v1alpha1.DeployerEngineFluxCD, "apps", "app")
			dep.Spec.DeletionPolicy = tc.policy

			out, err := Render(RenderInput{
				Dep:    dep,
				OCIURL: testOCIURL,
				Digest: testDigest,
				IsHelm: false,
			})
			r.NoError(err)
			// 0=OCIRepository, 1=Kustomization
			r.Len(out, 2)
			ks := out[1]
			r.Equal("Kustomization", ks.GetKind())

			prune, found, err := unstructured.NestedBool(ks.Object, "spec", "prune")
			r.NoError(err)
			r.True(found, "spec.prune must be present")
			r.Equal(tc.wantPrune, prune)

			annotation, _, _ := unstructured.NestedString(ks.Object, "spec", "commonMetadata", "annotations", "kustomize.toolkit.fluxcd.io/prune")
			if tc.wantAnnotation {
				r.Equal("disabled", annotation, "Orphan must set prune:disabled annotation")
			} else {
				r.Empty(annotation, "non-Orphan must not set prune:disabled annotation")
			}
		})
	}
}

// TestRenderArgoAppOrphan verifies that with DeletionPolicyOrphan the Application is rendered
// without the resources-finalizer, and the inverse (Background/Foreground) includes it.
func TestRenderArgoAppOrphan(t *testing.T) {
	tests := []struct {
		name          string
		policy        v1alpha1.DeletionPolicy
		wantFinalizer bool
	}{
		{name: "Background has finalizer", policy: v1alpha1.DeletionPolicyBackground, wantFinalizer: true},
		{name: "Foreground has finalizer", policy: v1alpha1.DeletionPolicyForeground, wantFinalizer: true},
		{name: "Orphan omits finalizer", policy: v1alpha1.DeletionPolicyOrphan, wantFinalizer: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			dep := makeDep(v1alpha1.DeployerEngineArgoCD, "apps", "app")
			dep.Spec.DeletionPolicy = tc.policy

			out, err := Render(RenderInput{
				Dep:    dep,
				OCIURL: testOCIURL,
				Digest: testDigest,
				IsHelm: false,
			})
			r.NoError(err)
			r.Len(out, 1)
			app := out[0]
			r.Equal("Application", app.GetKind())

			finalizers := app.GetFinalizers()
			hasFinalizer := false
			for _, f := range finalizers {
				if f == "resources-finalizer.argocd.argoproj.io" {
					hasFinalizer = true
				}
			}
			r.Equal(tc.wantFinalizer, hasFinalizer, "finalizer presence mismatch for policy %s", tc.policy)
		})
	}
}
