package deployment

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
)

// resourceWithReady builds a Resource whose Ready condition is set to the given status.
func resourceWithReady(ready bool) *v1alpha1.Resource {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}

	return &v1alpha1.Resource{
		Status: v1alpha1.ResourceStatus{
			Conditions: []metav1.Condition{{Type: v1alpha1.ReadyCondition, Status: status}},
		},
	}
}

// TestResourceReadyChanged verifies the owned-Resource watch predicate: the parent is enqueued on
// create, delete, and a Ready flip in either direction, but NOT on status writes that leave Ready
// unchanged (the churn that makes localizing many images O(N²)).
func TestResourceReadyChanged(t *testing.T) {
	r := require.New(t)
	p := resourceReadyChanged()

	t.Run("create always enqueues", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Create(event.CreateEvent{Object: resourceWithReady(false)}))
	})

	t.Run("delete always enqueues", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Delete(event.DeleteEvent{Object: resourceWithReady(true)}))
	})

	t.Run("generic never enqueues", func(t *testing.T) {
		r := require.New(t)
		r.False(p.Generic(event.GenericEvent{Object: resourceWithReady(true)}))
	})

	t.Run("not-ready to ready enqueues", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Update(event.UpdateEvent{
			ObjectOld: resourceWithReady(false),
			ObjectNew: resourceWithReady(true),
		}))
	})

	t.Run("ready to not-ready enqueues (drift must not be missed)", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Update(event.UpdateEvent{
			ObjectOld: resourceWithReady(true),
			ObjectNew: resourceWithReady(false),
		}))
	})

	t.Run("status write with Ready unchanged (still not-ready) is dropped", func(t *testing.T) {
		r := require.New(t)
		// Simulate an intermediate status write (e.g. ResolutionInProgress/digest update) that does
		// not change Ready: a resource with no Ready condition updated to another non-ready state.
		old := &v1alpha1.Resource{Status: v1alpha1.ResourceStatus{Conditions: []metav1.Condition{
			{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: "ResolutionInProgress"},
		}}}
		updated := &v1alpha1.Resource{Status: v1alpha1.ResourceStatus{Conditions: []metav1.Condition{
			{Type: v1alpha1.ReadyCondition, Status: metav1.ConditionFalse, Reason: "Downloading"},
		}}}
		r.False(p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}))
	})

	t.Run("ready unchanged (both ready) is dropped", func(t *testing.T) {
		r := require.New(t)
		r.False(p.Update(event.UpdateEvent{
			ObjectOld: resourceWithReady(true),
			ObjectNew: resourceWithReady(true),
		}))
	})

	t.Run("non-Resource object is treated as not-ready", func(t *testing.T) {
		r := require.New(t)
		// Defensive: a wrong type must not panic; ready()==false for both sides ⇒ dropped on update.
		var notResource client.Object = &v1alpha1.Component{}
		r.False(p.Update(event.UpdateEvent{ObjectOld: notResource, ObjectNew: notResource}))
	})

	_ = r // r declared for any top-level assertions; subtests use their own
}

// TestEngineObjectCreateIgnored verifies the engine-object watch predicate: Create events are dropped
// (they are self-triggered by the controller's own ApplySet apply), while Update, Delete, and Generic
// still enqueue the parent OCMDeployment so drift and readiness transitions are never missed.
func TestEngineObjectCreateIgnored(t *testing.T) {
	p := engineObjectCreateIgnored()

	t.Run("create is dropped (self-triggered by own apply)", func(t *testing.T) {
		r := require.New(t)
		r.False(p.Create(event.CreateEvent{Object: &unstructured.Unstructured{}}))
	})

	t.Run("update enqueues (drift and readiness flips must wake the controller)", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Update(event.UpdateEvent{
			ObjectOld: &unstructured.Unstructured{},
			ObjectNew: &unstructured.Unstructured{},
		}))
	})

	t.Run("delete enqueues (a deleted engine object must trigger re-apply)", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Delete(event.DeleteEvent{Object: &unstructured.Unstructured{}}))
	})

	t.Run("generic enqueues (preserves default controller-runtime behavior)", func(t *testing.T) {
		r := require.New(t)
		r.True(p.Generic(event.GenericEvent{Object: &unstructured.Unstructured{}}))
	})
}
