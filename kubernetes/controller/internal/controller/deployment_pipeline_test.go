package controller

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	deliveryv1alpha1 "ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

func TestPipelineNaming(t *testing.T) {
	p := &OCMPipeline{
		RepoURL:      "http://image-registry:5000",
		RepoType:     "OCIRegistry",
		RepoInterval: "10m",
		Component:    "ocm.software/example",
		Semver:       "1.0.0",
		CompInterval: "10m",
		ResourceID:   map[string]string{"name": "helm-resource"},
	}

	repoName := p.RepoName("my-deployment")
	compName := p.CompName("my-deployment")
	resName := p.ResName("my-deployment")

	// Names should be deterministic.
	assert.Equal(t, repoName, p.RepoName("my-deployment"))
	assert.Equal(t, compName, p.CompName("my-deployment"))
	assert.Equal(t, resName, p.ResName("my-deployment"))

	// Names should be different.
	assert.NotEqual(t, repoName, compName)
	assert.NotEqual(t, compName, resName)

	// Names should be within 63 char limit.
	assert.LessOrEqual(t, len(repoName), 63)
	assert.LessOrEqual(t, len(compName), 63)
	assert.LessOrEqual(t, len(resName), 63)
}

func TestBuildAdditionalStatusFields(t *testing.T) {
	fields := []string{"digest", "registry", "repository"}
	data, err := buildAdditionalStatusFields(fields)
	require.NoError(t, err)

	expected := `{"digest":"resource.access.toOCI().digest","registry":"resource.access.toOCI().registry","repository":"resource.access.toOCI().repository"}`
	assert.JSONEq(t, expected, string(data))
}

func TestExtractExpressions_NestedBraces(t *testing.T) {
	s := `oci://${repoByRef("http://example.com").componentByReference("comp").resourceByIdentity({"name": "res"}).toOCI().registry}/path`
	exprs := extractExpressions(s)
	require.Len(t, exprs, 1)
	assert.Equal(t, `repoByRef("http://example.com").componentByReference("comp").resourceByIdentity({"name": "res"}).toOCI().registry`, exprs[0])
}

func TestExtractExpressions_Multiple(t *testing.T) {
	s := `oci://${repoByRef("url").componentByReference("c").resourceByIdentity({"name": "r"}).toOCI().registry}/${repoByRef("url").componentByReference("c").resourceByIdentity({"name": "r"}).toOCI().repository}`
	exprs := extractExpressions(s)
	require.Len(t, exprs, 2)
	assert.Contains(t, exprs[0], "toOCI().registry")
	assert.Contains(t, exprs[1], "toOCI().repository")
}

func TestExtractExpressions_Simple(t *testing.T) {
	s := `${ocirepository.metadata.name}`
	exprs := extractExpressions(s)
	require.Len(t, exprs, 1)
	assert.Equal(t, "ocirepository.metadata.name", exprs[0])
}

func TestExtractExpressions_NoMatch(t *testing.T) {
	s := `no expressions here`
	exprs := extractExpressions(s)
	assert.Empty(t, exprs)
}

func TestIsStandaloneExpression(t *testing.T) {
	tests := []struct {
		s        string
		expected bool
	}{
		{`${ocirepository.metadata.name}`, true},
		{`${repoByRef("url").componentByReference("c").resourceByIdentity({"name": "r"}).toOCI().registry}`, true},
		{`oci://${expr}/path`, false},
		{`no expression`, false},
		{`${a}${b}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			assert.Equal(t, tt.expected, isStandaloneExpression(tt.s))
		})
	}
}

func TestPipelineKeys(t *testing.T) {
	p1 := &OCMPipeline{
		RepoURL:      "http://example.com",
		RepoType:     "OCIRegistry",
		RepoInterval: "10m",
		Component:    "comp",
		Semver:       "1.0.0",
		CompInterval: "10m",
		ResourceID:   map[string]string{"name": "res"},
	}

	p2 := &OCMPipeline{
		RepoURL:      "http://example.com",
		RepoType:     "OCIRegistry",
		RepoInterval: "10m",
		Component:    "comp",
		Semver:       "1.0.0",
		CompInterval: "10m",
		ResourceID:   map[string]string{"name": "res"},
	}

	// Same parameters should produce the same keys.
	assert.Equal(t, p1.RepoKey(), p2.RepoKey())
	assert.Equal(t, p1.CompKey(), p2.CompKey())
	assert.Equal(t, p1.ResKey(), p2.ResKey())

	// Different resource identity should produce different ResKey.
	p3 := &OCMPipeline{
		RepoURL:      "http://example.com",
		RepoType:     "OCIRegistry",
		RepoInterval: "10m",
		Component:    "comp",
		Semver:       "1.0.0",
		CompInterval: "10m",
		ResourceID:   map[string]string{"name": "other"},
	}
	assert.Equal(t, p1.RepoKey(), p3.RepoKey())
	assert.Equal(t, p1.CompKey(), p3.CompKey())
	assert.NotEqual(t, p1.ResKey(), p3.ResKey())
}

func TestParsePipelineChain_FullChain(t *testing.T) {
	expr := `repoByRef("http://image-registry:5000", {"type": "OCIRegistry", "interval": "10m"}).componentByReference("ocm.software/example/comp", {"semver": "1.0.0", "interval": "10m"}).resourceByIdentity({"name": "helm-resource"}).toOCI().registry`
	pipeline, ok := parsePipelineChain(expr)
	require.True(t, ok)
	assert.Equal(t, "http://image-registry:5000", pipeline.RepoURL)
	assert.Equal(t, "OCIRegistry", pipeline.RepoType)
	assert.Equal(t, "10m", pipeline.RepoInterval)
	assert.Equal(t, "ocm.software/example/comp", pipeline.Component)
	assert.Equal(t, "1.0.0", pipeline.Semver)
	assert.Equal(t, "10m", pipeline.CompInterval)
}

func TestParsePipelineChain_MinimalArgs(t *testing.T) {
	expr := `repoByRef("http://example.com").componentByReference("comp").resourceByIdentity({"name": "r"}).toOCI().digest`
	pipeline, ok := parsePipelineChain(expr)
	require.True(t, ok)
	assert.Equal(t, "http://example.com", pipeline.RepoURL)
	assert.Equal(t, "OCIRegistry", pipeline.RepoType) // default
	assert.Equal(t, "10m", pipeline.RepoInterval)     // default
	assert.Equal(t, "comp", pipeline.Component)
	assert.Equal(t, ">=0.0.0", pipeline.Semver)    // default
	assert.Equal(t, "10m", pipeline.CompInterval)  // default
}

func TestParsePipelineChain_NoRepo(t *testing.T) {
	expr := `componentByReference("comp").resourceByIdentity({"name": "r"}).toOCI().digest`
	_, ok := parsePipelineChain(expr)
	assert.False(t, ok)
}

func TestParsePipelineChain_NoComponent(t *testing.T) {
	expr := `repoByRef("http://example.com").resourceByIdentity({"name": "r"}).toOCI().digest`
	_, ok := parsePipelineChain(expr)
	assert.False(t, ok)
}

func TestParsePipelineChain_CrossResourceRef(t *testing.T) {
	expr := `ocirepository.metadata.name`
	_, ok := parsePipelineChain(expr)
	assert.False(t, ok)
}

func TestResourceIndex_Lookup(t *testing.T) {
	idx := NewResourceIndex()
	idx.Add(ResolvedResourceEntry{
		Pipeline:   &OCMPipeline{Component: "comp-a"},
		Component:  "comp-a",
		ResourceID: map[string]string{"name": "helm-resource"},
	})
	idx.Add(ResolvedResourceEntry{
		Pipeline:   &OCMPipeline{Component: "comp-a"},
		Component:  "comp-a",
		ResourceID: map[string]string{"name": "image-resource"},
	})

	entry, err := idx.Lookup("helm-resource")
	require.NoError(t, err)
	assert.Equal(t, "comp-a", entry.Component)

	entry, err = idx.Lookup("image-resource")
	require.NoError(t, err)
	assert.Equal(t, "comp-a", entry.Component)
}

func TestResourceIndex_LookupNotFound(t *testing.T) {
	idx := NewResourceIndex()
	_, err := idx.Lookup("nonexistent")
	assert.ErrorContains(t, err, "not found")
}

func TestResourceIndex_LookupAmbiguous(t *testing.T) {
	idx := NewResourceIndex()
	idx.Add(ResolvedResourceEntry{
		Pipeline:   &OCMPipeline{Component: "comp-a"},
		Component:  "comp-a",
		ResourceID: map[string]string{"name": "shared-res"},
	})
	idx.Add(ResolvedResourceEntry{
		Pipeline:   &OCMPipeline{Component: "comp-b"},
		Component:  "comp-b",
		ResourceID: map[string]string{"name": "shared-res"},
	})

	_, err := idx.Lookup("shared-res")
	assert.ErrorContains(t, err, "multiple components")
	assert.ErrorContains(t, err, "comp-a")
	assert.ErrorContains(t, err, "comp-b")
}

func TestResourceIndex_LookupForComponent(t *testing.T) {
	idx := NewResourceIndex()
	idx.Add(ResolvedResourceEntry{
		Pipeline:   &OCMPipeline{Component: "comp-a"},
		Component:  "comp-a",
		ResourceID: map[string]string{"name": "shared-res"},
	})
	idx.Add(ResolvedResourceEntry{
		Pipeline:   &OCMPipeline{Component: "comp-b"},
		Component:  "comp-b",
		ResourceID: map[string]string{"name": "shared-res"},
	})

	entry, err := idx.LookupForComponent("comp-b", "shared-res")
	require.NoError(t, err)
	assert.Equal(t, "comp-b", entry.Component)

	_, err = idx.LookupForComponent("comp-c", "shared-res")
	assert.ErrorContains(t, err, "not found in component")
}

func TestPreScanTemplates(t *testing.T) {
	resources := []deliveryv1alpha1.OCMDeploymentResource{
		{
			ID: "ociRepo",
			Template: mustJSON(map[string]any{
				"apiVersion": "source.toolkit.fluxcd.io/v1",
				"kind":       "OCIRepository",
				"spec": map[string]any{
					"url": `oci://${repoByRef("http://registry:5000", {"type": "OCIRegistry"}).componentByReference("ocm.software/comp-a", {"semver": "1.0.0"}).resourceByIdentity({"name": "helm-resource"}).toOCI().registry}`,
				},
			}),
		},
		{
			ID: "helmRelease",
			Template: mustJSON(map[string]any{
				"apiVersion": "helm.toolkit.fluxcd.io/v2",
				"kind":       "HelmRelease",
				"spec": map[string]any{
					"chartRef": map[string]any{
						"name": "${ociRepo.metadata.name}",
					},
					"values": map[string]any{
						"tag": `${resource("image-resource").toOCI().tag}`,
					},
				},
			}),
		},
	}

	pipelines, _, err := preScanTemplates(resources)
	require.NoError(t, err)
	require.Len(t, pipelines, 1)
	assert.Equal(t, "http://registry:5000", pipelines[0].RepoURL)
	assert.Equal(t, "OCIRegistry", pipelines[0].RepoType)
	assert.Equal(t, "ocm.software/comp-a", pipelines[0].Component)
	assert.Equal(t, "1.0.0", pipelines[0].Semver)
}

func TestPreScanTemplates_MultipleComponents(t *testing.T) {
	resources := []deliveryv1alpha1.OCMDeploymentResource{
		{
			ID: "res1",
			Template: mustJSON(map[string]any{
				"spec": map[string]any{
					"url": `${repoByRef("http://reg:5000").componentByReference("comp-a").resourceByIdentity({"name": "r1"}).toOCI().registry}`,
				},
			}),
		},
		{
			ID: "res2",
			Template: mustJSON(map[string]any{
				"spec": map[string]any{
					"url": `${repoByRef("http://reg:5000").componentByReference("comp-b").resourceByIdentity({"name": "r2"}).toOCI().registry}`,
				},
			}),
		},
	}

	pipelines, _, err := preScanTemplates(resources)
	require.NoError(t, err)
	require.Len(t, pipelines, 2)

	components := map[string]bool{}
	for _, p := range pipelines {
		components[p.Component] = true
	}
	assert.True(t, components["comp-a"])
	assert.True(t, components["comp-b"])
}

func TestPreScanTemplates_Deduplication(t *testing.T) {
	// Same repo+component in two different templates should be deduplicated.
	resources := []deliveryv1alpha1.OCMDeploymentResource{
		{
			ID: "res1",
			Template: mustJSON(map[string]any{
				"url": `${repoByRef("http://reg:5000").componentByReference("comp-a").resourceByIdentity({"name": "r1"}).toOCI().registry}`,
			}),
		},
		{
			ID: "res2",
			Template: mustJSON(map[string]any{
				"url": `${repoByRef("http://reg:5000").componentByReference("comp-a").resourceByIdentity({"name": "r2"}).toOCI().registry}`,
			}),
		},
	}

	pipelines, _, err := preScanTemplates(resources)
	require.NoError(t, err)
	require.Len(t, pipelines, 1)
	assert.Equal(t, "comp-a", pipelines[0].Component)
}

func TestExtractFuncArgs(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		funcName string
		wantArgs []string
		wantOK   bool
	}{
		{
			name:     "simple string arg",
			expr:     `repoByRef("http://example.com")`,
			funcName: "repoByRef",
			wantArgs: []string{`"http://example.com"`},
			wantOK:   true,
		},
		{
			name:     "string and map args",
			expr:     `repoByRef("http://example.com", {"type": "OCIRegistry"})`,
			funcName: "repoByRef",
			wantArgs: []string{`"http://example.com"`, `{"type": "OCIRegistry"}`},
			wantOK:   true,
		},
		{
			name:     "not found",
			expr:     `something("else")`,
			funcName: "repoByRef",
			wantArgs: nil,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, ok := extractFuncArgs(tt.expr, tt.funcName)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.wantArgs, args)
			}
		})
	}
}

func TestParseMapLiteral(t *testing.T) {
	result := parseMapLiteral(`{"type": "OCIRegistry", "interval": "10m"}`)
	require.NotNil(t, result)
	assert.Equal(t, "OCIRegistry", result["type"])
	assert.Equal(t, "10m", result["interval"])
}

func TestParseMapLiteral_Invalid(t *testing.T) {
	assert.Nil(t, parseMapLiteral(`not a map`))
	assert.Nil(t, parseMapLiteral(``))
}

// mustJSON marshals a value to apiextensionsv1.JSON, panicking on error.
func mustJSON(v any) apiextensionsv1.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return apiextensionsv1.JSON{Raw: b}
}
