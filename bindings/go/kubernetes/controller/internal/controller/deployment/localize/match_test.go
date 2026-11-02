package localize

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	localizationv1alpha1 "ocm.software/open-component-model/bindings/go/descriptor/runtime/labels/localization/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
)

// makeLocation builds an ImageLocation from explicit original coords and relocated ref.
// MatchKey is derived via NormalizeMatchKey so it exercises the real normalization path.
func makeLocation(t *testing.T, origRegistry, origRepo, origTag, origDigest, newRepo, newTag, newDigest string) ImageLocation {
	t.Helper()
	lv := &localizationv1alpha1.Value{
		Type: "ociImage",
		MatchKey: localizationv1alpha1.MatchKey{
			Registry:   origRegistry,
			Repository: origRepo,
			Tag:        origTag,
			Digest:     origDigest,
		},
	}
	mk, err := NormalizeMatchKey(lv)
	require.NoError(t, err, "NormalizeMatchKey")
	return ImageLocation{
		Repository: newRepo,
		Tag:        newTag,
		Digest:     newDigest,
		MatchKey:   mk,
		LabelValue: lv,
	}
}

func podinfoLocation(t *testing.T) ImageLocation {
	t.Helper()
	return makeLocation(t,
		"ghcr.io", "stefanprodan/podinfo", "6.9.1", "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
		"localhost:5000/stefanprodan/podinfo", "6.9.1", "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
	)
}

func redisLocation(t *testing.T) ImageLocation {
	t.Helper()
	return makeLocation(t,
		"docker.io", "library/redis", "7.0.7", "sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498",
		"localhost:5000/library/redis", "7.0.7", "sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498",
	)
}

func yamlValues(t *testing.T, v map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func TestApplyHelmLocalization(t *testing.T) {
	tests := []struct {
		name         string
		values       map[string]any
		locations    func(t *testing.T) []ImageLocation
		wantValues   map[string]any
		wantApplied  int  // number of AppliedLocalizations expected
		wantDigitPin bool // true if any applied has DigestPinned
	}{
		{
			// When repository embeds the registry ("ghcr.io/stefanprodan/podinfo") and there
			// is no separate "registry" key, the substitution keeps the combined form:
			// repository gets the full "relocated-registry/path" so that chart templates
			// of the form `{{ .Values.image.repository }}:{{ .Values.image.tag }}` work.
			name: "tag-only map registry embedded in repository",
			values: map[string]any{
				"image":        map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"},
				"replicaCount": 2,
			},
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return []ImageLocation{podinfoLocation(t)}
			},
			wantValues: map[string]any{
				// No "registry" key — combined form preserved.
				"image":        map[string]any{"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1"},
				"replicaCount": float64(2),
			},
			wantApplied: 1,
		},
		{
			// Bare-name "redis" normalizes to docker.io/library/redis; original has no
			// "registry" key so the combined form is preserved in repository.
			name: "bare-name redis at nested path",
			values: map[string]any{
				"redis": map[string]any{"repository": "redis", "tag": "7.0.7", "enabled": true},
			},
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return []ImageLocation{redisLocation(t)}
			},
			wantValues: map[string]any{
				// No "registry" key — combined form preserved.
				"redis": map[string]any{"repository": "localhost:5000/library/redis", "tag": "7.0.7", "enabled": true},
			},
			wantApplied: 1,
		},
		{
			name: "digest key present pins the digest",
			values: map[string]any{
				"image": map[string]any{
					"repository": "ghcr.io/stefanprodan/podinfo",
					"tag":        "6.9.1",
					"digest":     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
				},
			},
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return []ImageLocation{podinfoLocation(t)}
			},
			wantApplied:  1,
			wantDigitPin: true,
		},
		{
			name: "split registry key",
			values: map[string]any{
				"image": map[string]any{"registry": "ghcr.io", "repository": "stefanprodan/podinfo", "tag": "6.9.1"},
			},
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return []ImageLocation{podinfoLocation(t)}
			},
			wantValues: map[string]any{
				"image": map[string]any{"registry": "localhost:5000", "repository": "stefanprodan/podinfo", "tag": "6.9.1"},
			},
			wantApplied: 1,
		},
		{
			// Scalar image refs include the digest when the hint carries one.
			name:   "scalar reference includes digest",
			values: map[string]any{"sidecar": "ghcr.io/stefanprodan/podinfo:6.9.1"},
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return []ImageLocation{podinfoLocation(t)}
			},
			wantValues: map[string]any{
				"sidecar": "localhost:5000/stefanprodan/podinfo:6.9.1@sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
			},
			wantApplied: 1,
		},
		{
			name:   "unmatched location is skipped",
			values: map[string]any{"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"}},
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return []ImageLocation{redisLocation(t)}
			},
			wantValues:  map[string]any{"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"}},
			wantApplied: 0,
		},
		{
			name:   "empty inputs return empty result",
			values: nil,
			locations: func(t *testing.T) []ImageLocation {
				t.Helper()
				return nil
			},
			wantApplied: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			raw := yamlValues(t, tc.values)
			result, err := ApplyHelmLocalization(raw, tc.locations(t))
			r.NoError(err)
			r.Len(result.AppliedLocalizations, tc.wantApplied)
			if tc.wantValues != nil {
				r.Equal(tc.wantValues, result.Values)
			}
			if tc.wantDigitPin {
				r.True(result.AppliedLocalizations[0].DigestPinned, "expected DigestPinned=true")
			}
		})
	}
}

// TestApplyHelmLocalizationBareNameScalar verifies that a scalar image reference with no
// registry component (e.g. "redis:7.0.7", which normalizes to docker.io/library/redis) is
// recognized by looksLikeOCIRef and rewritten to the relocated reference.
func TestApplyHelmLocalizationBareNameScalar(t *testing.T) {
	r := require.New(t)
	values := map[string]any{
		"image": "redis:7.0.7",
	}
	result, err := ApplyHelmLocalization(yamlValues(t, values), []ImageLocation{redisLocation(t)})
	r.NoError(err)
	r.Len(result.AppliedLocalizations, 1, "bare-name scalar must be localized")
	got, _ := result.Values["image"].(string)
	r.Equal("localhost:5000/library/redis:7.0.7@sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498", got)
}

// TestApplyHelmLocalizationBareNameImageBlock verifies that an image block whose "image" key is a
// bare name (e.g. {"image": "redis:7.0.7"}, no registry/"/"), is recognized and rewritten, matching
// the ADR's statement that "image" is a full alias for "repository" with no form restriction.
func TestApplyHelmLocalizationBareNameImageBlock(t *testing.T) {
	r := require.New(t)
	values := map[string]any{
		"redis": map[string]any{"image": "redis:7.0.7", "enabled": true},
	}
	result, err := ApplyHelmLocalization(yamlValues(t, values), []ImageLocation{redisLocation(t)})
	r.NoError(err)
	r.Len(result.AppliedLocalizations, 1, "bare-name image block must be localized")
	block, _ := result.Values["redis"].(map[string]any)
	// The relocated reference is written back into the "image" key (its alias role), with the
	// digest pinned; no competing "repository" key is introduced.
	r.Equal("localhost:5000/library/redis:7.0.7@sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498", block["image"])
	_, hasRepo := block["repository"]
	r.False(hasRepo, "must not introduce a competing repository key")
	r.Equal(true, block["enabled"])
}

// TestApplyHelmLocalizationDuplicateKeyLastWins verifies that when two locations
// share the same MatchKey, the last one wins (index overwrite; no conflict error).
// A conflict error only fires when the *same value slot* resolves to two different
// targets in the same walk, which cannot happen with an overwritten index.
func TestApplyHelmLocalizationDuplicateKeyLastWins(t *testing.T) {
	r := require.New(t)
	loc1 := podinfoLocation(t)
	loc2 := makeLocation(t,
		"ghcr.io", "stefanprodan/podinfo", "6.9.1", "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
		"other-registry:5000/stefanprodan/podinfo", "6.9.1", "",
	)
	values := map[string]any{
		"image": map[string]any{"registry": "ghcr.io", "repository": "stefanprodan/podinfo", "tag": "6.9.1"},
	}
	result, err := ApplyHelmLocalization(yamlValues(t, values), []ImageLocation{loc1, loc2})
	r.NoError(err, "duplicate MatchKey is last-write-wins, not an error")
	// loc2 wins; registry = other-registry:5000.
	registry, _ := result.Values["image"].(map[string]any)["registry"].(string)
	r.Equal("other-registry:5000", registry)
}

// TestApplyHelmLocalizationConflict verifies that when the same value slot is resolved
// twice to two different targets (possible when AppliedLocalizations carries a prior record
// from a different slot with the same normalized key), the conflict error fires.
// This exercises the M1 fix: applied.MatchKey is stored as a raw ref and must be normalized
// before comparison; without normalization the check always misses and no error is returned.
func TestApplyHelmLocalizationConflict(t *testing.T) {
	r := require.New(t)

	// Build an index with one entry for stefanprodan/podinfo → registry-A.
	loc := makeLocation(t,
		"ghcr.io", "stefanprodan/podinfo", "6.9.1", "",
		"registry-a:5000/stefanprodan/podinfo", "6.9.1", "",
	)
	index := map[string]ImageLocation{loc.MatchKey: loc}

	// Pre-populate AppliedLocalizations as if a prior substitution for the same normalized key
	// had already run but resolved to a *different* target (registry-b).
	result := &HelmResult{
		AppliedLocalizations: []v1alpha1.AppliedLocalization{
			{
				// raw reference form — the bug was that this was compared directly to
				// the normalized matchKey, so it never matched.
				MatchKey: "ghcr.io/stefanprodan/podinfo:6.9.1",
				Applied:  "registry-b:5000/stefanprodan/podinfo:6.9.1",
			},
		},
	}

	parent := map[string]any{}
	parsed, err := parseNormalized("ghcr.io/stefanprodan/podinfo:6.9.1")
	r.NoError(err)

	err = substituteScalar(parent, "image", "ghcr.io/stefanprodan/podinfo:6.9.1", parsed, index, result)
	r.Error(err, "conflict: same normalized key, different Applied targets")
	r.Contains(err.Error(), "localization conflict")
}

func TestBuildKustomizeImages(t *testing.T) {
	r := require.New(t)
	locs := []ImageLocation{podinfoLocation(t), redisLocation(t)}
	got := BuildKustomizeImages(locs)
	r.Len(got, 2)
	// Name = MatchKey (normalized canonical ref incl. tag); index by NewName which is stable.
	byNewName := map[string]KustomizeImage{}
	for _, img := range got {
		byNewName[img.NewName] = img
	}

	podinfo := byNewName["localhost:5000/stefanprodan/podinfo"]
	r.NotEmpty(podinfo.Name, "Name (MatchKey) must be set")
	r.Equal("6.9.1", podinfo.NewTag)
	r.Equal("sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb", podinfo.Digest)

	redis := byNewName["localhost:5000/library/redis"]
	r.NotEmpty(redis.Name, "Name (MatchKey) must be set")
	r.Equal("7.0.7", redis.NewTag)
	r.Equal("sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498", redis.Digest)
}

func TestImageLocationsFromLabels(t *testing.T) {
	r := require.New(t)

	lv := localizationv1alpha1.Value{
		Type: "ociImage",
		MatchKey: localizationv1alpha1.MatchKey{
			Registry:   "ghcr.io",
			Repository: "stefanprodan/podinfo",
			Tag:        "6.9.1",
			Digest:     "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
		},
	}
	raw, err := json.Marshal(lv)
	r.NoError(err)

	labels := []v1alpha1.Label{
		{
			Name:    "unrelated-label",
			Version: "v1",
			Value:   apiextensionsv1.JSON{Raw: []byte(`"ignored"`)},
		},
		{
			Name:    localizationv1alpha1.LabelName,
			Version: localizationv1alpha1.Version,
			Value:   apiextensionsv1.JSON{Raw: raw},
		},
	}

	resolvedRef := "localhost:5000/stefanprodan/podinfo:6.9.1@sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb"
	locs, err := ImageLocationsFromLabels(labels, resolvedRef)
	r.NoError(err)
	r.Len(locs, 1, "only the localization label yields a location")

	loc := locs[0]
	r.Equal("localhost:5000/stefanprodan/podinfo", loc.Repository)
	// Label digest takes precedence over resolvedRef digest.
	r.Equal("sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb", loc.Digest)
	wantMK, err := NormalizeMatchKey(&lv)
	r.NoError(err)
	r.Equal(wantMK, loc.MatchKey)
}

func TestImageLocationsFromLabelsBadJSON(t *testing.T) {
	r := require.New(t)
	labels := []v1alpha1.Label{
		{
			Name:  localizationv1alpha1.LabelName,
			Value: apiextensionsv1.JSON{Raw: []byte(`not json`)},
		},
	}
	_, err := ImageLocationsFromLabels(labels, "localhost:5000/x:latest")
	r.Error(err)
}

func TestParseResolvedRef(t *testing.T) {
	tests := []struct {
		input        string
		wantRegistry string
		wantPath     string
		wantTag      string
		wantDigest   string
	}{
		{
			"registry:5000/path/image:tag@sha256:abc",
			"registry:5000", "path/image", "tag", "sha256:abc",
		},
		{
			"registry:5000/path/image:tag",
			"registry:5000", "path/image", "tag", "",
		},
		{
			"registry:5000/path/image@sha256:abc",
			"registry:5000", "path/image", "", "sha256:abc",
		},
		{
			"image",
			"", "image", "", "",
		},
		{
			"ocm-e2e-image-registry:5000/stefanprodan/charts/podinfo:6.9.1@sha256:565d",
			"ocm-e2e-image-registry:5000", "stefanprodan/charts/podinfo", "6.9.1", "sha256:565d",
		},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			r := require.New(t)
			reg, path, tag, digest := parseResolvedRef(tc.input)
			r.Equal(tc.wantRegistry, reg, "registry")
			r.Equal(tc.wantPath, path, "path")
			r.Equal(tc.wantTag, tag, "tag")
			r.Equal(tc.wantDigest, digest, "digest")
		})
	}
}

// TestApplyHelmLocalizationNoRecurseIntoUnmatchedBlock verifies that when an image-block map
// matches no localization label, the walk does NOT recurse into the block's children.
// Before fix C, the nested sidecar.repository would be localized because the outer unmatched
// block recursed and the inner sidecar sub-block was matched.
func TestApplyHelmLocalizationNoRecurseIntoUnmatchedBlock(t *testing.T) {
	r := require.New(t)
	// Outer block: repository="ghcr.io/other/unmatched" — no label for this.
	// Nested block: sidecar.repository="ghcr.io/stefanprodan/podinfo" — has a label, but must
	// NOT be reached because the outer block is a single image spec (no recursion into it).
	values := map[string]any{
		"app": map[string]any{
			"repository": "ghcr.io/other/unmatched",
			"tag":        "1.0",
			"sidecar": map[string]any{
				"repository": "ghcr.io/stefanprodan/podinfo",
				"tag":        "6.9.1",
			},
		},
	}
	result, err := ApplyHelmLocalization(yamlValues(t, values), []ImageLocation{podinfoLocation(t)})
	r.NoError(err)
	// No substitution must have happened: the outer block matched no label and recursion stopped.
	r.Empty(result.AppliedLocalizations, "nested sidecar block must not be localized")
	// Outer block values are unchanged.
	appBlock, _ := result.Values["app"].(map[string]any)
	r.Equal("ghcr.io/other/unmatched", appBlock["repository"])
}

// TestBuildKustomizeImagesSorted verifies that BuildKustomizeImages returns entries sorted by Name.
func TestBuildKustomizeImagesSorted(t *testing.T) {
	r := require.New(t)
	// Pass locations in reverse alphabetical Name order: redis > podinfo.
	locs := []ImageLocation{redisLocation(t), podinfoLocation(t)}
	got := BuildKustomizeImages(locs)
	r.Len(got, 2)
	// Regardless of input order, output must be sorted: podinfo (ghcr.io) < redis (docker.io/library/redis).
	r.Less(got[0].Name, got[1].Name, "images must be sorted by Name: got[0]=%q got[1]=%q", got[0].Name, got[1].Name)
}

// TestCollectValuesMatchKeys verifies the Helm usage pre-filter helper: it reports the canonical
// match-keys of every image reference in values.yaml (scalar, split block, bare-name block), and
// ignores non-image values. The returned keys use the same normalization ApplyHelmLocalization
// matches on, so a key present here guarantees ApplyHelmLocalization would substitute it.
func TestCollectValuesMatchKeys(t *testing.T) {
	r := require.New(t)
	values := map[string]any{
		"image":   "ghcr.io/stefanprodan/podinfo:6.9.1",                               // scalar
		"redis":   map[string]any{"image": "redis:7.0.7"},                             // bare-name block → familiar "redis"
		"sidecar": map[string]any{"repository": "quay.io/acme/sidecar", "tag": "1.0"}, // split block
		"ui":      map[string]any{"color": "#326ce5"},                                 // not an image
		"count":   3,
	}
	keys, err := CollectValuesMatchKeys(yamlValues(t, values))
	r.NoError(err)
	r.Contains(keys, "ghcr.io/stefanprodan/podinfo")
	r.Contains(keys, "redis", "bare docker.io/library/redis normalizes to the familiar name 'redis'")
	r.Contains(keys, "quay.io/acme/sidecar")
	r.NotContains(keys, "#326ce5")
	r.Len(keys, 3, "only the three image references should be collected")
}

// TestCollectValuesMatchKeysEmpty verifies empty/absent values yield an empty (non-nil) set.
func TestCollectValuesMatchKeysEmpty(t *testing.T) {
	r := require.New(t)
	keys, err := CollectValuesMatchKeys(nil)
	r.NoError(err)
	r.NotNil(keys)
	r.Empty(keys)
}

// TestImageBlockConflict verifies that substituteImageBlock returns a "localization conflict"
// error when two map-shaped image blocks with the same normalized matchKey resolve to different
// relocated locations (the conflict branch at match.go:222-231).
func TestImageBlockConflict(t *testing.T) {
	r := require.New(t)

	// Two index entries with the same registry+repo but different relocated targets.
	loc := makeLocation(t,
		"ghcr.io", "stefanprodan/podinfo", "6.9.1", "",
		"registry-a:5000/stefanprodan/podinfo", "6.9.1", "",
	)
	index := map[string]ImageLocation{loc.MatchKey: loc}

	// Pre-populate AppliedLocalizations as if a prior map-block substitution already ran
	// for the same normalised key but landed on registry-b.
	result := &HelmResult{
		AppliedLocalizations: []v1alpha1.AppliedLocalization{
			{
				MatchKey: "ghcr.io/stefanprodan/podinfo:6.9.1",
				Applied:  "registry-b:5000/stefanprodan/podinfo:6.9.1",
			},
		},
	}

	parent := map[string]any{}
	block := map[string]any{
		"registry":   "ghcr.io",
		"repository": "stefanprodan/podinfo",
		"tag":        "6.9.1",
	}

	err := substituteImageBlock(parent, "image", []string{"image"}, block, index, result)
	r.Error(err, "should return a conflict error for map-block with same key but different target")
	r.Contains(err.Error(), "localization conflict")
}

// TestTagOnlyDigestPinnedFalse verifies that when an image location has an empty Digest,
// the AppliedLocalization record carries DigestPinned=false (match.go:240).
func TestTagOnlyDigestPinnedFalse(t *testing.T) {
	r := require.New(t)

	// Tag-only location: no digest set.
	loc := makeLocation(t,
		"ghcr.io", "stefanprodan/podinfo", "6.9.1", "",
		"localhost:5000/stefanprodan/podinfo", "6.9.1", "", // empty newDigest
	)
	locations := []ImageLocation{loc}

	values := yamlValues(t, map[string]any{
		"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"},
	})

	result, err := ApplyHelmLocalization(values, locations)
	r.NoError(err)
	r.Len(result.AppliedLocalizations, 1)
	r.False(result.AppliedLocalizations[0].DigestPinned, "tag-only location must yield DigestPinned=false")
}
