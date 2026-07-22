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
	"ocm.software/open-component-model/bindings/go/plugin/manager"
	"ocm.software/open-component-model/bindings/go/repository/component/resolvers"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/event"
	"ocm.software/open-component-model/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution/workerpool"
	"ocm.software/open-component-model/kubernetes/controller/internal/selector"
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

	descs := []*descruntime.Descriptor{referencedDescriptor}
	skipReferences := discovery.Spec.Recursive != nil && *discovery.Spec.Recursive == 0 && discovery.Spec.ReferenceSelector == nil
	if !skipReferences && len(referencedDescriptor.Component.References) > 0 {
		resAndDis := resolverAndDiscoverer{
			repositoryResolver: cacheBackedRepo.GetRepositoryResolver(),
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

	// Emit warning if recursive=0 but referenceSelector is set (contradictory)
	if discovery.Spec.Recursive != nil && *discovery.Spec.Recursive == 0 && discovery.Spec.ReferenceSelector != nil {
		event.New(r.EventRecorder, discovery, nil, v1alpha1.EventSeverityInfo,
			"recursive is 0 but referenceSelector is set; resolving references anyway")
	}

	// Apply ReferenceSelector: post-filter discovered descriptors (excluding root).
	if discovery.Spec.ReferenceSelector != nil && len(descs) > 1 {
		filtered := []*descruntime.Descriptor{descs[0]}
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

	// Build output based on selector/discoveryFields combination
	hasRefSelector := discovery.Spec.ReferenceSelector != nil
	hasResSelector := discovery.Spec.ResourceSelector != nil
	hasFields := len(discovery.Spec.DiscoveryFields) > 0

	var output any
	switch {
	case !hasRefSelector && !hasResSelector && !hasFields:
		// RAW mode: full descriptors
		if len(descsV2) == 1 {
			output = descsV2[0]
		} else {
			output = descsV2
		}
	case hasFields:
		// COMPACT mode: root identity + resources with extracted fields
		compact, err := buildCompactDiscovery(descsV2, discovery.Spec.DiscoveryFields)
		if err != nil {
			status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
			return ctrl.Result{}, fmt.Errorf("failed to build compact discovery: %w", err)
		}
		output = compact
	case hasRefSelector && !hasResSelector:
		// STRUCTURED mode: root identity + filtered full descriptors
		root := descsV2[0]
		output = map[string]any{
			"component":  root.Component.Name,
			"version":    root.Component.Version,
			"components": descsV2[1:],
		}
	default:
		// STRUCTURED mode: root identity + filtered resources (with component context)
		root := descsV2[0]
		var resources []map[string]any
		for _, desc := range descsV2 {
			for _, res := range desc.Component.Resources {
				resJSON, _ := json.Marshal(res)
				var resMap map[string]any
				_ = json.Unmarshal(resJSON, &resMap)
				resMap["component"] = desc.Component.Name
				resMap["version"] = desc.Component.Version
				resources = append(resources, resMap)
			}
		}
		output = map[string]any{
			"component": root.Component.Name,
			"version":   root.Component.Version,
			"resources": resources,
		}
	}

	raw, err := json.Marshal(output)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, discovery, v1alpha1.GetComponentVersionFailedReason, err.Error())
		return ctrl.Result{}, fmt.Errorf("failed to marshal discovery result: %w", err)
	}

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

// compactComponent is the hierarchical per-component entry in compact discovery output.
type compactComponent struct {
	Component  string             `json:"component"`
	Version    string             `json:"version"`
	Resources  []map[string]any   `json:"resources,omitempty"`
	References []compactComponent `json:"references,omitempty"`
}

// buildCompactDiscovery creates a hierarchical compact discovery result.
// The root component is top-level, with nested references indented below it.
// Each resource entry contains its name plus any extracted fields directly (no wrapper).
func buildCompactDiscovery(descs []*v2.Descriptor, fields map[string]string) (*compactComponent, error) {
	if len(descs) == 0 {
		return nil, nil
	}

	// Build a lookup map: componentName+version → descriptor
	descMap := make(map[string]*v2.Descriptor, len(descs))
	for _, desc := range descs {
		key := desc.Component.Name + ":" + desc.Component.Version
		descMap[key] = desc
	}

	// Build the tree starting from the root (first descriptor)
	root := descs[0]
	result, err := buildCompactNode(root, descMap, fields)
	if err != nil {
		return nil, err
	}

	return result, nil
}

func buildCompactNode(desc *v2.Descriptor, descMap map[string]*v2.Descriptor, fields map[string]string) (*compactComponent, error) {
	node := &compactComponent{
		Component: desc.Component.Name,
		Version:   desc.Component.Version,
	}

	// Extract fields from each resource
	for _, res := range desc.Component.Resources {
		resJSON, err := json.Marshal(res)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal resource %s: %w", res.Name, err)
		}
		var resMap map[string]any
		if err := json.Unmarshal(resJSON, &resMap); err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource %s: %w", res.Name, err)
		}

		entry := map[string]any{"name": res.Name}
		for fieldName, path := range fields {
			val := extractPath(resMap, path)
			if val != nil {
				entry[fieldName] = val
			}
		}

		node.Resources = append(node.Resources, entry)
	}

	// Recursively build child references
	for _, ref := range desc.Component.References {
		key := ref.Component + ":" + ref.Version
		childDesc, ok := descMap[key]
		if !ok {
			continue
		}
		child, err := buildCompactNode(childDesc, descMap, fields)
		if err != nil {
			return nil, err
		}
		node.References = append(node.References, *child)
	}

	return node, nil
}

// extractPath extracts a value from a nested map using dot-separated path notation.
// For example, "access.imageReference" extracts map["access"]["imageReference"].
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
