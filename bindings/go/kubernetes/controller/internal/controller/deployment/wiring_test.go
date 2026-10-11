package deployment

import (
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/runtime"
)

func deploymentFor(repoSpec string, semver, semverFilter string) *v1alpha1.OCMDeployment {
	return &v1alpha1.OCMDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
		Spec: v1alpha1.OCMDeploymentSpec{
			RepositorySpec: &apiextensionsv1.JSON{Raw: []byte(repoSpec)},
			Component:      "ocm.software/examples/deployment",
			Semver:         semver,
			SemverFilter:   semverFilter,
			DeployResource: v1alpha1.ResourceID{ByReference: v1alpha1.ResourceReference{Resource: runtime.Identity{"name": "chart"}}},
			Interval:       metav1.Duration{Duration: 0},
		},
	}
}

func TestDesiredRepositorySpecPassesConfigThrough(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"OCIRegistry https", `{"type":"OCIRegistry","baseUrl":"https://ghcr.io"}`},
		{"OCIRegistry insecure http", `{"type":"OCIRegistry","baseUrl":"http://localhost:5000"}`},
		{"non-OCI repository type", `{"type":"CommonTransportFormat/v1","filePath":"/ctf"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			d := deploymentFor(tc.raw, "1.0.0", "")
			spec := desiredRepositorySpec(d)
			// The repositorySpec is opaque: it must reach the child byte-for-byte.
			r.Equal(tc.raw, string(spec.RepositorySpec.Raw))
			r.Equal(d.Spec.Interval, spec.Interval)
		})
	}
}

func TestDesiredComponentSpecCarriesSemver(t *testing.T) {
	tests := []struct {
		name         string
		semver       string
		semverFilter string
	}{
		{"exact version", "1.0.0", ""},
		{"semver range", ">=1.0.0 <2.0.0", ""},
		{"range with filter", ">=1.0.0", "^1\\."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			d := deploymentFor(`{"type":"OCIRegistry","baseUrl":"https://ghcr.io"}`, tc.semver, tc.semverFilter)
			spec := desiredComponentSpec(d)
			r.Equal("app", spec.RepositoryRef.Name, "child Component references the child Repository by the shared name")
			r.Equal(d.Spec.Component, spec.Component)
			r.Equal(tc.semver, spec.Semver)
			r.Equal(tc.semverFilter, spec.SemverFilter)
		})
	}
}

func TestDesiredResourceSpecReferencesComponentAndDeployResource(t *testing.T) {
	r := require.New(t)
	d := deploymentFor(`{"type":"OCIRegistry","baseUrl":"https://ghcr.io"}`, "1.0.0", "")
	spec := desiredResourceSpec(d)
	r.Equal("app", spec.ComponentRef.Name)
	r.Equal(d.Spec.DeployResource, spec.Resource)
}
