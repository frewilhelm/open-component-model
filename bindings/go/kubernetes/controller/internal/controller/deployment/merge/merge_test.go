package merge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValuesPrecedenceChain(t *testing.T) {
	r := require.New(t)
	chart := map[string]any{
		"replicaCount": 1,
		"ui":           map[string]any{"color": "#000000", "message": ""},
	}
	central := map[string]any{"ui": map[string]any{"message": "from central", "color": "#111111"}}
	local := map[string]any{"replicaCount": 2, "ui": map[string]any{"color": "#222222"}}
	inline := map[string]any{"ui": map[string]any{"color": "#326ce5"}}

	got, err := Values(chart, nil, central, local, inline)
	r.NoError(err)
	r.Equal(map[string]any{
		"replicaCount": 2,
		"ui":           map[string]any{"color": "#326ce5", "message": "from central"},
	}, got)
}

func TestValuesLocalizationMergesIntoChartBlock(t *testing.T) {
	r := require.New(t)
	chart := map[string]any{
		"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1", "pullPolicy": "IfNotPresent"},
	}
	localization := map[string]any{
		"image": map[string]any{"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1"},
	}
	got, err := Values(chart, localization)
	r.NoError(err)
	// pullPolicy survives; repository is relocated.
	r.Equal(map[string]any{
		"image": map[string]any{"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1", "pullPolicy": "IfNotPresent"},
	}, got)
}

func TestValuesConfigMayNotPartiallyOverrideLocalizedLeaf(t *testing.T) {
	r := require.New(t)
	chart := map[string]any{"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"}}
	localization := map[string]any{"image": map[string]any{"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1"}}
	inline := map[string]any{"image": map[string]any{"tag": "7.0.0"}}

	_, err := Values(chart, localization, inline)
	r.ErrorContains(err, "partially overrides localized image leaf")
	r.ErrorContains(err, "image.tag")
}

func TestValuesConfigMayRedefineWholeImage(t *testing.T) {
	r := require.New(t)
	chart := map[string]any{"image": map[string]any{"repository": "ghcr.io/stefanprodan/podinfo", "tag": "6.9.1"}}
	localization := map[string]any{"image": map[string]any{"repository": "localhost:5000/stefanprodan/podinfo", "tag": "6.9.1"}}
	// A whole-image redefinition carries the repository key, so it is allowed.
	inline := map[string]any{"image": map[string]any{"repository": "myregistry.example/podinfo", "tag": "7.0.0"}}

	got, err := Values(chart, localization, inline)
	r.NoError(err)
	r.Equal(map[string]any{
		"image": map[string]any{"repository": "myregistry.example/podinfo", "tag": "7.0.0"},
	}, got)
}

func TestValuesConfigMaySetSiblingOfLocalizedLeaf(t *testing.T) {
	r := require.New(t)
	// redis.repository/tag are localized; redis.enabled is an unrelated sibling
	// a config layer may freely set (this is how the example turns redis on).
	chart := map[string]any{"redis": map[string]any{"enabled": false, "repository": "redis", "tag": "7.0.7"}}
	localization := map[string]any{"redis": map[string]any{"repository": "localhost:5000/library/redis", "tag": "7.0.7"}}
	local := map[string]any{"redis": map[string]any{"enabled": true}}

	got, err := Values(chart, localization, local)
	r.NoError(err)
	r.Equal(map[string]any{
		"redis": map[string]any{"enabled": true, "repository": "localhost:5000/library/redis", "tag": "7.0.7"},
	}, got)
}

func TestValuesArraysReplaced(t *testing.T) {
	r := require.New(t)
	chart := map[string]any{"args": []any{"a", "b", "c"}}
	inline := map[string]any{"args": []any{"x"}}
	got, err := Values(chart, nil, inline)
	r.NoError(err)
	r.Equal(map[string]any{"args": []any{"x"}}, got)
}

func TestValuesDoesNotMutateInputs(t *testing.T) {
	r := require.New(t)
	chart := map[string]any{"ui": map[string]any{"color": "#000"}}
	inline := map[string]any{"ui": map[string]any{"color": "#fff"}}
	_, err := Values(chart, nil, inline)
	r.NoError(err)
	r.Equal("#000", chart["ui"].(map[string]any)["color"], "chart defaults must not be mutated")
}

func TestSortedLeaves(t *testing.T) {
	r := require.New(t)
	localization := map[string]any{
		"image": map[string]any{"repository": "x", "tag": "y"},
		"redis": map[string]any{"repository": "z", "tag": "w"},
	}
	r.Equal([]string{"image.repository", "image.tag", "redis.repository", "redis.tag"}, SortedLeaves(localization))
}
