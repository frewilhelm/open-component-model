package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	localization "ocm.software/open-component-model/bindings/go/descriptor/runtime/labels/localization/v1alpha1"
)

// TestValueRoundTrip verifies that a Value marshals to JSON and round-trips via GetValue.
func TestValueRoundTrip(t *testing.T) {
	r := require.New(t)

	original := localization.Value{
		Type: "ociImage",
		MatchKey: localization.MatchKey{
			Registry:   "ghcr.io",
			Repository: "stefanprodan/podinfo",
			Tag:        "6.9.1",
			Digest:     "sha256:262578cde928d5c9eba3bce079976444f624c13ed0afb741d90d5423877496cb",
		},
	}

	raw, err := json.Marshal(original)
	r.NoError(err)

	lbl := descriptor.Label{
		Name:    localization.LabelName,
		Version: localization.Version,
		Value:   raw,
		Signing: true,
	}

	var decoded localization.Value
	r.NoError(lbl.GetValue(&decoded))
	r.Equal(original, decoded)
}

// TestFromResourceLabels_VersionSkip verifies that labels whose version differs from v1 are ignored.
func TestFromResourceLabels_VersionSkip(t *testing.T) {
	r := require.New(t)

	labels := []descriptor.Label{
		{
			Name:    localization.LabelName,
			Version: "v99", // wrong version
			Value:   json.RawMessage(`{"type":"ociImage","matchKey":{"repository":"example/app"}}`),
		},
	}

	v, ok, err := localization.FromResourceLabels(labels)
	r.NoError(err)
	r.False(ok, "label with a different version must be skipped")
	r.Nil(v)
}

// TestFromResourceLabels_Match verifies that a correctly versioned label is decoded.
func TestFromResourceLabels_Match(t *testing.T) {
	r := require.New(t)

	lv := localization.Value{
		Type: "ociImage",
		MatchKey: localization.MatchKey{
			Registry:   "ghcr.io",
			Repository: "stefanprodan/podinfo",
			Tag:        "6.9.1",
		},
	}
	raw, err := json.Marshal(lv)
	r.NoError(err)

	labels := []descriptor.Label{
		{
			Name:    "unrelated-label",
			Version: "v1",
			Value:   json.RawMessage(`"ignored"`),
		},
		{
			Name:    localization.LabelName,
			Version: localization.Version,
			Value:   raw,
			Signing: true,
		},
	}

	v, ok, err := localization.FromResourceLabels(labels)
	r.NoError(err)
	r.True(ok)
	r.Equal(lv, *v)
}

// TestFromResourceLabels_Absent verifies that (nil, false, nil) is returned when no label is present.
func TestFromResourceLabels_Absent(t *testing.T) {
	r := require.New(t)

	v, ok, err := localization.FromResourceLabels(nil)
	r.NoError(err)
	r.False(ok)
	r.Nil(v)
}
