package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	"ocm.software/open-component-model/bindings/go/runtime"
	deliveryv1alpha1 "ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/configuration"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution/workerpool"
)

const (
	labelManagedBy      = "delivery.ocm.software/managed-by"
	labelDeploymentName = "delivery.ocm.software/deployment-name"
	labelManagedByValue = "deployment"
)

// ensureRepository creates or updates a Repository CR via SSA.
func (r *DeploymentReconciler) ensureRepository(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
	p *OCMPipeline,
) error {
	name := p.RepoName(deployment.Name)

	repoSpec, err := json.Marshal(map[string]any{
		"baseUrl": p.RepoURL,
		"type":    p.RepoType,
	})
	if err != nil {
		return err
	}

	repo := &deliveryv1alpha1.Repository{
		TypeMeta: metav1.TypeMeta{
			APIVersion: deliveryv1alpha1.GroupVersion.String(),
			Kind:       "Repository",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: deployment.Namespace,
			Labels:    managedLabels(deployment.Name),
		},
		Spec: deliveryv1alpha1.RepositorySpec{
			RepositorySpec: &apiextensionsv1.JSON{Raw: repoSpec},
			Interval:       metav1.Duration{Duration: mustParseDuration(p.RepoInterval)},
		},
	}

	if err := controllerutil.SetControllerReference(deployment, repo, r.Scheme); err != nil {
		logf.FromContext(ctx).V(4).Info("could not set owner reference on repository", "error", err)
	}

	return r.Patch(ctx, repo, client.Apply, client.FieldOwner(deploymentFieldManager), client.ForceOwnership)
}

// ensureComponent creates or updates a Component CR via SSA.
func (r *DeploymentReconciler) ensureComponent(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
	p *OCMPipeline,
) error {
	name := p.CompName(deployment.Name)
	repoName := p.RepoName(deployment.Name)

	comp := &deliveryv1alpha1.Component{
		TypeMeta: metav1.TypeMeta{
			APIVersion: deliveryv1alpha1.GroupVersion.String(),
			Kind:       "Component",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: deployment.Namespace,
			Labels:    managedLabels(deployment.Name),
		},
		Spec: deliveryv1alpha1.ComponentSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: repoName},
			Component:     p.Component,
			Semver:        p.Semver,
			Interval:      metav1.Duration{Duration: mustParseDuration(p.CompInterval)},
		},
	}

	// Propagate verification entries from the pipeline to the Component CR.
	for _, v := range p.Verify {
		entry := deliveryv1alpha1.Verification{
			Signature: v.Signature,
		}
		if v.SecretRef != "" {
			entry.SecretRef = corev1.LocalObjectReference{Name: v.SecretRef}
		}
		if v.Value != "" {
			entry.Value = v.Value
		}
		comp.Spec.Verify = append(comp.Spec.Verify, entry)
	}

	if err := controllerutil.SetControllerReference(deployment, comp, r.Scheme); err != nil {
		logf.FromContext(ctx).V(4).Info("could not set owner reference on component", "error", err)
	}

	return r.Patch(ctx, comp, client.Apply, client.FieldOwner(deploymentFieldManager), client.ForceOwnership)
}

// ensureResource creates or updates a Resource CR via SSA.
func (r *DeploymentReconciler) ensureResource(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
	p *OCMPipeline,
	tociFields []string,
) error {
	name := p.ResName(deployment.Name)
	compName := p.CompName(deployment.Name)

	additionalFields, err := buildAdditionalStatusFields(tociFields)
	if err != nil {
		return fmt.Errorf("building additional status fields: %w", err)
	}

	// Build the resource identity.
	identity := make(map[string]string, len(p.ResourceID))
	for k, v := range p.ResourceID {
		identity[k] = v
	}

	res := &deliveryv1alpha1.Resource{
		TypeMeta: metav1.TypeMeta{
			APIVersion: deliveryv1alpha1.GroupVersion.String(),
			Kind:       "Resource",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: deployment.Namespace,
			Labels:    managedLabels(deployment.Name),
		},
		Spec: deliveryv1alpha1.ResourceSpec{
			ComponentRef: corev1.LocalObjectReference{Name: compName},
			Resource: deliveryv1alpha1.ResourceID{
				ByReference: deliveryv1alpha1.ResourceReference{
					Resource:      identity,
					ReferencePath: buildReferencePath(p.ReferencePath),
				},
			},
			AdditionalStatusFields: &apiextensionsv1.JSON{Raw: additionalFields},
		},
	}

	if err := controllerutil.SetControllerReference(deployment, res, r.Scheme); err != nil {
		logf.FromContext(ctx).V(4).Info("could not set owner reference on resource", "error", err)
	}

	return r.Patch(ctx, res, client.Apply, client.FieldOwner(deploymentFieldManager), client.ForceOwnership)
}

// deleteAutoCreatedObjects deletes all auto-created OCM objects for a Deployment.
func (r *DeploymentReconciler) deleteAutoCreatedObjects(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
) error {
	log := logf.FromContext(ctx)
	labels := client.MatchingLabels{
		labelManagedBy:      labelManagedByValue,
		labelDeploymentName: deployment.Name,
	}
	ns := client.InNamespace(deployment.Namespace)

	// Delete Resources first, then Components, then Repositories (reverse dependency order).
	for _, list := range []client.ObjectList{
		&deliveryv1alpha1.ResourceList{},
		&deliveryv1alpha1.ComponentList{},
		&deliveryv1alpha1.RepositoryList{},
	} {
		if err := r.List(ctx, list, labels, ns); err != nil {
			log.V(4).Info("could not list auto-created objects", "error", err)
			continue
		}

		items := extractObjectsFromList(list)
		for _, obj := range items {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				log.Error(err, "failed to delete auto-created object",
					"kind", obj.GetObjectKind().GroupVersionKind().Kind,
					"name", obj.GetName())
			} else {
				log.Info("deleted auto-created object",
					"kind", obj.GetObjectKind().GroupVersionKind().Kind,
					"name", obj.GetName())
			}
		}
	}

	return nil
}

// --- Helpers ---

func managedLabels(deploymentName string) map[string]string {
	return map[string]string{
		labelManagedBy:      labelManagedByValue,
		labelDeploymentName: deploymentName,
	}
}

// mustParseDuration parses a duration string like "10m", "1h", "30s".
// Returns 10 minutes as default if parsing fails.
func mustParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 10 * time.Minute
	}
	return d
}

func extractObjectsFromList(list client.ObjectList) []client.Object {
	switch l := list.(type) {
	case *deliveryv1alpha1.ResourceList:
		objs := make([]client.Object, len(l.Items))
		for i := range l.Items {
			objs[i] = &l.Items[i]
		}
		return objs
	case *deliveryv1alpha1.ComponentList:
		objs := make([]client.Object, len(l.Items))
		for i := range l.Items {
			objs[i] = &l.Items[i]
		}
		return objs
	case *deliveryv1alpha1.RepositoryList:
		objs := make([]client.Object, len(l.Items))
		for i := range l.Items {
			objs[i] = &l.Items[i]
		}
		return objs
	default:
		return nil
	}
}

// buildReferencePath converts a slice of reference names into the
// runtime.Identity slice expected by ResourceReference.ReferencePath.
func buildReferencePath(refs []string) []runtime.Identity {
	if len(refs) == 0 {
		return nil
	}
	path := make([]runtime.Identity, len(refs))
	for i, name := range refs {
		path[i] = runtime.Identity{"name": name}
	}
	return path
}

// --- Component Descriptor Introspection ---

// preScanAndBuildIndex implements Phase 0 of the reconciliation: it scans all
// templates for full pipeline chains, creates Repository + Component CRs,
// waits for readiness, fetches component descriptors, and builds a
// ResourceIndex mapping resource identities to their providing components.
//
// Returns nil index (not an error) if no pipeline chains are found — this
// means the deployment only uses cross-resource references, not OCM.
func (r *DeploymentReconciler) preScanAndBuildIndex(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
) (*ResourceIndex, error) {
	log := logf.FromContext(ctx)

	// Step 1: Scan templates for pipeline chains.
	pipelines, expressionResources, err := preScanTemplates(deployment.Spec.Resources)
	if err != nil {
		return nil, fmt.Errorf("pre-scan templates: %w", err)
	}
	if len(pipelines) == 0 {
		return nil, nil
	}

	// Step 2: Ensure Repository + Component CRs for each pipeline.
	for _, pipeline := range pipelines {
		if err := r.ensureRepository(ctx, deployment, pipeline); err != nil {
			return nil, fmt.Errorf("ensure repository for %s: %w", pipeline.RepoURL, err)
		}
		if err := r.ensureComponent(ctx, deployment, pipeline); err != nil {
			return nil, fmt.Errorf("ensure component for %s: %w", pipeline.Component, err)
		}
	}

	// Step 3: Wait for all Component CRs to be ready and fetch descriptors.
	index := NewResourceIndex()

	for _, pipeline := range pipelines {
		compName := pipeline.CompName(deployment.Name)

		comp := &deliveryv1alpha1.Component{}
		if err := r.Get(ctx, k8stypes.NamespacedName{
			Namespace: deployment.Namespace,
			Name:      compName,
		}, comp); err != nil {
			return nil, fmt.Errorf("get component %s: %w", compName, err)
		}

		// Check readiness.
		if !isComponentReady(comp) {
			log.Info("component not ready yet, will retry", "component", compName)
			return nil, fmt.Errorf("component %s not ready yet", compName)
		}

		if comp.Status.Component.RepositorySpec == nil {
			return nil, fmt.Errorf("component %s has no repository spec in status", compName)
		}

		// Step 4: Fetch the component descriptor via the resolver.
		desc, err := r.fetchDescriptor(ctx, deployment, comp)
		if err != nil {
			if errors.Is(err, workerpool.ErrResolutionInProgress) {
				log.Info("component version resolution in progress", "component", compName)
				return nil, fmt.Errorf("component %s resolution in progress", compName)
			}
			return nil, fmt.Errorf("fetch descriptor for %s: %w", compName, err)
		}

		// Step 5: Register all resources from this descriptor in the index.
		for _, res := range desc.Component.Resources {
			identity := make(map[string]string)
			identity["name"] = res.Name
			for k, v := range res.ExtraIdentity {
				identity[k] = v
			}
			index.Add(ResolvedResourceEntry{
				Pipeline:   pipeline,
				Component:  pipeline.Component,
				ResourceID: identity,
			})
		}

		log.V(1).Info("indexed component resources",
			"component", pipeline.Component,
			"resourceCount", len(desc.Component.Resources))
	}

	// Step 6: Register resources discovered from parsed expressions.
	// This covers resources in nested components that aren't directly
	// in the root component's descriptor (e.g., resources reached via
	// componentReference).
	for _, entry := range expressionResources {
		index.Add(entry)
	}

	return index, nil
}

// fetchDescriptor fetches a component descriptor using the resolver
// infrastructure, following the same pattern as the Resource controller.
func (r *DeploymentReconciler) fetchDescriptor(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
	comp *deliveryv1alpha1.Component,
) (*descriptor.Descriptor, error) {
	repoSpec := &runtime.Raw{}
	if err := runtime.NewScheme(runtime.WithAllowUnknown()).Decode(
		bytes.NewReader(comp.Status.Component.RepositorySpec.Raw), repoSpec); err != nil {
		return nil, fmt.Errorf("decode repository spec: %w", err)
	}

	cfg, err := configuration.LoadConfigurations(ctx, r.Client, deployment.GetNamespace(), nil)
	if err != nil {
		return nil, fmt.Errorf("load configurations: %w", err)
	}

	cacheBackedRepo, err := r.Resolver.NewCacheBackedRepository(ctx, &resolution.RepositoryOptions{
		RepositorySpec:  repoSpec,
		Configuration:   cfg,
		SigningRegistry: r.PluginManager.SigningRegistry,
		RequesterFunc: func() workerpool.RequesterInfo {
			return workerpool.RequesterInfo{
				NamespacedName: k8stypes.NamespacedName{
					Namespace: deployment.GetNamespace(),
					Name:      deployment.GetName(),
				},
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create cache-backed repository: %w", err)
	}

	desc, err := cacheBackedRepo.GetComponentVersion(ctx,
		comp.Status.Component.Component,
		comp.Status.Component.Version)
	if err != nil {
		return nil, err
	}

	return desc, nil
}

// isComponentReady checks if a Component CR has a Ready=True condition.
func isComponentReady(comp *deliveryv1alpha1.Component) bool {
	for _, cond := range comp.GetConditions() {
		if cond.Type == deliveryv1alpha1.ReadyCondition {
			return cond.Status == metav1.ConditionTrue
		}
	}
	return false
}
