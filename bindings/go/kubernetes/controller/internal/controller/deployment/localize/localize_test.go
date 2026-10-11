package localize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseImageNormalization(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want Image
	}{
		{"embedded registry", "ghcr.io/stefanprodan/podinfo:6.9.1", Image{"ghcr.io", "stefanprodan/podinfo", "6.9.1", ""}},
		{"docker user repo defaults registry", "stefanprodan/podinfo", Image{"docker.io", "stefanprodan/podinfo", "", ""}},
		{"bare name gets library prefix", "redis", Image{"docker.io", "library/redis", "", ""}},
		{"index.docker.io equivalence", "index.docker.io/library/redis:7.0.7", Image{"docker.io", "library/redis", "7.0.7", ""}},
		{"tag and digest", "docker.io/library/redis:7.0.7@sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498",
			Image{"docker.io", "library/redis", "7.0.7", "sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498"}},
		{"local registry with port", "localhost:5000/stefanprodan/podinfo:6.9.1", Image{"localhost:5000", "stefanprodan/podinfo", "6.9.1", ""}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			got, err := ParseImage(tc.ref)
			r.NoError(err)
			r.Equal(tc.want, got)
		})
	}
}

func TestParseLabel(t *testing.T) {
	r := require.New(t)
	raw := []byte(`{"type":"ociImage","original":{"registry":"ghcr.io","repository":"stefanprodan/podinfo","tag":"6.9.1","digest":"sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb"}}`)
	img, err := ParseLabel(raw)
	r.NoError(err)
	r.Equal(Image{"ghcr.io", "stefanprodan/podinfo", "6.9.1", "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb"}, img)
}

// podinfoHint and redisHint mirror the example: upstream originals relocated to
// a local registry.
func podinfoHint() Hint {
	return Hint{
		Original: Image{"ghcr.io", "stefanprodan/podinfo", "6.9.1", "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb"},
		Current:  Image{"localhost:5000", "stefanprodan/podinfo", "6.9.1", "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb"},
	}
}

func redisHint() Hint {
	return Hint{
		Original: Image{"docker.io", "library/redis", "7.0.7", "sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498"},
		Current:  Image{"localhost:5000", "library/redis", "7.0.7", "sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498"},
	}
}

func TestHelmMatching(t *testing.T) {
	tests := []struct {
		name         string
		values       map[string]any
		hints        []Hint
		wantOverride map[string]any
		wantApplied  []Applied
	}{
		{
			name: "podinfo tag-only map (registry embedded in repository, no digest key)",
			values: map[string]any{
				"image":        map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"},
				"replicaCount": 2,
			},
			hints:        []Hint{podinfoHint()},
			wantOverride: map[string]any{"image": map[string]any{"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1"}},
			wantApplied:  []Applied{{Original: "ghcr.io/stefanprodan/podinfo:6.9.1", Applied: "localhost:5000/stefanprodan/podinfo:6.9.1", DigestPinned: false}},
		},
		{
			name: "bare-name redis at a different path",
			values: map[string]any{
				"redis": map[string]any{"repository": "redis", "tag": "7.0.7", "enabled": true},
			},
			hints:        []Hint{redisHint()},
			wantOverride: map[string]any{"redis": map[string]any{"repository": "localhost:5000/library/redis", "tag": "7.0.7"}},
			wantApplied:  []Applied{{Original: "docker.io/library/redis:7.0.7", Applied: "localhost:5000/library/redis:7.0.7", DigestPinned: false}},
		},
		{
			name: "digest key present pins the digest",
			values: map[string]any{
				"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1", "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
			},
			hints: []Hint{podinfoHint()},
			wantOverride: map[string]any{"image": map[string]any{
				"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1",
				"digest": "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
			}},
			wantApplied: []Applied{{
				Original:     "ghcr.io/stefanprodan/podinfo:6.9.1",
				Applied:      "localhost:5000/stefanprodan/podinfo:6.9.1@sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
				DigestPinned: true,
			}},
		},
		{
			name: "split registry key",
			values: map[string]any{
				"image": map[string]any{"registry": "ghcr.io", "repository": "stefanprodan/podinfo", "tag": "6.9.1"},
			},
			hints:        []Hint{podinfoHint()},
			wantOverride: map[string]any{"image": map[string]any{"registry": "localhost:5000", "repository": "stefanprodan/podinfo", "tag": "6.9.1"}},
			wantApplied:  []Applied{{Original: "ghcr.io/stefanprodan/podinfo:6.9.1", Applied: "localhost:5000/stefanprodan/podinfo:6.9.1", DigestPinned: false}},
		},
		{
			name:         "scalar reference",
			values:       map[string]any{"sidecar": "ghcr.io/stefanprodan/podinfo:6.9.1"},
			hints:        []Hint{podinfoHint()},
			wantOverride: map[string]any{"sidecar": "localhost:5000/stefanprodan/podinfo:6.9.1"},
			wantApplied:  []Applied{{Original: "ghcr.io/stefanprodan/podinfo:6.9.1", Applied: "localhost:5000/stefanprodan/podinfo:6.9.1", DigestPinned: false}},
		},
		{
			name:         "unmatched hint is skipped",
			values:       map[string]any{"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"}},
			hints:        []Hint{redisHint()},
			wantOverride: nil,
			wantApplied:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			override, applied, err := Helm(tc.values, tc.hints)
			r.NoError(err)
			r.Equal(tc.wantOverride, override)
			r.Equal(tc.wantApplied, applied)
		})
	}
}

func TestHelmConflict(t *testing.T) {
	r := require.New(t)
	conflicting := Hint{
		Original: podinfoHint().Original,
		Current:  Image{"other:5000", "stefanprodan/podinfo", "6.9.1", ""},
	}
	_, _, err := Helm(
		map[string]any{"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"}},
		[]Hint{podinfoHint(), conflicting},
	)
	r.ErrorContains(err, "conflicting localizations")
}

func TestKustomize(t *testing.T) {
	r := require.New(t)
	got := Kustomize([]Hint{podinfoHint(), redisHint()})
	r.Equal([]KustomizeImage{
		{Name: "docker.io/library/redis", NewName: "localhost:5000/library/redis", NewTag: "7.0.7", Digest: "sha256:bb474c35022ca2c5618f4c49ca759bd2c0eea1daf5d934c560bd30092b97b498"},
		{Name: "ghcr.io/stefanprodan/podinfo", NewName: "localhost:5000/stefanprodan/podinfo", NewTag: "6.9.1", Digest: "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb"},
	}, got)
}
