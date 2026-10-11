package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/status"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/util"
)

// Reconciler reconciles an OCMDeployment object. It creates and owns the child
// Repository/Component/Resource objects the user would otherwise author by hand,
// watches them to Ready, and rolls their readiness into the OCMDeployment
// status so a single resource tracks the whole chain.
type Reconciler struct {
	*ocm.BaseReconciler
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.OCMDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&v1alpha1.Repository{}).
		Owns(&v1alpha1.Component{}).
		Owns(&v1alpha1.Resource{}).
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
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=repositories;components;resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=repositories/finalizers;components/finalizers;resources/finalizers,verbs=update

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

	if err := r.ensureChildren(ctx, deployment); err != nil {
		status.MarkNotReady(r.EventRecorder, deployment, v1alpha1.CreateOrUpdateFailedReason, err.Error())
		return ctrl.Result{}, err
	}

	component, err := r.childrenReady(ctx, deployment)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, deployment, v1alpha1.GetResourceFailedReason, err.Error())

		var notReady util.NotReadyError
		var deleting util.DeletionError
		if errors.As(err, &notReady) || errors.As(err, &deleting) {
			logger.Info("children not ready yet", "error", err)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, err
	}

	deployment.Status.DeployedComponentVersion = component.Status.Component.Version
	deployment.Status.Deployed = []v1alpha1.DeployedObjectReference{
		childRef(deployment, v1alpha1.KindRepository),
		childRef(deployment, v1alpha1.KindComponent),
		childRef(deployment, v1alpha1.KindResource),
	}
	status.MarkReady(r.EventRecorder, deployment, "children ready")

	return ctrl.Result{RequeueAfter: deployment.GetRequeueAfter()}, nil
}

// ensureChildren creates or updates the child Repository, Component and Resource
// in the OCMDeployment's own namespace, owner-ref'd for cascade GC and so the
// Owns watches fire. The children share the OCMDeployment's name (distinct kinds
// never collide within a namespace).
func (r *Reconciler) ensureChildren(ctx context.Context, d *v1alpha1.OCMDeployment) error {
	repo := &v1alpha1.Repository{ObjectMeta: childMeta(d)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.GetClient(), repo, func() error {
		repo.Spec = desiredRepositorySpec(d)
		return controllerutil.SetControllerReference(d, repo, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to upsert child Repository: %w", err)
	}

	component := &v1alpha1.Component{ObjectMeta: childMeta(d)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.GetClient(), component, func() error {
		component.Spec = desiredComponentSpec(d)
		return controllerutil.SetControllerReference(d, component, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to upsert child Component: %w", err)
	}

	resource := &v1alpha1.Resource{ObjectMeta: childMeta(d)}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.GetClient(), resource, func() error {
		resource.Spec = desiredResourceSpec(d)
		return controllerutil.SetControllerReference(d, resource, r.Scheme)
	}); err != nil {
		return fmt.Errorf("failed to upsert child Resource: %w", err)
	}

	return nil
}

// desiredRepositorySpec maps the OCMDeployment onto its child Repository. The
// repositorySpec is opaque OCM repository config, passed through unchanged, so
// any repository type is supported and an insecure registry is just an http
// baseUrl inside it.
func desiredRepositorySpec(d *v1alpha1.OCMDeployment) v1alpha1.RepositorySpec {
	return v1alpha1.RepositorySpec{
		RepositorySpec: d.Spec.RepositorySpec,
		OCMConfig:      d.Spec.OCMConfig,
		Interval:       d.Spec.Interval,
	}
}

// desiredComponentSpec maps the OCMDeployment onto its child Component. Semver
// and SemverFilter pass through unchanged, so exact versions and ranges behave
// exactly as they do on a hand-authored Component.
func desiredComponentSpec(d *v1alpha1.OCMDeployment) v1alpha1.ComponentSpec {
	return v1alpha1.ComponentSpec{
		RepositoryRef: corev1.LocalObjectReference{Name: d.GetName()},
		Component:     d.Spec.Component,
		Semver:        d.Spec.Semver,
		SemverFilter:  d.Spec.SemverFilter,
		OCMConfig:     d.Spec.OCMConfig,
		Interval:      d.Spec.Interval,
	}
}

// desiredResourceSpec maps the OCMDeployment's deploy resource onto its child
// Resource.
func desiredResourceSpec(d *v1alpha1.OCMDeployment) v1alpha1.ResourceSpec {
	return v1alpha1.ResourceSpec{
		ComponentRef: corev1.LocalObjectReference{Name: d.GetName()},
		Resource:     d.Spec.DeployResource,
		OCMConfig:    d.Spec.OCMConfig,
	}
}

// childrenReady returns the child Component once Repository, Component and
// Resource all report Ready; otherwise a NotReadyError/DeletionError.
func (r *Reconciler) childrenReady(ctx context.Context, d *v1alpha1.OCMDeployment) (*v1alpha1.Component, error) {
	key := client.ObjectKey{Namespace: d.GetNamespace(), Name: d.GetName()}

	if _, err := util.GetReadyObject[v1alpha1.Repository, *v1alpha1.Repository](ctx, r.GetClient(), key); err != nil {
		return nil, err
	}
	component, err := util.GetReadyObject[v1alpha1.Component, *v1alpha1.Component](ctx, r.GetClient(), key)
	if err != nil {
		return nil, err
	}
	if _, err := util.GetReadyObject[v1alpha1.Resource, *v1alpha1.Resource](ctx, r.GetClient(), key); err != nil {
		return nil, err
	}

	return component, nil
}

func childMeta(d *v1alpha1.OCMDeployment) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: d.GetName(), Namespace: d.GetNamespace()}
}

func childRef(d *v1alpha1.OCMDeployment, kind string) v1alpha1.DeployedObjectReference {
	return v1alpha1.DeployedObjectReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       kind,
		Name:       d.GetName(),
		Namespace:  d.GetNamespace(),
	}
}
