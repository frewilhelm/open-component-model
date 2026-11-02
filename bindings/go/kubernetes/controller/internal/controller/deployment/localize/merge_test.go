package localize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// localizedValues simulates a values tree after localization rewrote image.repository/tag/digest
// into the relocated coordinates.
func localizedValues() map[string]any {
	return map[string]any{
		"image": map[string]any{
			"repository": "localhost:5000/stefanprodan/podinfo",
			"tag":        "6.9.1",
			"digest":     "sha256:abc",
		},
		"replicaCount": 1,
		"service":      map[string]any{"port": 9898},
	}
}

func TestMergeHelmConfig_AtomicImageBlock(t *testing.T) {
	imagePaths := [][]string{{"image"}}

	t.Run("whole-block config override wins over the localized block", func(t *testing.T) {
		r := require.New(t)
		// Config provides a full image block (identity key present) -> replaces the whole block.
		inline := []byte(`{"image":{"repository":"example.com/custom/app","tag":"2.0.0"}}`)

		out, err := MergeHelmConfig(localizedValues(), nil, inline, imagePaths)
		r.NoError(err)

		img := out["image"].(map[string]any)
		r.Equal("example.com/custom/app", img["repository"])
		r.Equal("2.0.0", img["tag"])
		// The localized digest must be gone: the whole block was replaced, not merged.
		_, hasDigest := img["digest"]
		r.False(hasDigest, "whole-block override must drop the localized digest, not keep it")
	})

	t.Run("partial config override of the image block is skipped", func(t *testing.T) {
		r := require.New(t)
		// Config sets only tag (no identity key) -> partial -> must NOT be applied.
		inline := []byte(`{"image":{"tag":"9.9.9"}}`)

		out, err := MergeHelmConfig(localizedValues(), nil, inline, imagePaths)
		r.NoError(err)

		img := out["image"].(map[string]any)
		// Localized block is retained unchanged; the partial tag override did not split the ref.
		r.Equal("localhost:5000/stefanprodan/podinfo", img["repository"])
		r.Equal("6.9.1", img["tag"])
		r.Equal("sha256:abc", img["digest"])
	})

	t.Run("non-image map keys continue to deep-merge", func(t *testing.T) {
		r := require.New(t)
		inline := []byte(`{"service":{"type":"LoadBalancer"}}`)

		out, err := MergeHelmConfig(localizedValues(), nil, inline, imagePaths)
		r.NoError(err)

		svc := out["service"].(map[string]any)
		r.Equal("LoadBalancer", svc["type"])
		// Existing key survives the deep-merge.
		r.EqualValues(9898, svc["port"])
	})

	t.Run("config layers then inline apply in precedence order", func(t *testing.T) {
		r := require.New(t)
		layer := []byte(`replicaCount: 3`)
		inline := []byte(`{"replicaCount":5}`)

		out, err := MergeHelmConfig(localizedValues(), [][]byte{layer}, inline, imagePaths)
		r.NoError(err)
		r.EqualValues(5, out["replicaCount"], "inline config must win over config-resource layer")
	})
}

func TestDeepMerge_ArraysReplace(t *testing.T) {
	r := require.New(t)
	dst := map[string]any{"args": []any{"a", "b"}}
	src := map[string]any{"args": []any{"c"}}

	out := DeepMerge(dst, src)
	r.Equal([]any{"c"}, out["args"], "arrays replace, not append")
}
