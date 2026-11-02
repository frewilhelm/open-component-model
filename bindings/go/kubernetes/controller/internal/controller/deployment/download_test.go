package deployment

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"ocm.software/open-component-model/bindings/go/blob"
)

// fakeSizeAwareBlob is a blob.ReadOnlyBlob that reports a fixed declared size.
type fakeSizeAwareBlob struct{ size int64 }

func (b fakeSizeAwareBlob) ReadCloser() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (b fakeSizeAwareBlob) Size() int64 { return b.size }

// fakeUnsizedBlob is a blob.ReadOnlyBlob without a declared size (not SizeAware).
type fakeUnsizedBlob struct{}

func (fakeUnsizedBlob) ReadCloser() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func TestImageRefToOCIURL(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantURL      string
		wantInsecure bool
		wantErr      bool
	}{
		{
			name:         "plain ref strips tag and digest",
			input:        "registry:5000/charts/podinfo:6.9.1@sha256:abc",
			wantURL:      "oci://registry:5000/charts/podinfo",
			wantInsecure: false,
		},
		{
			name:         "http scheme sets insecure and is stripped",
			input:        "http://registry:5000/charts/podinfo:6.9.1",
			wantURL:      "oci://registry:5000/charts/podinfo",
			wantInsecure: true,
		},
		{
			name:         "https scheme is stripped, not insecure",
			input:        "https://ghcr.io/stefanprodan/charts/podinfo:6.9.1",
			wantURL:      "oci://ghcr.io/stefanprodan/charts/podinfo",
			wantInsecure: false,
		},
		{
			name:         "e2e registry ref strips tag and digest",
			input:        "ocm-e2e-image-registry:5000/stefanprodan/charts/podinfo:6.9.1@sha256:565d310746f1fa4be7f93ba7965bb393153a2d57a15cfe5befc909b790a73f8a",
			wantURL:      "oci://ocm-e2e-image-registry:5000/stefanprodan/charts/podinfo",
			wantInsecure: false,
		},
		{
			name:         "digest-only ref (no tag)",
			input:        "registry:5000/image@sha256:abc",
			wantURL:      "oci://registry:5000/image",
			wantInsecure: false,
		},
		{
			name:         "no tag no digest",
			input:        "registry:5000/image",
			wantURL:      "oci://registry:5000/image",
			wantInsecure: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			gotURL, gotInsecure, err := imageRefToOCIURL(tc.input)
			if tc.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(tc.wantURL, gotURL)
			r.Equal(tc.wantInsecure, gotInsecure)
			// The result must never smuggle http/https into the OCI URL.
			r.NotContains(gotURL, "http://")
			r.NotContains(gotURL, "https://")
		})
	}
}

func TestNormalizeDigest(t *testing.T) {
	tests := []struct {
		name  string
		algo  string
		value string
		want  string
	}{
		{"SHA-256 normalised to sha256", "SHA-256", "565d310746f1", "sha256:565d310746f1"},
		{"sha256 already lowercase", "sha256", "565d310746f1", "sha256:565d310746f1"},
		{"empty algo returns bare value", "", "565d310746f1", "565d310746f1"},
		{"empty value returns empty regardless of algo", "SHA-256", "", ""},
		{"both empty returns empty", "", "", ""},
		{"SHA-512 normalised", "SHA-512", "abcdef", "sha512:abcdef"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			r.Equal(tc.want, normalizeDigest(tc.algo, tc.value))
		})
	}
}

// ---- helpers for extractHelmChartFiles tests ----

// makeHelmTGZ builds a minimal Helm chart tgz in memory:
// podinfo/Chart.yaml and podinfo/values.yaml.
func makeHelmTGZ(t *testing.T, valuesContent, appVersion string) []byte {
	t.Helper()
	r := require.New(t)

	chartYAML := "apiVersion: v2\nname: podinfo\nappVersion: " + appVersion + "\n"

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range map[string]string{
		"podinfo/Chart.yaml":  chartYAML,
		"podinfo/values.yaml": valuesContent,
	} {
		r.NoError(tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}))
		_, err := tw.Write([]byte(content))
		r.NoError(err)
	}
	r.NoError(tw.Close())
	r.NoError(gw.Close())

	return buf.Bytes()
}

// makeOCILayoutTar wraps a Helm chart tgz (chartTGZ) inside a minimal OCI
// layout tar, exactly as produced by OCIResourceStream.Materialize.
// compressed=true wraps the layout in gzip (the real plugin path).
func makeOCILayoutTar(t *testing.T, chartTGZ []byte, compressed bool) []byte {
	t.Helper()
	r := require.New(t)

	chartDigest := "sha256:" + fmt.Sprintf("%x", sha256sum(chartTGZ))

	configJSON := []byte(`{}`)
	configDigest := "sha256:" + fmt.Sprintf("%x", sha256sum(configJSON))

	manifestJSON := []byte(`{
  "schemaVersion": 2,
  "config": {"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":"` + configDigest + `","size":` + fmt.Sprint(len(configJSON)) + `},
  "layers": [
    {"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + chartDigest + `","size":` + fmt.Sprint(len(chartTGZ)) + `}
  ]
}`)
	manifestDigest := "sha256:" + fmt.Sprintf("%x", sha256sum(manifestJSON))

	indexJSON := []byte(`{"schemaVersion":2,"manifests":[{"digest":"` + manifestDigest + `","mediaType":"application/vnd.oci.image.manifest.v1+json","size":` + fmt.Sprint(len(manifestJSON)) + `}]}`)

	ociLayout := []byte(`{"imageLayoutVersion":"1.0.0"}`)

	var buf bytes.Buffer
	var tw *tar.Writer
	var gw *gzip.Writer
	if compressed {
		gw = gzip.NewWriter(&buf)
		tw = tar.NewWriter(gw)
	} else {
		tw = tar.NewWriter(&buf)
	}

	for name, content := range map[string][]byte{
		"oci-layout": ociLayout,
		"index.json": indexJSON,
		"blobs/" + strings.ReplaceAll(manifestDigest, ":", "/"): manifestJSON,
		"blobs/" + strings.ReplaceAll(configDigest, ":", "/"):   configJSON,
		"blobs/" + strings.ReplaceAll(chartDigest, ":", "/"):    chartTGZ,
	} {
		r.NoError(tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}))
		_, err := tw.Write(content)
		r.NoError(err)
	}
	r.NoError(tw.Close())
	if gw != nil {
		r.NoError(gw.Close())
	}

	return buf.Bytes()
}

func sha256sum(b []byte) [32]byte {
	// import crypto/sha256 inline via stdlib
	return sha256.Sum256(b)
}

func TestExtractHelmChartFiles(t *testing.T) {
	const valuesYAML = `image:
  repository: ghcr.io/stefanprodan/podinfo
  tag: "6.9.1"
`
	const appVersion = "6.9.1"

	t.Run("raw Helm tgz", func(t *testing.T) {
		r := require.New(t)
		tgz := makeHelmTGZ(t, valuesYAML, appVersion)
		gotValues, gotApp, err := extractHelmChartFiles(tgz)
		r.NoError(err)
		r.YAMLEq(valuesYAML, string(gotValues))
		r.Equal(appVersion, gotApp)
	})

	t.Run("OCI layout tar wrapping Helm tgz (uncompressed)", func(t *testing.T) {
		r := require.New(t)
		tgz := makeHelmTGZ(t, valuesYAML, appVersion)
		ociTar := makeOCILayoutTar(t, tgz, false)
		gotValues, gotApp, err := extractHelmChartFiles(ociTar)
		r.NoError(err)
		r.YAMLEq(valuesYAML, string(gotValues))
		r.Equal(appVersion, gotApp)
	})

	t.Run("gzipped OCI layout tar wrapping Helm tgz (real plugin path)", func(t *testing.T) {
		r := require.New(t)
		tgz := makeHelmTGZ(t, valuesYAML, appVersion)
		ociTar := makeOCILayoutTar(t, tgz, true)
		gotValues, gotApp, err := extractHelmChartFiles(ociTar)
		r.NoError(err)
		r.YAMLEq(valuesYAML, string(gotValues))
		r.Equal(appVersion, gotApp)
	})

	t.Run("OCI layout tar without helm layer returns error", func(t *testing.T) {
		r := require.New(t)
		// Build an OCI layout with a non-helm layer media type.
		configJSON := []byte(`{}`)
		configDigest := "sha256:" + fmt.Sprintf("%x", sha256sum(configJSON))
		manifestJSON := []byte(`{
  "schemaVersion": 2,
  "config": {"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + configDigest + `","size":2},
  "layers": []
}`)
		manifestDigest := "sha256:" + fmt.Sprintf("%x", sha256sum(manifestJSON))
		indexJSON := []byte(`{"schemaVersion":2,"manifests":[{"digest":"` + manifestDigest + `","mediaType":"application/vnd.oci.image.manifest.v1+json","size":` + fmt.Sprint(len(manifestJSON)) + `}]}`)

		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, content := range map[string][]byte{
			"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`),
			"index.json": indexJSON,
			"blobs/" + strings.ReplaceAll(manifestDigest, ":", "/"): manifestJSON,
			"blobs/" + strings.ReplaceAll(configDigest, ":", "/"):   configJSON,
		} {
			require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}))
			_, err := tw.Write(content)
			require.NoError(t, err)
		}
		require.NoError(t, tw.Close())

		_, _, err := extractHelmChartFiles(buf.Bytes())
		r.ErrorContains(err, helmChartContentMediaType)
	})

	t.Run("neither tgz nor OCI layout returns error", func(t *testing.T) {
		r := require.New(t)
		_, _, err := extractHelmChartFiles([]byte("not a valid blob"))
		r.ErrorContains(err, "neither a Helm chart tgz nor a valid OCI layout")
	})
}

func TestExtractHelmChartFilesSubchartShadowing(t *testing.T) {
	r := require.New(t)

	const rootValues = "image:\n  repository: root-chart/app\n  tag: \"1.0.0\"\n"
	const subValues = "image:\n  repository: subchart/app\n  tag: \"2.0.0\"\n"
	const chartYAML = "apiVersion: v2\nname: mychart\nappVersion: \"1.0.0\"\n"

	// Build a tgz that contains both root values.yaml and a subchart values.yaml.
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for name, content := range map[string]string{
		"mychart/Chart.yaml":             chartYAML,
		"mychart/values.yaml":            rootValues,
		"mychart/charts/sub/values.yaml": subValues,
		"mychart/charts/sub/Chart.yaml":  "apiVersion: v2\nname: sub\nappVersion: \"2.0.0\"\n",
	} {
		r.NoError(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}))
		_, err := tw.Write([]byte(content))
		r.NoError(err)
	}
	r.NoError(tw.Close())
	r.NoError(gw.Close())

	gotValues, gotApp, err := extractHelmChartFiles(buf.Bytes())
	r.NoError(err)
	r.YAMLEq(rootValues, string(gotValues), "root chart values.yaml must not be shadowed by subchart")
	r.Equal("1.0.0", gotApp, "appVersion must come from root Chart.yaml")
}

func TestCheckDeclaredSize(t *testing.T) {
	t.Run("declared size over limit is rejected before read", func(t *testing.T) {
		r := require.New(t)
		rec := &Reconciler{MaxResourceSizeBytes: 1024}
		err := rec.checkDeclaredSize(fakeSizeAwareBlob{size: 2048})
		r.ErrorContains(err, "exceeds maximum allowed size")
	})

	t.Run("declared size under limit passes", func(t *testing.T) {
		r := require.New(t)
		rec := &Reconciler{MaxResourceSizeBytes: 1024}
		r.NoError(rec.checkDeclaredSize(fakeSizeAwareBlob{size: 512}))
	})

	t.Run("unknown size falls through to the LimitedReader cap", func(t *testing.T) {
		r := require.New(t)
		rec := &Reconciler{MaxResourceSizeBytes: 1024}
		r.NoError(rec.checkDeclaredSize(fakeSizeAwareBlob{size: blob.SizeUnknown}))
	})

	t.Run("blob without a declared size is not pre-checked", func(t *testing.T) {
		r := require.New(t)
		rec := &Reconciler{MaxResourceSizeBytes: 1024}
		r.NoError(rec.checkDeclaredSize(fakeUnsizedBlob{}))
	})

	t.Run("no limit disables the pre-check", func(t *testing.T) {
		r := require.New(t)
		rec := &Reconciler{MaxResourceSizeBytes: 0}
		r.NoError(rec.checkDeclaredSize(fakeSizeAwareBlob{size: 1 << 30}))
	})
}

// ---- Helm/v1 access detection and parsing tests ----

func TestIsHelmRepositoryAccess(t *testing.T) {
	cases := []struct {
		name       string
		accessType string
		want       bool
	}{
		{"Helm/v1", "Helm/v1", true},
		{"helm/v1", "helm/v1", true},
		{"Helm (unversioned)", "Helm", true},
		{"helm (unversioned)", "helm", true},
		{"OciArtifact/v1", "OciArtifact/v1", false},
		{"LocalBlob/v1", "LocalBlob/v1", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			r.Equal(tc.want, isHelmRepositoryAccess(tc.accessType))
		})
	}
}

func TestParseHelmRepoAccess(t *testing.T) {
	t.Run("chart name and version split from helmChart field", func(t *testing.T) {
		r := require.New(t)
		raw := []byte(`{"type":"Helm/v1","helmRepository":"https://charts.example.com","helmChart":"podinfo:6.9.1"}`)
		repoURL, chartName, chartVersion, ok := parseHelmRepoAccess(raw)
		r.True(ok)
		r.Equal("https://charts.example.com", repoURL)
		r.Equal("podinfo", chartName)
		r.Equal("6.9.1", chartVersion)
	})

	t.Run("version in separate field takes precedence", func(t *testing.T) {
		r := require.New(t)
		raw := []byte(`{"type":"Helm/v1","helmRepository":"https://charts.example.com","helmChart":"podinfo","version":"6.9.1"}`)
		repoURL, chartName, chartVersion, ok := parseHelmRepoAccess(raw)
		r.True(ok)
		r.Equal("https://charts.example.com", repoURL)
		r.Equal("podinfo", chartName)
		r.Equal("6.9.1", chartVersion)
	})

	t.Run("missing helmRepository returns not ok", func(t *testing.T) {
		r := require.New(t)
		raw := []byte(`{"type":"Helm/v1","helmChart":"podinfo:6.9.1"}`)
		_, _, _, ok := parseHelmRepoAccess(raw)
		r.False(ok)
	})

	t.Run("missing helmChart returns not ok", func(t *testing.T) {
		r := require.New(t)
		raw := []byte(`{"type":"Helm/v1","helmRepository":"https://charts.example.com"}`)
		_, _, _, ok := parseHelmRepoAccess(raw)
		r.False(ok)
	})

	t.Run("invalid JSON returns not ok", func(t *testing.T) {
		r := require.New(t)
		_, _, _, ok := parseHelmRepoAccess([]byte(`{invalid`))
		r.False(ok)
	})

	t.Run("helm legacy type parses correctly", func(t *testing.T) {
		r := require.New(t)
		raw := []byte(`{"type":"helm/v1","helmRepository":"https://charts.example.com","helmChart":"podinfo:6.9.1"}`)
		repoURL, chartName, chartVersion, ok := parseHelmRepoAccess(raw)
		r.True(ok)
		r.Equal("https://charts.example.com", repoURL)
		r.Equal("podinfo", chartName)
		r.Equal("6.9.1", chartVersion)
	})
}
