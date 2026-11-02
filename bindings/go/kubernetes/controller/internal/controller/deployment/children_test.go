package deployment

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
)

func newDeploymentReconciler(fc client.Client, scheme *runtime.Scheme) *Reconciler {
	return &Reconciler{
		BaseReconciler: &ocm.BaseReconciler{
			Client: fc,
			Scheme: scheme,
		},
	}
}

func makeImageResourceCR(name, namespace string, owner *v1alpha1.OCMDeployment, scheme *runtime.Scheme) *v1alpha1.Resource {
	res := &v1alpha1.Resource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}

	_ = setOwnerRef(res, owner, scheme)

	return res
}

// setOwnerRef sets a controller owner reference without requiring a full scheme round-trip.
func setOwnerRef(obj, owner metav1.Object, _ *runtime.Scheme) error {
	obj.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion: "delivery.ocm.software/v1alpha1",
			Kind:       "OCMDeployment",
			Name:       owner.GetName(),
			UID:        owner.GetUID(),
			Controller: func() *bool { b := true; return &b }(),
		},
	})

	return nil
}

// TestPruneImageResourceCRs verifies that pruneImageResourceCRs deletes stale
// index-named image Resource CRs while leaving in-range and unrelated CRs intact.
func TestPruneImageResourceCRs(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	scheme := runtime.NewScheme()
	r.NoError(v1alpha1.AddToScheme(scheme))

	dep := &v1alpha1.OCMDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "podinfo",
			Namespace: "default",
			UID:       "uid-1234",
		},
	}

	// Build: 3 image CRs (indices 0,1,2), one unrelated CR, one CR owned by a different OCMDeployment.
	img0 := makeImageResourceCR("podinfo-img-0", "default", dep, scheme)
	img1 := makeImageResourceCR("podinfo-img-1", "default", dep, scheme)
	img2 := makeImageResourceCR("podinfo-img-2", "default", dep, scheme)
	unrelated := &v1alpha1.Resource{ObjectMeta: metav1.ObjectMeta{Name: "some-other-resource", Namespace: "default"}}
	otherDep := &v1alpha1.OCMDeployment{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "uid-9999"}}
	imgOther := makeImageResourceCR("podinfo-img-3", "default", otherDep, scheme)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(img0, img1, img2, unrelated, imgOther).
		Build()

	rec := newDeploymentReconciler(fc, scheme)

	// Prune to desiredCount=1: indices 1 and 2 should be deleted; index 0 kept.
	r.NoError(rec.pruneImageResourceCRs(ctx, dep, 1))

	var remaining v1alpha1.ResourceList
	r.NoError(fc.List(ctx, &remaining, client.InNamespace("default")))

	names := make([]string, 0, len(remaining.Items))
	for _, item := range remaining.Items {
		names = append(names, item.Name)
	}

	r.Contains(names, "podinfo-img-0", "index 0 must survive")
	r.NotContains(names, "podinfo-img-1", "index 1 must be pruned")
	r.NotContains(names, "podinfo-img-2", "index 2 must be pruned")
	r.Contains(names, "some-other-resource", "unrelated CR must survive")
	r.Contains(names, "podinfo-img-3", "CR owned by different OCMDeployment must survive")
}

// TestPruneImageResourceCRs_ZeroDesired verifies all image CRs are pruned when
// the new component version exposes no localizable images (desiredCount=0).
func TestPruneImageResourceCRs_ZeroDesired(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	scheme := runtime.NewScheme()
	r.NoError(v1alpha1.AddToScheme(scheme))

	dep := &v1alpha1.OCMDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default", UID: "uid-1234"},
	}

	img0 := makeImageResourceCR("podinfo-img-0", "default", dep, scheme)
	img1 := makeImageResourceCR("podinfo-img-1", "default", dep, scheme)

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(img0, img1).Build()
	rec := newDeploymentReconciler(fc, scheme)

	r.NoError(rec.pruneImageResourceCRs(ctx, dep, 0))

	var remaining v1alpha1.ResourceList
	r.NoError(fc.List(ctx, &remaining, client.InNamespace("default")))
	r.Empty(remaining.Items, "all image CRs should be pruned when desiredCount=0")
}
