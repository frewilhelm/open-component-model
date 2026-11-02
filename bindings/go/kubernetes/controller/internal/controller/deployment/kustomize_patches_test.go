package deployment

import (
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
)

// makeReconcilerWithRecorder builds a minimal *Reconciler with a fake client and event recorder,
// sufficient for testing methods that only need r.Client, r.Scheme, and r.EventRecorder.
func makeReconcilerWithRecorder(t *testing.T, scheme *runtime.Scheme) (*Reconciler, *record.FakeRecorder) {
	t.Helper()
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(16)
	rec := &Reconciler{
		BaseReconciler: &ocm.BaseReconciler{
			Client:        fc,
			Scheme:        scheme,
			EventRecorder: recorder,
		},
	}
	return rec, recorder
}

// TestCollectKustomizePatchesUnsupportedConfiguration verifies that collectKustomizePatches
// rejects inline spec.config that has no top-level "patches" list (UnsupportedConfiguration),
// and that an invalid-YAML config wraps the parse error.
func TestCollectKustomizePatchesUnsupportedConfiguration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	dep := &v1alpha1.OCMDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "default"},
		Spec: v1alpha1.OCMDeploymentSpec{
			Deployer: v1alpha1.DeployerEngineFluxCD,
		},
	}
	comp := &v1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "default"},
	}

	t.Run("no patches list returns UnsupportedConfiguration", func(t *testing.T) {
		r := require.New(t)
		rec, _ := makeReconcilerWithRecorder(t, scheme)

		// Valid YAML but no "patches" key — must be rejected.
		dep.Spec.Config = &apiextensionsv1.JSON{Raw: []byte(`{"values": {"replicas": 2}}`)}

		patches, err := rec.collectKustomizePatches(t.Context(), dep, comp)
		r.Error(err)
		r.Contains(err.Error(), "patches")
		r.Nil(patches)
	})

	t.Run("invalid YAML wraps the parse error", func(t *testing.T) {
		r := require.New(t)
		rec, _ := makeReconcilerWithRecorder(t, scheme)

		// Malformed YAML that cannot be unmarshalled.
		dep.Spec.Config = &apiextensionsv1.JSON{Raw: []byte(`{invalid yaml`)}

		patches, err := rec.collectKustomizePatches(t.Context(), dep, comp)
		r.Error(err)
		r.Contains(err.Error(), "spec.config is not valid YAML")
		r.Nil(patches)
	})

	t.Run("valid patches list is accepted", func(t *testing.T) {
		r := require.New(t)
		rec, _ := makeReconcilerWithRecorder(t, scheme)

		dep.Spec.Config = &apiextensionsv1.JSON{Raw: []byte(`{"patches": [{"patch": "[{\"op\":\"add\",\"path\":\"/spec/replicas\",\"value\":3}]", "target": {"kind": "Deployment"}}]}`)}

		patches, err := rec.collectKustomizePatches(t.Context(), dep, comp)
		r.NoError(err)
		r.Len(patches, 1)
	})
}
