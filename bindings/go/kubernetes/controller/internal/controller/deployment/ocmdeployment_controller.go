package deployment

import (
	"context"
	"errors"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/status"
)

// Reconciler reconciles an OCMDeployment object.
type Reconciler struct {
	*ocm.BaseReconciler
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.OCMDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, 5*time.Minute),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(10, 100)},
			),
		}).
		Complete(r)
}

// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments/finalizers,verbs=update

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	logger.Info("starting reconciliation")

	deployment := &v1alpha1.OCMDeployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if deployment.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	old := deployment.DeepCopy()
	defer func(ctx context.Context) {
		status.UpdateBeforePatch(deployment, r.EventRecorder, deployment.GetRequeueAfter(), err)
		if !equality.Semantic.DeepEqual(deployment.Status, old.Status) {
			err = errors.Join(err, r.GetClient().Status().Patch(ctx, deployment, client.MergeFrom(old)))
		}
	}(ctx)

	// ponytail: Phase 0 is wiring only; reconcile logic lands in Phase 4.
	return ctrl.Result{}, nil
}
