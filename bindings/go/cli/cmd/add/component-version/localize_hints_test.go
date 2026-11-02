package componentversion

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	localizationv1alpha1 "ocm.software/open-component-model/bindings/go/descriptor/runtime/labels/localization/v1alpha1"
	"ocm.software/open-component-model/bindings/go/runtime"
)

// makeOCIResource builds a descriptor.Resource with an OCI image access spec.
func makeOCIResource(t *testing.T, name, imageRef string) *descriptor.Resource {
	t.Helper()
	acc := &unregisteredAccess{
		Type:           runtime.NewUnversionedType("OCIImage"),
		ImageReference: imageRef,
	}
	return &descriptor.Resource{
		ElementMeta: descriptor.ElementMeta{
			ObjectMeta: descriptor.ObjectMeta{Name: name},
		},
		Type:   "ociImage",
		Access: acc,
	}
}

// makeNonOCIResource builds a descriptor.Resource with a non-OCI access spec.
func makeNonOCIResource(t *testing.T, name string) *descriptor.Resource {
	t.Helper()
	acc := &unregisteredAccess{
		Type:           runtime.NewUnversionedType("helmChart"),
		ImageReference: "",
	}
	return &descriptor.Resource{
		ElementMeta: descriptor.ElementMeta{
			ObjectMeta: descriptor.ObjectMeta{Name: name},
		},
		Type:   "helmChart",
		Access: acc,
	}
}

func localizationLabelFrom(t *testing.T, res *descriptor.Resource) *localizationv1alpha1.Value {
	t.Helper()
	v, ok, err := localizationv1alpha1.FromResourceLabels(res.Labels)
	require.NoError(t, err)
	if !ok {
		return nil
	}
	return v
}

// TestAttachLocalizationLabel_OCIWithDigest verifies that a resource with a full OCI reference
// (registry, repo, tag, digest) gets the label with Digest set and Signing=true.
func TestAttachLocalizationLabel_OCIWithDigest(t *testing.T) {
	r := require.New(t)

	res := makeOCIResource(t, "app", "ghcr.io/stefanprodan/podinfo:6.9.1@sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb")
	r.NoError(attachLocalizationLabel(context.Background(), res, nil))
	r.Len(res.Labels, 1, "exactly one localization label")

	lbl := res.Labels[0]
	r.Equal(localizationv1alpha1.LabelName, lbl.Name)
	r.True(lbl.Signing, "localization label must be signed")

	v := localizationLabelFrom(t, res)
	r.NotNil(v)
	r.Equal("ghcr.io", v.MatchKey.Registry)
	r.Equal("stefanprodan/podinfo", v.MatchKey.Repository)
	r.Equal("6.9.1", v.MatchKey.Tag)
	r.Equal("sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb", v.MatchKey.Digest)
}

// TestAttachLocalizationLabel_OCITagOnly verifies that a tag-only reference leaves Digest empty.
func TestAttachLocalizationLabel_OCITagOnly(t *testing.T) {
	r := require.New(t)

	res := makeOCIResource(t, "app", "ghcr.io/stefanprodan/podinfo:6.9.1")
	r.NoError(attachLocalizationLabel(context.Background(), res, nil))
	r.Len(res.Labels, 1)

	v := localizationLabelFrom(t, res)
	r.NotNil(v)
	r.Equal("6.9.1", v.MatchKey.Tag)
	r.Empty(v.MatchKey.Digest)
}

// TestAttachLocalizationLabel_NonOCI verifies that non-OCI resources are skipped (no label added).
func TestAttachLocalizationLabel_NonOCI(t *testing.T) {
	r := require.New(t)

	res := makeNonOCIResource(t, "chart")
	r.NoError(attachLocalizationLabel(context.Background(), res, nil))
	r.Empty(res.Labels, "non-OCI resource must not receive a localization label")
}

// TestAttachLocalizationLabel_AlreadyLabeled verifies that a resource already carrying the label
// is not overwritten.
func TestAttachLocalizationLabel_AlreadyLabeled(t *testing.T) {
	r := require.New(t)

	original := localizationv1alpha1.Value{
		Type:     "ociImage",
		MatchKey: localizationv1alpha1.MatchKey{Repository: "original/repo"},
	}
	raw, err := json.Marshal(original)
	r.NoError(err)

	res := makeOCIResource(t, "app", "ghcr.io/other/image:1.0.0")
	res.Labels = []descriptor.Label{
		{
			Name:    localizationv1alpha1.LabelName,
			Version: localizationv1alpha1.Version,
			Value:   raw,
		},
	}

	r.NoError(attachLocalizationLabel(context.Background(), res, nil))
	r.Len(res.Labels, 1, "must not add a second localization label")

	// Confirm the original label was not replaced.
	v := localizationLabelFrom(t, res)
	r.NotNil(v)
	r.Equal("original/repo", v.MatchKey.Repository)
}

// TestAttachLocalizationLabel_UnparseableRef verifies that a resource whose imageReference
// cannot be parsed is skipped silently (no label added, no error returned).
func TestAttachLocalizationLabel_UnparseableRef(t *testing.T) {
	r := require.New(t)

	// looseref.ParseReference rejects truly invalid refs (e.g. uppercase in repo name per
	// the OCI distribution spec).  Use a deliberately malformed reference.
	res := makeOCIResource(t, "app", "UPPERCASE-NOT-VALID:INVALID")
	r.NoError(attachLocalizationLabel(context.Background(), res, nil))
	r.Empty(res.Labels, "unparseable ref must be skipped: no label added")
}

// TestAttachLocalizationLabel_ConstructError verifies that a construction error is propagated
// without attaching a label.
func TestAttachLocalizationLabel_ConstructError(t *testing.T) {
	r := require.New(t)

	res := makeOCIResource(t, "app", "ghcr.io/example/app:1.0.0")
	constructErr := &testError{"construct failed"}
	err := attachLocalizationLabel(context.Background(), res, constructErr)
	r.ErrorIs(err, constructErr)
	r.Empty(res.Labels)
}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
