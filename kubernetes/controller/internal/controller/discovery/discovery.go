package discovery

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/log"

	syncdag "ocm.software/open-component-model/bindings/go/dag/sync"
	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/repository/component/resolvers"
	"ocm.software/open-component-model/bindings/go/runtime"
)

// errShortCircuit is returned by resolverAndDiscoverer.Discover when the
// parent being discovered is the sought target vertex. The reconciler treats
// it as a success (not an error) so the graph traversal stops as soon as the
// target has been resolved.
var errShortCircuit = errors.New("short-circuit: discovery target found")

// resolverAndDiscoverer bridges the OCM repository resolver and the DAG
// discoverer: it turns vertex keys into component descriptors (Resolve) and
// enumerates a descriptor's references as child keys (Discover).
type resolverAndDiscoverer struct {
	repositoryResolver resolvers.ComponentVersionRepositoryResolver
	// shortCircuitKey, if non-empty, causes Discover to return no children and
	// errShortCircuit when a parent's identity string matches this key. This
	// propagates through the DAG discoverer's errgroup and cancels sibling
	// in-flight resolutions, terminating traversal as soon as the target
	// vertex has been resolved.
	shortCircuitKey string
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

	if r.shortCircuitKey != "" {
		parentKey := parent.Component.ToIdentity().String()
		if parentKey == r.shortCircuitKey {
			logger.V(1).Info("short-circuit: target found, stopping traversal", "target", parentKey)
			return nil, errShortCircuit
		}
	}

	children := make([]string, 0, len(parent.Component.References))
	for i := range parent.Component.References {
		ref := &parent.Component.References[i]
		children = append(children, ref.ToComponentIdentity().String())
	}
	logger.V(1).Info("discovering children", "component", parent.Component.ToIdentity().String(), "children", children)
	return children, nil
}
