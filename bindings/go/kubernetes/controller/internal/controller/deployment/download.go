package deployment

// download.go handles downloading the deploy artifact blob for OCMDeployment.
// The child Resource CR has already resolved the OCI coordinates; this file
// downloads the blob for localization (values.yaml inspection for Helm, or
// kustomize layout traversal).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"ocm.software/open-component-model/bindings/go/blob"
	helmv1 "ocm.software/open-component-model/bindings/go/helm/spec/access/v1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/render"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/limitedreader"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/resolution"
	ocmruntime "ocm.software/open-component-model/bindings/go/runtime"
)

// ociAccessSpec is the minimal subset of the Resource status Access JSON we need.
type ociAccessSpec struct {
	Type           string `json:"type"`
	ImageRef       string `json:"imageReference,omitempty"`
	LocalReference string `json:"localReference,omitempty"`
	MediaType      string `json:"mediaType,omitempty"`
}

// downloadDeployArtifact resolves the deploy artifact coordinates from the Resource's status and
// parses them into a deploySource. For Helm it also downloads the chart to extract values.yaml;
// for kustomize the artifact is left for the engine to pull.
func (r *Reconciler) downloadDeployArtifact(ctx context.Context, dep *v1alpha1.OCMDeployment, res *v1alpha1.Resource) (source *deploySource, err error) {
	logger := log.FromContext(ctx)

	if res.Status.Resource == nil {
		return nil, fmt.Errorf("deploy resource %s has no status.resource", res.Name)
	}

	cacheKey := resourceDigest(res)
	if cacheKey != "" {
		if cached := r.getCachedArtifact(cacheKey); cached != nil {
			return cached, nil
		}
	}

	defer func() {
		if err == nil {
			r.putCachedArtifact(cacheKey, source)
		}
	}()

	var access ociAccessSpec
	if err := json.Unmarshal(res.Status.Resource.Access.Raw, &access); err != nil {
		return nil, fmt.Errorf("failed to parse resource access spec: %w", err)
	}

	// --- Helm/v1 (classic HTTP Helm repository) branch ---
	if isHelmRepositoryAccess(access.Type) {
		repoURL, chartName, chartVersion, ok := parseHelmRepoAccess(res.Status.Resource.Access.Raw)
		if !ok {
			return nil, fmt.Errorf("deploy resource %s: Helm/v1 access spec missing required fields", res.Name)
		}

		source = &deploySource{
			RenderInput: render.RenderInput{
				IsHelm:           true,
				HelmRepo:         true,
				HelmRepoURL:      repoURL,
				HelmChartName:    chartName,
				HelmChartVersion: chartVersion,
			},
		}

		// helm ResourceRepository plugin handles the Helm/v1 HTTP-repo access.
		chartBytes, err := r.downloadBlob(ctx, dep, res)
		if err != nil {
			return nil, fmt.Errorf("failed to download helm chart blob for %s: %w", res.Name, err)
		}

		valuesYAML, _, err := extractHelmChartFiles(chartBytes) // appVersion parsed but unused: no chart exposes it as a tag fallback today.
		if err != nil {
			return nil, fmt.Errorf("failed to extract helm chart files for %s: %w", res.Name, err)
		}

		source.valuesYAML = valuesYAML
		return source, nil
	}

	// --- OCI / LocalBlob branch ---
	imageRef := access.ImageRef
	if imageRef == "" && access.Type == "LocalBlob/v1" {
		// LocalBlob resources are stored inside the component descriptor OCI repo.
		// The imageReference can be reconstructed as:
		//   <baseUrl>/component-descriptors/<component>@sha256:<localReference>
		var repoSpec struct {
			BaseURL string `json:"baseUrl"`
		}
		if err := json.Unmarshal(res.Status.Component.RepositorySpec.Raw, &repoSpec); err != nil {
			return nil, fmt.Errorf("failed to parse repository spec for %s: %w", res.Name, err)
		}
		if repoSpec.BaseURL == "" {
			return nil, fmt.Errorf("deploy resource %s: LocalBlob/v1 but repository spec has no baseUrl", res.Name)
		}
		imageRef = strings.TrimRight(repoSpec.BaseURL, "/") +
			"/component-descriptors/" + res.Status.Component.Component +
			"@" + access.LocalReference
	}
	if imageRef == "" {
		return nil, fmt.Errorf("deploy resource %s access spec has no imageReference", res.Name)
	}

	// imageRef is e.g. "registry:5000/charts/podinfo:6.9.1@sha256:..."
	// The oci:// prefix form for Flux/Argo is: oci://<registry>/<repository>
	ociURL, insecure, err := imageRefToOCIURL(imageRef)
	if err != nil {
		return nil, fmt.Errorf("failed to build OCI URL from imageRef %q: %w", imageRef, err)
	}

	isHelm := isHelmArtifact(res, access)

	source = &deploySource{
		RenderInput: render.RenderInput{
			OCIURL:   ociURL,
			Insecure: insecure,
			Digest:   cacheKey,
			IsHelm:   isHelm,
		},
	}

	if !isHelm {
		logger.Info("deploy artifact is kustomize, skipping blob download for values extraction")

		return source, nil
	}

	// helm ResourceRepository plugin handles the Helm/v1 HTTP-repo access; OCI Helm uses the plugin registry.
	chartBytes, err := r.downloadBlob(ctx, dep, res)
	if err != nil {
		return nil, fmt.Errorf("failed to download helm chart blob: %w", err)
	}

	valuesYAML, _, err := extractHelmChartFiles(chartBytes) // appVersion parsed but unused: no chart exposes it as a tag fallback today.
	if err != nil {
		return nil, fmt.Errorf("failed to extract helm chart files: %w", err)
	}

	source.valuesYAML = valuesYAML
	return source, nil
}

// downloadBlob downloads a resource blob, dispatching on access type:
//   - LocalBlob/v1: the blob is stored inline in the OCI manifest; retrieve via GetLocalResource.
//   - All other types (OCI artifacts): use the resource plugin registry.
func (r *Reconciler) downloadBlob(ctx context.Context, dep *v1alpha1.OCMDeployment, res *v1alpha1.Resource) ([]byte, error) {
	logger := log.FromContext(ctx)

	if res.Status.Resource == nil {
		return nil, fmt.Errorf("resource %s has no status.resource", res.Name)
	}

	// Check access type.
	var raw struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(res.Status.Resource.Access.Raw, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse access type for %s: %w", res.Name, err)
	}

	cfg, err := r.getConfiguration(ctx, dep)
	if err != nil {
		return nil, fmt.Errorf("failed to get OCM configuration: %w", err)
	}

	pm, err := r.PluginManagerFor(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create plugin manager: %w", err)
	}

	var blobRC io.ReadCloser

	if raw.Type == "LocalBlob/v1" {
		// LocalBlob resources are stored as blobs in the OCI manifest of the component version.
		// The resource plugin registry has no handler for them; use the repository directly.
		repoSpec := &ocmruntime.Raw{}
		if err := repoSpec.UnmarshalJSON(res.Status.Component.RepositorySpec.Raw); err != nil {
			return nil, fmt.Errorf("failed to decode repository spec for %s: %w", res.Name, err)
		}

		repoResolver, err := r.Resolver.RepositoryResolver(ctx, &resolution.Options{
			RepositorySpec: repoSpec,
			Configuration:  cfg,
			PluginManager:  pm,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create repository resolver for local blob download: %w", err)
		}

		repo, err := repoResolver.GetComponentVersionRepositoryForComponent(ctx,
			res.Status.Component.Component,
			res.Status.Component.Version,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve repository for local blob download: %w", err)
		}

		identity := ocmruntime.Identity{"name": res.Status.Resource.Name}

		localBlob, _, err := repo.GetLocalResource(ctx,
			res.Status.Component.Component,
			res.Status.Component.Version,
			identity,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to get local resource %s: %w", res.Name, err)
		}

		if err := r.checkDeclaredSize(localBlob); err != nil {
			return nil, err
		}

		blobRC, err = localBlob.ReadCloser()
		if err != nil {
			return nil, fmt.Errorf("failed to open local blob reader for %s: %w", res.Name, err)
		}
	} else {
		imageRef := extractImageRef(res)
		logger.Info("downloading artifact blob", "imageRef", imageRef)

		descriptorResource := resStatusToDescriptorResource(res)

		resourcePlugin, err := pm.ResourcePluginRegistry.GetResourcePlugin(ctx, descriptorResource.Access)
		if err != nil {
			return nil, fmt.Errorf("failed to get resource plugin for %s: %w", imageRef, err)
		}

		blobData, err := resourcePlugin.DownloadResource(ctx, descriptorResource, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to download resource blob: %w", err)
		}

		if err := r.checkDeclaredSize(blobData); err != nil {
			return nil, err
		}

		blobRC, err = blobData.ReadCloser()
		if err != nil {
			return nil, fmt.Errorf("failed to open blob reader: %w", err)
		}
	}

	defer blobRC.Close()

	var reader io.Reader = blobRC
	if r.MaxResourceSizeBytes > 0 {
		reader = limitedreader.New(blobRC, r.MaxResourceSizeBytes)
	}

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, reader); err != nil {
		return nil, fmt.Errorf("failed to read blob: %w", err)
	}

	return buf.Bytes(), nil
}

// checkDeclaredSize is an opportunistic pre-check: when a blob reports its declared size and the
// controller enforces a maximum, reject before reading. Mirrors the deployer controller's pre-check.
// Blobs that do not report a size (blob.SizeUnknown) fall through to the io.LimitedReader hard cap.
func (r *Reconciler) checkDeclaredSize(b blob.ReadOnlyBlob) error {
	if r.MaxResourceSizeBytes <= 0 {
		return nil
	}
	sizeAware, ok := b.(blob.SizeAware)
	if !ok {
		return nil
	}
	size := sizeAware.Size()
	if size != blob.SizeUnknown && size > r.MaxResourceSizeBytes {
		return fmt.Errorf("resource size %s exceeds maximum allowed size of %s",
			apiresource.NewQuantity(size, apiresource.BinarySI),
			apiresource.NewQuantity(r.MaxResourceSizeBytes, apiresource.BinarySI))
	}

	return nil
}

// getCachedArtifact looks up a deploySource by digest key. A nil cache means the Reconciler was
// constructed without SetupWithManager (unit tests); expirable.LRU is internally synchronized.
func (r *Reconciler) getCachedArtifact(key string) *deploySource {
	if r.deployArtifactCache == nil {
		return nil
	}
	v, _ := r.deployArtifactCache.Get(key)
	return v
}

// putCachedArtifact stores a deploySource under the given digest key. An empty key skips caching
// (tag-only resources carry no digest); a nil cache (unit-test Reconciler) is a no-op.
func (r *Reconciler) putCachedArtifact(key string, a *deploySource) {
	if key == "" || r.deployArtifactCache == nil {
		return
	}
	r.deployArtifactCache.Add(key, a)
}

// extractHelmChartFiles reads values.yaml and Chart.yaml out of a Helm chart blob.
//
// The blob may be delivered in two forms depending on the access type:
//  1. A raw Helm chart tgz (application/vnd.cncf.helm.chart.content.v1.tar+gzip):
//     a gzip-compressed tar whose entries include "<chart>/values.yaml" etc.
//  2. An OCI layout tar produced by the resource plugin's Materialize path:
//     an uncompressed tar with oci-layout, index.json, and blobs/sha256/<digest>
//     entries.  The chart tgz is stored inside as one of the layer blobs.
//
// The function handles both transparently.
func extractHelmChartFiles(data []byte) (valuesYAML []byte, appVersion string, err error) {
	// Try OCI layout tar first (gzipped or plain) — this is what the resource plugin's
	// Materialize path always returns (CopyToOCILayoutInMemory produces a gzipped tar).
	chartTGZ, ociErr := extractHelmLayerFromOCILayout(data)
	if ociErr == nil {
		gz, err := gzip.NewReader(bytes.NewReader(chartTGZ))
		if err != nil {
			return nil, "", fmt.Errorf("helm chart layer in OCI layout is not a gzip: %w", err)
		}
		defer gz.Close()

		return extractHelmFilesFromTar(gz)
	}

	// Fallback: try to open directly as a raw gzip-compressed Helm chart tgz.
	gz, gzErr := gzip.NewReader(bytes.NewReader(data))
	if gzErr != nil {
		return nil, "", fmt.Errorf("blob is neither a Helm chart tgz nor a valid OCI layout: %w", errors.Join(gzErr, ociErr))
	}
	defer gz.Close()

	return extractHelmFilesFromTar(gz)
}

// extractHelmFilesFromTar reads values.yaml and Chart.yaml from an open tar reader.
func extractHelmFilesFromTar(r io.Reader) (valuesYAML []byte, appVersion string, err error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, "", fmt.Errorf("reading tar: %w", err)
		}

		// Skip files nested under a subchart directory; only the root chart's files are wanted.
		if strings.Contains(hdr.Name, "/charts/") {
			continue
		}

		base := filepath.Base(hdr.Name)
		switch base {
		case "values.yaml":
			valuesYAML, err = io.ReadAll(tr)
			if err != nil {
				return nil, "", fmt.Errorf("reading values.yaml: %w", err)
			}
		case "Chart.yaml":
			chartBytes, err := io.ReadAll(tr)
			if err != nil {
				return nil, "", fmt.Errorf("reading Chart.yaml: %w", err)
			}

			var chartMeta struct {
				AppVersion string `yaml:"appVersion"`
			}
			if err := yaml.Unmarshal(chartBytes, &chartMeta); err == nil {
				appVersion = chartMeta.AppVersion
			}
		}
	}

	return valuesYAML, appVersion, nil
}

// helmChartContentMediaType is the OCI media type for a Helm chart content layer.
const helmChartContentMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// extractHelmLayerFromOCILayout walks an OCI layout tar, parses index.json to find
// the manifest, then reads the manifest to locate the Helm chart content layer blob.
// It returns the raw tgz bytes of that layer.
func extractHelmLayerFromOCILayout(data []byte) ([]byte, error) {
	// Collect all blobs and index.json from the OCI layout tar.
	// The OCI layout tar may be gzip-compressed (CopyToOCILayoutInMemory always gzips).
	blobs := make(map[string][]byte)
	var indexJSON []byte

	var tarReader io.Reader = bytes.NewReader(data)
	if gz, err := gzip.NewReader(bytes.NewReader(data)); err == nil {
		defer gz.Close()
		tarReader = gz
	}

	tr := tar.NewReader(tarReader)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, fmt.Errorf("reading OCI layout tar: %w", err)
		}

		entry, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading OCI layout tar entry %q: %w", hdr.Name, err)
		}

		name := filepath.ToSlash(hdr.Name)
		switch {
		case name == "index.json":
			indexJSON = entry
		case strings.HasPrefix(name, "blobs/"):
			// Key by the basename (the hex digest), prefixed with algo from the path segment.
			// blobs/sha256/<hex> → "sha256:<hex>"
			parts := strings.SplitN(name, "/", 3)
			if len(parts) == 3 {
				blobs[parts[1]+":"+parts[2]] = entry
			}
		}
	}

	if len(indexJSON) == 0 {
		return nil, fmt.Errorf("OCI layout tar has no index.json")
	}

	// Parse index.json → manifest descriptor.
	var index struct {
		Manifests []struct {
			Digest string `json:"digest"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		return nil, fmt.Errorf("parsing OCI index.json: %w", err)
	}

	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("OCI index.json has no manifests")
	}

	manifestBytes, ok := blobs[index.Manifests[0].Digest]
	if !ok {
		return nil, fmt.Errorf("OCI layout missing manifest blob %q", index.Manifests[0].Digest)
	}

	// Parse manifest → find the Helm chart content layer.
	var manifest struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing OCI manifest: %w", err)
	}

	for _, layer := range manifest.Layers {
		if layer.MediaType == helmChartContentMediaType {
			chartTGZ, ok := blobs[layer.Digest]
			if !ok {
				return nil, fmt.Errorf("OCI layout missing helm chart layer blob %q", layer.Digest)
			}

			return chartTGZ, nil
		}
	}

	return nil, fmt.Errorf("OCI layout manifest has no layer with media type %q", helmChartContentMediaType)
}

// isHelmArtifact returns true when the resource's access spec indicates a Helm chart.
func isHelmArtifact(res *v1alpha1.Resource, access ociAccessSpec) bool {
	if strings.Contains(access.MediaType, "helm") {
		return true
	}
	// Fallback: resource type field carries "helmChart"
	if res.Status.Resource != nil && strings.EqualFold(res.Status.Resource.Type, "helmChart") {
		return true
	}

	return false
}

// isHelmRepositoryAccess returns true when the access type base name is "helm" (case-insensitive).
// It matches all four spellings: "Helm", "Helm/v1", "helm", "helm/v1".
func isHelmRepositoryAccess(accessType string) bool {
	base := strings.SplitN(accessType, "/", 2)[0]
	return strings.EqualFold(base, helmv1.Type) || strings.EqualFold(base, helmv1.LegacyType)
}

// parseHelmRepoAccess unmarshals the raw access JSON into a Helm/v1 spec and extracts the
// repository URL, chart name, and chart version. Returns ok=false if required fields are missing.
func parseHelmRepoAccess(raw []byte) (repoURL, chartName, chartVersion string, ok bool) {
	var h helmv1.Helm
	if err := json.Unmarshal(raw, &h); err != nil {
		return "", "", "", false
	}
	if h.HelmRepository == "" || h.HelmChart == "" {
		return "", "", "", false
	}
	return h.HelmRepository, h.GetChartName(), h.GetVersion(), true
}

// imageRefToOCIURL converts an OCI image reference like
// "registry:5000/charts/podinfo:6.9.1@sha256:abc" or
// "http://registry:5000/charts/podinfo:6.9.1@sha256:abc"
// to ("oci://registry:5000/charts/podinfo", insecure).
// insecure is true when the original ref carried an http:// scheme.
func imageRefToOCIURL(imageRef string) (string, bool, error) {
	insecure := false
	ref := imageRef

	// Strip explicit http:// or https:// scheme — Flux/Argo use oci:// with an insecure flag.
	switch {
	case strings.HasPrefix(ref, "http://"):
		insecure = true
		ref = strings.TrimPrefix(ref, "http://")
	case strings.HasPrefix(ref, "https://"):
		ref = strings.TrimPrefix(ref, "https://")
	}

	// Strip digest.
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	}
	// Strip tag (colon after the last slash is a tag, not a port).
	if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		if lastSlash := strings.LastIndex(ref, "/"); idx > lastSlash {
			ref = ref[:idx]
		}
	}

	return "oci://" + ref, insecure, nil
}

// normalizeDigest converts an OCM digest algorithm+value pair into the canonical
// "algo:value" form expected by Flux and Argo (e.g. "SHA-256"+"abc" → "sha256:abc").
// If algo is empty, value is returned as-is. If value is empty, an empty string is
// returned regardless of algo.
func normalizeDigest(algo, value string) string {
	if value == "" {
		return ""
	}

	if algo == "" {
		return value
	}
	// Normalize: "SHA-256" → "sha256" (lowercase, hyphens removed).
	return strings.ToLower(strings.ReplaceAll(algo, "-", "")) + ":" + value
}

// resourceDigest returns the normalized content digest of a resolved Resource, or "" when the status
// carries no digest (tag-only resources — see downloadDeployArtifact's cache-key handling).
func resourceDigest(res *v1alpha1.Resource) string {
	if res.Status.Resource == nil || res.Status.Resource.Digest == nil {
		return ""
	}
	return normalizeDigest(res.Status.Resource.Digest.HashAlgorithm, res.Status.Resource.Digest.Value)
}

// extractImageRef returns the imageReference from a Resource's status Access JSON.
// The Resource controller may store plain-HTTP registry refs with a URI scheme
// (e.g. "http://host/path", "https://host/path", "oci://host/path").
// Any "scheme://" prefix is stripped so callers receive a bare OCI reference.
func extractImageRef(res *v1alpha1.Resource) string {
	if res.Status.Resource == nil {
		return ""
	}

	var acc struct {
		ImageRef string `json:"imageReference"`
	}

	_ = json.Unmarshal(res.Status.Resource.Access.Raw, &acc)

	return stripScheme(acc.ImageRef)
}

// stripScheme removes a leading "scheme://" from a URI string, returning a bare OCI reference.
// The scheme may be any sequence of alphanumeric characters, '+', '-', or '.', followed by "://".
func stripScheme(ref string) string {
	if idx := strings.Index(ref, "://"); idx >= 0 {
		return ref[idx+3:]
	}

	return ref
}

// parseRefComponents splits an OCI image reference into (registry, path, tag, digest).
func parseRefComponents(ref string) (registry, path, tag, digest string) {
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		digest = ref[idx+1:]
		ref = ref[:idx]
	}

	if idx := strings.LastIndex(ref, ":"); idx >= 0 {
		if slashIdx := strings.LastIndex(ref, "/"); idx > slashIdx {
			tag = ref[idx+1:]
			ref = ref[:idx]
		}
	}

	if idx := strings.Index(ref, "/"); idx >= 0 {
		registry = ref[:idx]
		path = ref[idx+1:]
	} else {
		path = ref
	}

	return
}
