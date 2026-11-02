package deployment

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
)

func TestResStatusToDescriptorResource(t *testing.T) {
	r := require.New(t)

	accessJSON, err := json.Marshal(map[string]any{
		"type":           "ociArtifact",
		"imageReference": "localhost:5000/charts/podinfo:6.9.1",
	})
	r.NoError(err)

	res := &v1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "chart", Namespace: "default"},
		Status: v1alpha1.ResourceStatus{
			Resource: &v1alpha1.ResourceInfo{
				Name:    "chart",
				Version: "1.0.0",
				Access:  apiextensionsv1.JSON{Raw: accessJSON},
				Digest: &v2.Digest{
					HashAlgorithm:          "SHA-256",
					NormalisationAlgorithm: "ociArtifactDigest/v1",
					Value:                  "565d310746f1fa4be7f93ba7965bb393153a2d57a15cfe5befc909b790a73f8a",
				},
			},
		},
	}

	got := resStatusToDescriptorResource(res)
	r.Equal("chart", got.Name)
	r.Equal("1.0.0", got.Version)
	r.NotNil(got.Access, "access must be carried across")
	r.NotNil(got.Digest, "digest must be converted")
	r.Equal("565d310746f1fa4be7f93ba7965bb393153a2d57a15cfe5befc909b790a73f8a", got.Digest.Value)
}

func TestResStatusToDescriptorResourceNilStatus(t *testing.T) {
	r := require.New(t)
	res := &v1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
	}
	got := resStatusToDescriptorResource(res)
	// nil Status.Resource → empty descriptor resource (no panic).
	r.NotNil(got)
	r.Equal(&descriptor.Resource{}, got)
}
