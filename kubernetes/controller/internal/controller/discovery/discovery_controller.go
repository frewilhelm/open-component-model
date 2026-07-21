package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/time/rate"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	syncdag "ocm.software/open-component-model/bindings/go/dag/sync"
	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/plugin/manager"
	"ocm.software/open-component-model/bindings/go/repository/component/resolvers"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/event"
	"ocm.software/open-component-model/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution/workerpool"
	"ocm.software/open-component-model/kubernetes/controller/internal/status"
	"ocm.software/open-component-model/kubernetes/controller/internal/util"
	"ocm.software/open-component-model/kubernetes/controller/internal/verification"
	"ocm.software/open-component-model/kubernetes/controller/pkg/configuration"
)

type Reconciler struct {
	*ocm.BaseReconciler

	// Resolver provides repository resolution and caching for resource reconciliation.
	// It ensures that repository access is efficient and consistent during reconciliation operations.
	Resolver *resolution.Resolver

	// PluginManager manages plugins for resource operations.
	// It enables dynamic loading and execution of plugins required for resource access.
	PluginManager *manager.PluginManager
}

var _ ocm.Reconciler = (*Reconciler)(nil)

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, concurrency int) error {
	// Build index for discoveries that reference a component to make sure that we get notified when a component changes.
	const fieldName = "spec.componentRef.name"
	if err := mgr.GetFieldIndexer().IndexField(ctx, &v1alpha1.Discovery{}, fieldName, func(obj client.Object) []string {
		discovery, ok := obj.(*v1alpha1.Discovery)
		if !ok {
			return nil
		}

		return []string{discovery.Spec.ComponentRef.Name}
	}); err != nil {
		return err
	}

	// event source from resolver's worker pool to get notified when resolutions complete
	eventSource := workerpool.NewEventSource(r.Resolver.WorkerPool())

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Discovery{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(eventSource).
		// Watch for component-events that are referenced by resources
		Watches(
			&v1alpha1.Component{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				component, ok := obj.(*v1alpha1.Component)
				if !ok {
					return []reconcile.Request{}
				}

				// Get list of discoveries that reference the component
				list := &v1alpha1.DiscoveryList{}
				if err := r.List(ctx, list, client.MatchingFields{fieldName: component.GetName()}); err != nil {
					return []reconcile.Request{}
				}

				// For every discovery that references the component create a reconciliation request for that discovery
				requests := make([]reconcile.Request, 0, len(list.Items))
				for _, discovery := range list.Items {
					requests = append(requests, reconcile.Request{
						NamespacedName: k8stypes.NamespacedName{
							Namespace: discovery.GetNamespace(),
							Name:      discovery.GetName(),
						},
					})
				}

				return requests
			}), builder.WithPredicates(ComponentInfoChangedPredicate{})).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: concurrency,
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter(
				workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](5*time.Millisecond, 5*time.Minute),
				&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(10, 100)},
			),
		}).
		Complete(r)
}

// +kubebuilder:rbac:groups=delivery.ocm.software,resources=discovery,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=discovery/status,verbs=get;update;patch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	logger.Info("starting reconciliation")

	discovery := &v1alpha1.Discovery{}
	if err := r.Get(ctx, req.NamespacedName, discovery); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	old := discovery.DeepCopy()
	defer func(ctx context.Context) {
		status.UpdateBeforePatch(discovery, r.EventRecorder, 0, err)
		if !equality.Semantic.DeepEqual(discovery.Status, old.Status) {
			err = errors.Join(err, r.GetClient().Status().Patch(ctx, discovery, client.MergeFrom(old)))
		}
	}(ctx)

	logger.Info("preparing reconciling discovery")
	if discovery.Spec.Suspend {
		return ctrl.Result{}, nil
	}

	// TODO: Think about adding finalizers, but for what?
	if !discovery.GetDeletionTimestamp().IsZero() {
		logger.Info("discovery is marked for deletion")

		return ctrl.Result{}, nil
	}

	component, err := util.GetReadyObject[v1alpha1.Component, *v1alpha1.Component](ctx, r.Client, client.ObjectKey{
		Namespace: discovery.GetNamespace(),
		Name:      discovery.Spec.ComponentRef.Name,
	})
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.ResourceIsNotAvailable, err.Error())

		var notReadyErr util.NotReadyError
		var deletionErr util.DeletionError
		if errors.As(err, &notReadyErr) || errors.As(err, &deletionErr) {
			logger.Info("component is not available", "error", err)

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get ready component: %w", err)
	}

	if component.Status.Component.RepositorySpec == nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.ResourceIsNotAvailable, "repository spec in component status must not be nil")

		return ctrl.Result{}, fmt.Errorf("repository spec in component status must not be nil for component: %s", component.Name)
	}

	logger.Info("reconciling discovery")
	configs, err := ocm.GetEffectiveConfig(ctx, r.GetClient(), discovery, component)
	if err != nil {
		status.MarkNotReady(r.GetEventRecorder(), discovery, v1alpha1.GetConfigurationFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to configure context: %w", err)
	}

	// Set effective config immediately so the deferred patch persists it
	// even if a subsequent step fails.
	if !equality.Semantic.DeepEqual(discovery.Status.EffectiveOCMConfig, configs) {
		discovery.Status.EffectiveOCMConfig = configs

		return ctrl.Result{}, fmt.Errorf("effective ocm config changed")
	}

	repoSpec := &runtime.Raw{}
	if err := runtime.NewScheme(runtime.WithAllowUnknown()).Decode(
		bytes.NewReader(component.Status.Component.RepositorySpec.Raw), repoSpec); err != nil {
		status.MarkNotReady(r.GetEventRecorder(), discovery, v1alpha1.GetRepositoryFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to decode repository spec: %w", err)
	}

	// Add verifications from the component to the cache-backed repository to make sure they are included in the
	// cache key and used for verification.
	verifications, err := verification.GetVerifications(ctx, r.Client, component)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to get verifications: %w", err)
	}

	cfg, err := configuration.LoadConfigurations(ctx, r.Client, discovery.GetNamespace(), configs)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to load configurations: %w", err)
	}

	cacheBackedRepo, err := r.Resolver.NewCacheBackedRepository(ctx, &resolution.RepositoryOptions{
		RepositorySpec:  repoSpec,
		Configuration:   cfg,
		SigningRegistry: r.PluginManager.SigningRegistry,
		Verifications:   verifications,
		RequesterFunc: func() workerpool.RequesterInfo {
			return workerpool.RequesterInfo{
				NamespacedName: k8stypes.NamespacedName{
					Namespace: discovery.GetNamespace(),
					Name:      discovery.GetName(),
				},
			}
		},
	})
	if err != nil {
		status.MarkNotReady(r.GetEventRecorder(), discovery, v1alpha1.GetRepositoryFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to create cache-backed repository: %w", err)
	}

	referencedDescriptor, err := cacheBackedRepo.GetComponentVersion(ctx,
		component.Status.Component.Component,
		component.Status.Component.Version)
	switch {
	case errors.Is(err, workerpool.ErrResolutionInProgress):
		// Resolution is in progress, the controller will be re-triggered via event source when resolution completes
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.ResolutionInProgress, err.Error())
		logger.Info("component version resolution in progress, waiting for event notification",
			"component", component.Status.Component.Component,
			"version", component.Status.Component.Version)

		return ctrl.Result{}, nil
	case errors.Is(err, workerpool.ErrNotSafelyDigestible):
		// Ignore error, but log event
		event.New(r.EventRecorder, discovery, nil, v1alpha1.EventSeverityError, "%s", err.Error())
	default:
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

			return ctrl.Result{}, fmt.Errorf("failed to get component version: %w", err)
		}
	}

	descs := []*descriptor.Descriptor{referencedDescriptor}
	if len(referencedDescriptor.Component.References) > 0 {
		// TODO: Reference filtering
		resAndDis := resolverAndDiscoverer{
			repositoryResolver: cacheBackedRepo.GetRepositoryResolver(),
		}
		discoverer := syncdag.NewGraphDiscoverer(&syncdag.GraphDiscovererOptions[string, *descriptor.Descriptor]{
			Roots:      []string{referencedDescriptor.Component.Name},
			Resolver:   &resAndDis,
			Discoverer: &resAndDis,
		})
		discoverer.Graph()
	}

	// TODO: Resource filtering
	if discovery.Spec.ResourceFilter != "" {
	}

	raw, err := json.Marshal(descs)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
		return ctrl.Result{}, fmt.Errorf("PLACEHOLDER", err)
	}

	//resourceIdentity := resource.Spec.Resource.ByReference.Resource
	//var matchedResource *descriptor.Resource
	//for i, res := range resourceDescriptor.Component.Resources {
	//	resIdentity := res.ToIdentity()
	//	if resourceIdentity.Match(resIdentity, ocm.IdentityFuncIgnoreVersion()) {
	//		matchedResource = &resourceDescriptor.Component.Resources[i]
	//		break
	//	}
	//}

	// if matchedResource == nil {
	//	err := fmt.Errorf("resource with identity %v not found in component %s:%s",
	//		resourceIdentity, resourceDescriptor.Component.Name, resourceDescriptor.Component.Version)
	//	status.MarkNotReady(r.EventRecorder, resource, v1alpha1.GetOCMResourceFailedReason, err.Error())

	//	return ctrl.Result{}, err
	//}

	if err = setDiscoveryStatus(ctx, configs, discovery, &apiextensionsv1.JSON{Raw: raw}); err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.StatusSetFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to set resource status: %w", err)
	}

	status.MarkReady(r.EventRecorder, discovery, "Discovered %s", discovery.Spec.ComponentRef)

	return ctrl.Result{}, nil
}

// setResourceStatus updates the resource status with all required information.
func setDiscoveryStatus(
	ctx context.Context,
	configs []v1alpha1.OCMConfiguration,
	discovery *v1alpha1.Discovery,
	raw *apiextensionsv1.JSON,
) error {
	log.FromContext(ctx).V(1).Info("updating discovery status")

	discovery.Status.Discovery = raw
	discovery.Status.EffectiveOCMConfig = configs

	return nil
}

type resolverAndDiscoverer struct {
	repositoryResolver resolvers.ComponentVersionRepositoryResolver
}

var (
	_ syncdag.Resolver[string, *descriptor.Descriptor]   = (*resolverAndDiscoverer)(nil)
	_ syncdag.Discoverer[string, *descriptor.Descriptor] = (*resolverAndDiscoverer)(nil)
)

func (r *resolverAndDiscoverer) Resolve(ctx context.Context, key string) (*descriptor.Descriptor, error) {
	id, err := runtime.ParseIdentity(key)
	if err != nil {
		return nil, fmt.Errorf("parsing identity %q failed: %w", key, err)
	}

	component, version := id[descriptor.IdentityAttributeName], id[descriptor.IdentityAttributeVersion]
	repo, err := r.repositoryResolver.GetComponentVersionRepositoryForComponent(ctx, component, version)
	if err != nil {
		return nil, fmt.Errorf("getting component version repository for identity %q failed: %w", id, err)
	}

	desc, err := repo.GetComponentVersion(ctx, component, version)
	if err != nil {
		return nil, fmt.Errorf("getting component version for identity %q failed: %w", id, err)
	}

	return desc, nil
}

func (r *resolverAndDiscoverer) Discover(ctx context.Context, parent *descriptor.Descriptor) ([]string, error) {
	logger := log.FromContext(ctx)

	// unlimited recursion
	children := make([]string, len(parent.Component.References))
	for index, reference := range parent.Component.References {
		children[index] = reference.ToComponentIdentity().String()
	}

	logger.Info("discovering children", "component", parent.Component.ToIdentity().String(), "children", children)

	return children, nil
}
