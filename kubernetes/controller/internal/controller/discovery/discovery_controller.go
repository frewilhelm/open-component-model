package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/equality"
	k8stypes "k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"ocm.software/open-component-model/bindings/go/dag"
	syncdag "ocm.software/open-component-model/bindings/go/dag/sync"
	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	v2 "ocm.software/open-component-model/bindings/go/descriptor/v2"
	ocirepository "ocm.software/open-component-model/bindings/go/oci/spec/repository"
	"ocm.software/open-component-model/bindings/go/plugin/manager"
	"ocm.software/open-component-model/bindings/go/repository/component/resolvers"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/kubernetes/controller/internal/selector"
	"ocm.software/open-component-model/kubernetes/controller/internal/setup"
	"ocm.software/open-component-model/kubernetes/controller/internal/status"
	"ocm.software/open-component-model/kubernetes/controller/internal/util"
	"ocm.software/open-component-model/kubernetes/controller/pkg/configuration"
)

type Reconciler struct {
	*ocm.BaseReconciler

	// PluginManager manages plugins for repository operations.
	PluginManager *manager.PluginManager
}

var _ ocm.Reconciler = (*Reconciler)(nil)

func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Discovery{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
				if err := r.List(ctx, list,
					client.InNamespace(component.GetNamespace()),
					client.MatchingFields{fieldName: component.GetName()},
				); err != nil {
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
		Complete(r)
}

// +kubebuilder:rbac:groups=delivery.ocm.software,resources=discoveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=discoveries/status,verbs=get;update;patch

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
		if equality.Semantic.DeepEqual(discovery.Status, old.Status) {
			return
		}
		patchErr := r.GetClient().Status().Patch(ctx, discovery, client.MergeFrom(old))
		if apierrors.IsRequestEntityTooLargeError(patchErr) {
			// Drop the payload and retry so the user sees a clear condition
			// instead of the last successful reconcile's stale status.
			discovery.Status.Discovery = nil
			status.MarkNotReady(r.EventRecorder, discovery,
				v1alpha1.PayloadTooLargeReason,
				fmt.Sprintf("discovery payload exceeds status size limit: %s", patchErr.Error()))
			status.UpdateBeforePatch(discovery, r.EventRecorder, 0, nil)
			patchErr = r.GetClient().Status().Patch(ctx, discovery, client.MergeFrom(old))
		}
		err = errors.Join(err, patchErr)
	}(ctx)

	logger.Info("preparing reconciling discovery")
	if discovery.Spec.Suspend {
		return ctrl.Result{}, nil
	}

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

	cfg, err := configuration.LoadConfigurations(ctx, r.Client, discovery.GetNamespace(), configs)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to load configurations: %w", err)
	}

	repoResolver, err := r.createResolver(ctx, repoSpec, cfg)
	if err != nil {
		status.MarkNotReady(r.GetEventRecorder(), discovery, v1alpha1.GetRepositoryFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to create repository resolver: %w", err)
	}

	// uniqueTargetKey is the identity of a target vertex that the DAG traversal
	// should short-circuit at, if any selector uniquely identifies one.
	identityKey := func(name, version string) string {
		return runtime.Identity{
			descruntime.IdentityAttributeName:    name,
			descruntime.IdentityAttributeVersion: version,
		}.String()
	}

	var uniqueTargetKey string
	if name, version, ok := uniqueTargetFromSelector(discovery.Spec.ComponentSelector, descruntime.IdentityAttributeName); ok {
		uniqueTargetKey = identityKey(name, version)
	} else if name, version, ok := uniqueTargetFromSelector(discovery.Spec.ReferenceSelector, IdentityAttributeComponentName); ok {
		uniqueTargetKey = identityKey(name, version)
	}

	resAndDis := &resolverAndDiscoverer{
		repositoryResolver: repoResolver,
		shortCircuitKey:    uniqueTargetKey,
	}

	rootKey := identityKey(
		component.Status.Component.Component,
		component.Status.Component.Version,
	)

	// If uniqueTargetKey happens to equal rootKey (a selector uniquely
	// identifying the root itself), Discover fires errShortCircuit on the very
	// first vertex. That is the correct outcome: a ReferenceSelector targeting
	// the root matches nothing (root has no incoming edge in a DAG), and a
	// ComponentSelector targeting the root matches exactly the one descriptor
	// already in the graph.

	discoverer := syncdag.NewGraphDiscoverer(&syncdag.GraphDiscovererOptions[string, *descruntime.Descriptor]{
		Roots:      []string{rootKey},
		Resolver:   resAndDis,
		Discoverer: resAndDis,
	})

	if err := discoverer.Discover(ctx); err != nil && !errors.Is(err, errShortCircuit) {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("discovering component graph: %w", err)
	}

	// Flatten resolved descriptors out of the DAG vertices for downstream
	// filtering / projection. Vertices we couldn't attach a Descriptor to are
	// skipped silently; the resolver already surfaced any hard error via the
	// Discover() call above.
	var descs []*descruntime.Descriptor
	if err := discoverer.Graph().WithReadLock(func(d *dag.DirectedAcyclicGraph[string]) error {
		for _, vert := range d.Vertices {
			val, ok := vert.Attributes[syncdag.AttributeValue]
			if !ok {
				continue
			}
			desc, ok := val.(*descruntime.Descriptor)
			if !ok {
				continue
			}
			descs = append(descs, desc)
		}
		return nil
	}); err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("walking discovered graph: %w", err)
	}

	// Sort descriptors for deterministic status output. Map iteration above is
	// nondeterministic; without this, status.discovery would flap on every
	// reconcile, causing spurious patches and downstream watcher churn.
	sort.SliceStable(descs, func(i, j int) bool {
		if descs[i].Component.Name != descs[j].Component.Name {
			return descs[i].Component.Name < descs[j].Component.Name
		}
		return descs[i].Component.Version < descs[j].Component.Version
	})

	if !selector.IsEmpty(discovery.Spec.ReferenceSelector) {
		var err error
		descs, err = filterByReferenceSelector(ctx, descs, discovery.Spec.ReferenceSelector)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.SelectorFailedReason, err.Error())
			return ctrl.Result{}, err
		}
		if len(descs) == 0 {
			discovery.Status.Discovery = &apiextensionsv1.JSON{Raw: []byte("[]")}
			status.MarkReadyWithReason(r.EventRecorder, discovery,
				v1alpha1.NoReferencesMatchedReason,
				"reference selector matched no references")
			return ctrl.Result{}, nil
		}
	}

	if !selector.IsEmpty(discovery.Spec.ComponentSelector) {
		var err error
		descs, err = filterByComponentSelector(ctx, descs, discovery.Spec.ComponentSelector)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.SelectorFailedReason, err.Error())
			return ctrl.Result{}, err
		}
		if len(descs) == 0 {
			discovery.Status.Discovery = &apiextensionsv1.JSON{Raw: []byte("[]")}
			status.MarkReadyWithReason(r.EventRecorder, discovery,
				v1alpha1.NoComponentsMatchedReason,
				"component selector matched no components")
			return ctrl.Result{}, nil
		}
	}

	if !selector.IsEmpty(discovery.Spec.ResourceSelector) {
		if err := filterResourcesInPlace(ctx, descs, discovery.Spec.ResourceSelector); err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.SelectorFailedReason, err.Error())
			return ctrl.Result{}, err
		}
	}

	// Convert to v2 descriptors
	descsV2 := make([]*v2.Descriptor, 0, len(descs))
	for _, desc := range descs {
		descV2, err := descruntime.ConvertToV2(runtime.NewScheme(runtime.WithAllowUnknown()), desc)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to convert descriptor: %w", err)
		}
		descsV2 = append(descsV2, descV2)
	}

	var output any
	if discovery.Spec.Extract != nil {
		result, err := applyExtract(ctx, descsV2, discovery.Spec.Extract)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.ExtractFailedReason, err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to apply extract: %w", err)
		}
		output = result
	} else {
		// Always emit an array. Consumers can then iterate uniformly regardless
		// of how many descriptors survived resolution and filtering.
		output = descsV2
	}

	raw, err := json.Marshal(output)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to marshal discovery result: %w", err)
	}

	logger.V(1).Info("updating discovery status")
	discovery.Status.Discovery = &apiextensionsv1.JSON{Raw: raw}
	discovery.Status.EffectiveOCMConfig = configs

	status.MarkReady(r.EventRecorder, discovery, "Discovery for %s finished", discovery.Spec.ComponentRef.Name)

	return ctrl.Result{}, nil
}

// createResolver wires a ComponentVersionRepositoryResolver from a repository
// spec and (optional) configuration. It duplicates a similar helper in
// internal/resolution. Kept local for now because the resolution service is
// slated for removal; once that lands we can decide whether to promote this
// or fold it back into a shared setup helper.
func (r *Reconciler) createResolver(ctx context.Context, spec runtime.Typed, cfg *configuration.Configuration) (resolvers.ComponentVersionRepositoryResolver, error) {
	logger := log.FromContext(ctx)

	opts := resolvers.Options{
		RepoProvider: r.PluginManager.ComponentVersionRepositoryRegistry,
	}

	if cfg != nil {
		credGraph, err := setup.NewCredentialGraph(ctx, cfg.Config, setup.CredentialGraphOptions{
			PluginManager: r.PluginManager,
			Logger:        &logger,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create credential graph: %w", err)
		}
		opts.CredentialGraph = credGraph

		fallbackResolvers, pathMatchers, err := resolvers.ExtractResolvers(cfg.Config, ocirepository.Scheme)
		if err != nil {
			return nil, err
		}
		opts.FallbackResolvers = fallbackResolvers
		opts.PathMatchers = pathMatchers
	}

	return resolvers.New(ctx, opts, spec)
}

// uniqueTargetFromSelector returns the (name, version) target that a selector
// picks by concrete equality on MatchIdentity, if any. Because component
// identity {name, version} is globally unique per OCM spec, having both keys
// set is sufficient to short-circuit DAG traversal at that vertex.
//
// MatchLabels and Expression constraints must be empty. For a
// ReferenceSelector, reference labels are per-edge and the same target can
// be reachable via multiple edges; short-circuiting terminates traversal
// before all edges are enumerated, so a post-filter using MatchLabels /
// Expression may reject the target based on the edges we happened to walk
// and miss matching edges we cancelled. Requiring empty label/expression
// clauses keeps the optimisation to cases where the post-filter is a no-op.
//
// nameKey is "componentName" for a ReferenceSelector or "name" for a
// ComponentSelector.
func uniqueTargetFromSelector(s *v1alpha1.Selector, nameKey string) (name, version string, ok bool) {
	if s == nil {
		return "", "", false
	}
	if len(s.MatchLabels) > 0 || s.Expression != "" {
		return "", "", false
	}
	name = s.MatchIdentity[nameKey]
	version = s.MatchIdentity[descruntime.IdentityAttributeVersion]
	if name == "" || version == "" {
		return "", "", false
	}
	return name, version, true
}

