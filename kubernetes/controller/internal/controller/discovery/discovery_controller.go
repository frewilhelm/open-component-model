package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	"ocm.software/open-component-model/kubernetes/controller/internal/event"
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

	// event source from resolver's worker pool to get notified when resolutions complete

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

	// Emit warning if recursive=0 but referenceSelector is set (contradictory)
	if discovery.Spec.Recursive != nil && *discovery.Spec.Recursive == 0 && discovery.Spec.ReferenceSelector != nil {
		event.New(r.EventRecorder, discovery, nil, v1alpha1.EventSeverityInfo,
			"recursive is 0 but referenceSelector is set; resolving references anyway")
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

	referencedDescriptor, err := getComponentVersion(ctx, repoResolver,
		component.Status.Component.Component,
		component.Status.Component.Version)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, fmt.Errorf("failed to get component version: %w", err)
	}

	descs := []*descruntime.Descriptor{referencedDescriptor}
	skipReferences := discovery.Spec.Recursive != nil && *discovery.Spec.Recursive == 0 && discovery.Spec.ReferenceSelector == nil
	if !skipReferences && len(referencedDescriptor.Component.References) > 0 {
		resAndDis := resolverAndDiscoverer{
			repositoryResolver: repoResolver,
			recursive:          discovery.Spec.Recursive,
		}

		var componentIDs []string
		for _, reference := range referencedDescriptor.Component.References {
			componentIDs = append(componentIDs, reference.ToComponentIdentity().String())
		}

		discoverer := syncdag.NewGraphDiscoverer(&syncdag.GraphDiscovererOptions[string, *descruntime.Descriptor]{
			Roots:      componentIDs,
			Resolver:   &resAndDis,
			Discoverer: &resAndDis,
		})

		if err := discoverer.Discover(ctx); err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to discover component version: %w", err)
		}

		x := discoverer.Graph()

		err = x.WithReadLock(func(d *dag.DirectedAcyclicGraph[string]) error {
			for _, vert := range d.Vertices {
				val, ok := vert.Attributes["dag/value"]
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
		})
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to discover component version: %w", err)
		}
	}

	// Apply ReferenceSelector: post-filter discovered descriptors.
	// When referenceSelector is set, exclude the root — user only wants matching references.
	if discovery.Spec.ReferenceSelector != nil && len(descs) > 1 {
		var filtered []*descruntime.Descriptor
		for _, desc := range descs[1:] {
			element := componentToMatchable(desc)
			matched, err := selector.Matches(discovery.Spec.ReferenceSelector, element)
			if err != nil {
				status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
				return ctrl.Result{}, fmt.Errorf("failed to evaluate reference selector: %w", err)
			}
			if matched {
				filtered = append(filtered, desc)
			}
		}
		descs = filtered
	}

	// Apply ResourceSelector: filter resources on each descriptor
	if discovery.Spec.ResourceSelector != nil {
		for _, desc := range descs {
			filtered := make([]descruntime.Resource, 0, len(desc.Component.Resources))
			for _, res := range desc.Component.Resources {
				element := resourceToMatchable(&res)
				matched, err := selector.Matches(discovery.Spec.ResourceSelector, element)
				if err != nil {
					status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
					return ctrl.Result{}, fmt.Errorf("failed to evaluate resource selector: %w", err)
				}
				if matched {
					filtered = append(filtered, res)
				}
			}
			desc.Component.Resources = filtered
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
	if len(discovery.Spec.DiscoveryFields) > 0 {
		// COMPACT mode: flat list of entries with only the defined fields
		compact, err := buildCompactDiscovery(descsV2, discovery.Spec.DiscoveryFields)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to build compact discovery: %w", err)
		}
		output = compact
	} else if discovery.Spec.ReferenceSelector == nil && discovery.Spec.ResourceSelector == nil && len(descsV2) == 1 {
		// No selectors, single descriptor: return as object
		output = descsV2[0]
	} else {
		// Full descriptors as array (with selectors applied)
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

	status.MarkReady(r.EventRecorder, discovery, "Discovered %s", discovery.Spec.ComponentRef)

	return ctrl.Result{}, nil
}

// TODO: This might be something to do while applying the filters, because we loop over the descriptors again
// buildCompactDiscovery creates a flat list of entries, one per resource from each discovered component.
// Each entry contains only the fields specified in discoveryFields — nothing is added automatically.
// component.* paths extract from the component, resource.* paths extract from each resource.
func buildCompactDiscovery(descs []*v2.Descriptor, fields map[string]string) ([]map[string]any, error) {
	if len(descs) == 0 {
		return nil, nil
	}

	// Split fields by prefix
	compFields := make(map[string]string)
	resFields := make(map[string]string)
	for fieldName, path := range fields {
		switch {
		case strings.HasPrefix(path, "component."):
			compFields[fieldName] = strings.TrimPrefix(path, "component.")
		case strings.HasPrefix(path, "resource."):
			resFields[fieldName] = strings.TrimPrefix(path, "resource.")
		}
	}

	var result []map[string]any

	for _, desc := range descs {
		// Extract component-level fields once per component
		var compExtracted map[string]any
		if len(compFields) > 0 {
			compJSON, err := json.Marshal(desc.Component)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal component %s: %w", desc.Component.Name, err)
			}
			var compMap map[string]any
			if err := json.Unmarshal(compJSON, &compMap); err != nil {
				return nil, fmt.Errorf("failed to unmarshal component %s: %w", desc.Component.Name, err)
			}
			compExtracted = make(map[string]any, len(compFields))
			for fieldName, path := range compFields {
				val := extractPath(compMap, path)
				if val != nil {
					compExtracted[fieldName] = val
				}
			}
		}

		// One entry per resource
		for _, res := range desc.Component.Resources {
			entry := make(map[string]any, len(compFields)+len(resFields))

			// Add component-level fields
			for k, v := range compExtracted {
				entry[k] = v
			}

			// Add resource-level fields
			if len(resFields) > 0 {
				resJSON, err := json.Marshal(res)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal resource %s: %w", res.Name, err)
				}
				var resMap map[string]any
				if err := json.Unmarshal(resJSON, &resMap); err != nil {
					return nil, fmt.Errorf("failed to unmarshal resource %s: %w", res.Name, err)
				}
				for fieldName, path := range resFields {
					val := extractPath(resMap, path)
					if val != nil {
						entry[fieldName] = val
					}
				}
			}

			result = append(result, entry)
		}
	}

	return result, nil
}

// extractPath extracts a value from a nested map using dot-separated path notation.
// For example, "access.imageReference" extracts map["access"]["imageReference"].
// TODO: Consider upgrading to CEL expressions (like Resource CR's additionalStatusFields)
// if users need transformations or conditional logic.
func extractPath(data map[string]any, path string) any {
	parts := splitPath(path)
	var current any = data

	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = m[part]
		if !ok {
			return nil
		}
	}

	return current
}

// splitPath splits a dot-separated path into parts.
func splitPath(path string) []string {
	var parts []string
	for _, p := range strings.Split(path, ".") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

type resolverAndDiscoverer struct {
	repositoryResolver resolvers.ComponentVersionRepositoryResolver
	recursive          *int32
}

var (
	_ syncdag.Resolver[string, *descruntime.Descriptor]   = (*resolverAndDiscoverer)(nil)
	_ syncdag.Discoverer[string, *descruntime.Descriptor] = (*resolverAndDiscoverer)(nil)
)

func (r *resolverAndDiscoverer) Resolve(ctx context.Context, key string) (*descruntime.Descriptor, error) {
	id, err := runtime.ParseIdentity(key)
	if err != nil {
		return nil, fmt.Errorf("parsing identity %q failed: %w", key, err)
	}

	component, version := id[descruntime.IdentityAttributeName], id[descruntime.IdentityAttributeVersion]
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

func (r *resolverAndDiscoverer) Discover(ctx context.Context, parent *descruntime.Descriptor) ([]string, error) {
	logger := log.FromContext(ctx)

	switch {
	case r.recursive == nil:
		// Unlimited recursion
		children := make([]string, len(parent.Component.References))
		for i, ref := range parent.Component.References {
			children[i] = ref.ToComponentIdentity().String()
		}
		logger.Info("discovering children", "component", parent.Component.ToIdentity().String(), "children", children)
		return children, nil
	case *r.recursive == 0:
		logger.Info("not discovering children, recursive is 0", "component", parent.Component.ToIdentity().String())
		return nil, nil
	default:
		// >0: not implemented yet
		return nil, fmt.Errorf("recursive depth %d is not supported yet, use 0 or leave unset for unlimited", *r.recursive)
	}
}

// createResolver creates a ComponentVersionRepositoryResolver from a repository spec and configuration.
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

// getComponentVersion retrieves a component version from the resolver.
func getComponentVersion(ctx context.Context, resolver resolvers.ComponentVersionRepositoryResolver, component, version string) (*descruntime.Descriptor, error) {
	repo, err := resolver.GetComponentVersionRepositoryForComponent(ctx, component, version)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository for component %s:%s: %w", component, version, err)
	}

	desc, err := repo.GetComponentVersion(ctx, component, version)
	if err != nil {
		return nil, fmt.Errorf("failed to get component version %s:%s: %w", component, version, err)
	}

	return desc, nil
}

// componentToMatchable converts a discovered component descriptor into a Matchable for selector evaluation.
// Identity contains the component name and version.
func componentToMatchable(desc *descruntime.Descriptor) selector.Matchable {
	identity := desc.Component.ToIdentity()

	labels := make(map[string]string, len(desc.Component.Labels))
	for _, l := range desc.Component.Labels {
		var s string
		if err := json.Unmarshal(l.Value, &s); err == nil {
			labels[l.Name] = s
		}
	}

	return selector.Matchable{
		Identity: identity,
		Labels:   labels,
	}
}

// resourceToMatchable converts a resource into a Matchable for selector evaluation.
func resourceToMatchable(res *descruntime.Resource) selector.Matchable {
	identity := res.ToIdentity()

	labels := make(map[string]string, len(res.Labels))
	for _, l := range res.Labels {
		var s string
		if err := json.Unmarshal(l.Value, &s); err == nil {
			labels[l.Name] = s
		}
	}

	return selector.Matchable{
		Identity: identity,
		Labels:   labels,
	}
}
