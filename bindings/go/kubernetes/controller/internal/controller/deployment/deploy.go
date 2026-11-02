package deployment

// deploy.go implements impersonate-apply via ApplySet for OCMDeployment.

import (
	"context"
	"fmt"
	"maps"
	goruntime "runtime"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/applyset"
	renderPkg "ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/controller/deployment/render"
)

// applyEngineObjects impersonates the configured service account, applies the rendered engine
// objects via ApplySet, prunes any stale members, and returns the deployed object references.
func (r *Reconciler) applyEngineObjects(ctx context.Context, dep *v1alpha1.OCMDeployment, engineObjects []*unstructured.Unstructured) ([]v1alpha1.DeployedObjectReference, error) {
	logger := log.FromContext(ctx)

	applyClient, err := r.buildApplyClient(ctx, dep)
	if err != nil {
		return nil, fmt.Errorf("failed to build apply client: %w", err)
	}

	set := applyset.New(applyset.Config{
		Client:          applyClient,
		RESTMapper:      r.resourceRESTMapper,
		Log:             logger,
		ParentNamespace: dep.Namespace,
	}, dep)

	resources := make([]applyset.Resource, 0, len(engineObjects))
	for _, obj := range engineObjects {
		// Label the applyset member.
		obj = obj.DeepCopy()
		obj.SetLabels(mergeLabels(obj.GetLabels(), map[string]string{
			applyset.ApplysetPartOfLabel: applyset.ID(dep),
		}))
		resources = append(resources, applyset.Resource{
			ID:     obj.GetKind() + "/" + obj.GetName(),
			Object: obj,
		})
	}

	logger.Info("projecting applyset")

	metadata, err := set.Project(resources)
	if err != nil {
		return nil, fmt.Errorf("failed to project applyset: %w", err)
	}

	// Patch the parent (OCMDeployment) with the applyset metadata labels/annotations.
	if err := r.patchApplySetMetadata(ctx, dep, metadata); err != nil {
		return nil, fmt.Errorf("failed to patch applyset metadata onto OCMDeployment: %w", err)
	}

	logger.Info("applying engine objects", "count", len(resources))

	applyResult, err := set.Apply(ctx, resources, applyset.ApplyMode{Concurrency: goruntime.NumCPU()})
	if err != nil {
		return nil, fmt.Errorf("applyset apply failed: %w", err)
	}

	if applyResult.Errors() != nil {
		return nil, fmt.Errorf("applyset apply errors: %w", applyResult.Errors())
	}

	propagation := propagationPolicyForDep(dep)

	_, err = set.Prune(ctx, applyset.PruneOptions{
		KeepUIDs:          applyResult.ObservedUIDs(),
		Scope:             metadata.PruneScope(),
		Concurrency:       goruntime.NumCPU(),
		PropagationPolicy: propagation,
	})
	if err != nil {
		return nil, fmt.Errorf("applyset prune failed: %w", err)
	}

	// Build deployed references from the post-apply cluster state so UIDs are populated.
	byID := applyResult.ByID()
	deployed := make([]v1alpha1.DeployedObjectReference, 0, len(engineObjects))
	for _, obj := range engineObjects {
		id := obj.GetKind() + "/" + obj.GetName()
		if item, ok := byID[id]; ok && item.Observed != nil {
			deployed = append(deployed, renderPkg.DeployedObjectReferenceFromUnstructured(item.Observed))
		} else {
			deployed = append(deployed, renderPkg.DeployedObjectReferenceFromUnstructured(obj))
		}
	}

	return deployed, nil
}

// buildApplyClient constructs a client impersonating spec.applyServiceAccountName.
// Falls back to the namespace's default SA when the field is empty.
func (r *Reconciler) buildApplyClient(ctx context.Context, dep *v1alpha1.OCMDeployment) (client.Client, error) {
	saName := dep.Spec.ApplyServiceAccountName
	if saName == "" {
		saName = "default"
	}

	key := client.ObjectKey{Namespace: dep.Namespace, Name: saName}
	if err := r.apiReader.Get(ctx, key, &corev1.ServiceAccount{}); err != nil {
		return nil, fmt.Errorf("service account %s not found: %w", key, err)
	}

	username := fmt.Sprintf("system:serviceaccount:%s:%s", key.Namespace, key.Name)
	cfg := rest.CopyConfig(r.restConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username}

	c, err := client.New(cfg, client.Options{
		Scheme: r.Scheme,
		Mapper: r.resourceRESTMapper,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create impersonation client for %s: %w", username, err)
	}

	return c, nil
}

// pruneOnDelete prunes or orphans all applyset members on OCMDeployment deletion.
func (r *Reconciler) pruneOnDelete(ctx context.Context, dep *v1alpha1.OCMDeployment) error {
	logger := log.FromContext(ctx)

	// Restore GVK so applyset.ID() hashes the same value as at apply-time.
	setApplySetGVK(dep)

	applyClient, err := r.buildApplyClient(ctx, dep)
	if err != nil {
		// SA gone: if Orphan, strip labels anyway using the controller's own client.
		if dep.Spec.DeletionPolicy == v1alpha1.DeletionPolicyOrphan {
			logger.Info("service account gone during Orphan delete, releasing namespaced applyset members with controller identity (Namespace members left untouched)")

			return r.releaseApplySetMembers(ctx, r.Client, dep, false, logger)
		}

		return fmt.Errorf("build apply client failed during delete: %w", err)
	}

	set := applyset.New(applyset.Config{
		Client:          applyClient,
		RESTMapper:      r.resourceRESTMapper,
		Log:             logger,
		ParentNamespace: dep.Namespace,
	}, dep)

	metadata, err := set.Project(nil)
	if err != nil {
		return fmt.Errorf("failed to project applyset for deletion: %w", err)
	}

	if dep.Spec.DeletionPolicy == v1alpha1.DeletionPolicyOrphan {
		return r.releaseApplySetMembers(ctx, applyClient, dep, true, logger)
	}

	propagation := propagationPolicyForDep(dep)

	_, err = set.Prune(ctx, applyset.PruneOptions{
		KeepUIDs:          nil, // prune everything
		Scope:             metadata.PruneScope(),
		Concurrency:       goruntime.NumCPU(),
		PropagationPolicy: propagation,
	})
	if err != nil {
		return fmt.Errorf("applyset prune on delete failed: %w", err)
	}

	// Prune the target namespace if configured.
	if ns := targetNS(dep); ns != "" && ns != dep.Namespace {
		if dep.Spec.Namespace != nil && dep.Spec.Namespace.PruneNamespace {
			nsObj := &unstructured.Unstructured{}
			nsObj.SetAPIVersion("v1")
			nsObj.SetKind("Namespace")
			nsObj.SetName(ns)
			if err := applyClient.Delete(ctx, nsObj); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to prune target namespace %s: %w", ns, err)
			}
		}
	}

	return nil
}

// releaseApplySetMembers removes the applyset part-of label and owner reference from
// every member, so another tool can adopt them (Orphan policy). Cluster-scoped Namespace
// members are released only when releaseClusterScoped is true — i.e. through the impersonated
// service account. The controller never manages namespace lifecycle under its own identity, so
// the SA-gone fallback passes false and leaves any Namespace member untouched.
func (r *Reconciler) releaseApplySetMembers(ctx context.Context, c client.Client, dep *v1alpha1.OCMDeployment, releaseClusterScoped bool, logger logr.Logger) error {
	applySetID := applyset.ID(dep)
	labelSelector := client.MatchingLabels{applyset.ApplysetPartOfLabel: applySetID}

	// List all namespace-scoped members bearing the applyset label in this namespace.
	var objs unstructured.UnstructuredList
	if err := c.List(ctx, &objs, client.InNamespace(dep.Namespace), labelSelector); err != nil {
		logger.Error(err, "failed to list applyset members for release")
	} else {
		for i := range objs.Items {
			releaseMember(ctx, c, &objs.Items[i], dep, logger)
		}
	}

	if !releaseClusterScoped {
		return nil
	}

	// List cluster-scoped Namespace members bearing the applyset label (InNamespace excludes them).
	var nsList unstructured.UnstructuredList
	nsList.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"})
	if err := c.List(ctx, &nsList, labelSelector); err != nil {
		logger.Error(err, "failed to list cluster-scoped applyset namespace members for release")
	} else {
		for i := range nsList.Items {
			releaseMember(ctx, c, &nsList.Items[i], dep, logger)
		}
	}

	return nil
}

// releaseMember strips the applyset part-of label and the owner reference pointing to dep from obj.
func releaseMember(ctx context.Context, c client.Client, obj *unstructured.Unstructured, dep *v1alpha1.OCMDeployment, logger logr.Logger) {
	orig := obj.DeepCopy()

	// Remove applyset part-of label.
	lbls := obj.GetLabels()
	delete(lbls, applyset.ApplysetPartOfLabel)
	obj.SetLabels(lbls)

	// Remove owner reference pointing to dep.
	refs := obj.GetOwnerReferences()
	filtered := refs[:0]
	for _, ref := range refs {
		if ref.UID != dep.UID {
			filtered = append(filtered, ref)
		}
	}
	obj.SetOwnerReferences(filtered)

	if err := c.Patch(ctx, obj, client.MergeFrom(orig)); err != nil {
		logger.Error(err, "failed to release applyset member", "name", obj.GetName())
	}
}

// patchApplySetMetadata patches the applyset labels/annotations onto the OCMDeployment itself.
func (r *Reconciler) patchApplySetMetadata(ctx context.Context, dep *v1alpha1.OCMDeployment, metadata applyset.Metadata) error {
	patch := client.MergeFrom(dep.DeepCopy())
	lbls := mergeLabels(dep.GetLabels(), metadata.Labels())
	dep.SetLabels(lbls)
	anns := mergeLabels(dep.GetAnnotations(), metadata.Annotations())
	dep.SetAnnotations(anns)

	return r.Patch(ctx, dep, patch)
}

func propagationPolicyForDep(dep *v1alpha1.OCMDeployment) *metav1.DeletionPropagation {
	switch dep.Spec.DeletionPolicy {
	case v1alpha1.DeletionPolicyForeground:
		p := metav1.DeletePropagationForeground

		return &p
	default:
		p := metav1.DeletePropagationBackground

		return &p
	}
}

func mergeLabels(base, overlay map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overlay))
	maps.Copy(out, base)

	maps.Copy(out, overlay)

	return out
}

func targetNS(dep *v1alpha1.OCMDeployment) string {
	if dep.Spec.Namespace != nil {
		return dep.Spec.Namespace.TargetNamespace
	}

	return ""
}

// SetApplySetGVK sets the GVK on the OCMDeployment so applyset.ID() can compute the stable ID.
// Called at the start of reconcile, before any apply.
func setApplySetGVK(dep *v1alpha1.OCMDeployment) {
	dep.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(v1alpha1.KindOCMDeployment))
}
