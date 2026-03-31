package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"ocm.software/open-component-model/bindings/go/dag"
	"ocm.software/open-component-model/bindings/go/plugin/manager"
	deliveryv1alpha1 "ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/resolution"
)

const (
	deploymentFieldManager   = "deployment-controller"
	deploymentRequeueDefault = 5 * time.Second
	deploymentRequeueReady   = 1 * time.Minute
)

// DeploymentReconciler reconciles a Deployment object.
type DeploymentReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Resolver      *resolution.Resolver
	PluginManager *manager.PluginManager
}

// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete

func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	deployment := &deliveryv1alpha1.OCMDeployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if deployment.Spec.Suspend {
		log.Info("reconciliation suspended")
		return ctrl.Result{}, nil
	}

	// Handle deletion.
	if !deployment.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, deployment)
	}

	// Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(deployment, deliveryv1alpha1.OCMDeploymentFinalizer) {
		controllerutil.AddFinalizer(deployment, deliveryv1alpha1.OCMDeploymentFinalizer)
		if err := r.Update(ctx, deployment); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Phase 0: Pre-scan templates, create OCM CRs, build resource index.
	var resourceIndex *ResourceIndex
	if r.Resolver != nil {
		var err error
		resourceIndex, err = r.preScanAndBuildIndex(ctx, deployment)
		if err != nil {
			meta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
				Type:               deliveryv1alpha1.ReadyCondition,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: deployment.Generation,
				Reason:             deliveryv1alpha1.ProgressingWithRetryReason,
				Message:            fmt.Sprintf("building resource index: %v", err),
			})
			_ = r.Status().Update(ctx, deployment)
			return ctrl.Result{RequeueAfter: deploymentRequeueDefault}, nil
		}
	}

	// Build dependency graph from cross-resource references.
	depGraph, resourceDeps, err := buildDependencyGraph(deployment.Spec.Resources)
	if err != nil {
		meta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               deliveryv1alpha1.ReadyCondition,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: deployment.Generation,
			Reason:             deliveryv1alpha1.StalledCondition,
			Message:            fmt.Sprintf("failed to build dependency graph: %v", err),
		})
		_ = r.Status().Update(ctx, deployment)
		return ctrl.Result{}, err
	}

	order, err := depGraph.TopologicalSort()
	if err != nil {
		meta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               deliveryv1alpha1.ReadyCondition,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: deployment.Generation,
			Reason:             deliveryv1alpha1.StalledCondition,
			Message:            fmt.Sprintf("cyclic dependency: %v", err),
		})
		_ = r.Status().Update(ctx, deployment)
		return ctrl.Result{}, err
	}

	// Process resources in dependency order.
	resourceStates := make(map[string]map[string]any)
	allReady := true
	statusList := make([]deliveryv1alpha1.OCMDeploymentResourceStatus, 0, len(order))

	for _, id := range order {
		res := findResourceByID(deployment.Spec.Resources, id)
		if res == nil {
			continue
		}

		templateMap, err := templateToMap(res.Template.Raw)
		if err != nil {
			allReady = false
			statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
				ID:    id,
				Error: fmt.Sprintf("invalid template: %v", err),
			})
			continue
		}

		// Check if all dependencies are resolved.
		deps := resourceDeps[id]
		canResolve := true
		for _, dep := range deps {
			if _, ok := resourceStates[dep]; !ok {
				canResolve = false
				break
			}
		}

		if !canResolve {
			allReady = false
			statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
				ID:    id,
				Error: "waiting for dependencies",
			})
			continue
		}

		// Create a CEL environment with current resource states as variables.
		celEnv, err := newDeploymentCELEnv(ctx, r, deployment, resourceStates, resourceIndex)
		if err != nil {
			allReady = false
			statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
				ID:    id,
				Error: fmt.Sprintf("failed to create CEL environment: %v", err),
			})
			continue
		}

		// Resolve all ${} expressions in the template via CEL.
		resolved, err := r.resolveTemplateExpressions(ctx, celEnv, templateMap, resourceStates)
		if err != nil {
			allReady = false
			statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
				ID:    id,
				Error: fmt.Sprintf("expression resolution failed: %v", err),
			})
			continue
		}

		// Apply the resource.
		obj, err := r.applyResource(ctx, deployment, resolved)
		if err != nil {
			log.Error(err, "failed to apply resource", "id", id)
			allReady = false
			statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
				ID:    id,
				Error: fmt.Sprintf("apply failed: %v", err),
			})
			continue
		}

		// Read back the full object from the cluster to get status.
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			log.Error(err, "failed to read back applied resource", "id", id)
			allReady = false
			statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
				ID: id,
				ObjectReference: &deliveryv1alpha1.DeployedObjectReference{
					APIVersion: obj.GetAPIVersion(),
					Kind:       obj.GetKind(),
					Name:       obj.GetName(),
					Namespace:  obj.GetNamespace(),
				},
				Error: fmt.Sprintf("failed to read back: %v", err),
			})
			continue
		}

		ready := isResourceReady(live)
		if !ready {
			allReady = false
		}

		resourceStates[id] = live.Object

		statusList = append(statusList, deliveryv1alpha1.OCMDeploymentResourceStatus{
			ID: id,
			ObjectReference: &deliveryv1alpha1.DeployedObjectReference{
				APIVersion: live.GetAPIVersion(),
				Kind:       live.GetKind(),
				Name:       live.GetName(),
				Namespace:  live.GetNamespace(),
				UID:        live.GetUID(),
			},
			Ready: ready,
		})
	}

	// --- Phase 6: STATUS ---
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Resources = statusList

	if allReady && len(statusList) == len(deployment.Spec.Resources) {
		meta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               deliveryv1alpha1.ReadyCondition,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: deployment.Generation,
			Reason:             deliveryv1alpha1.SucceededReason,
			Message:            fmt.Sprintf("all %d resources are ready", len(statusList)),
		})
	} else {
		readyCount := 0
		for _, s := range statusList {
			if s.Ready {
				readyCount++
			}
		}
		meta.SetStatusCondition(&deployment.Status.Conditions, metav1.Condition{
			Type:               deliveryv1alpha1.ReadyCondition,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: deployment.Generation,
			Reason:             deliveryv1alpha1.ProgressingWithRetryReason,
			Message:            fmt.Sprintf("%d/%d resources ready", readyCount, len(deployment.Spec.Resources)),
		})
	}

	if err := r.Status().Update(ctx, deployment); err != nil {
		return ctrl.Result{}, err
	}

	if !allReady {
		return ctrl.Result{RequeueAfter: deploymentRequeueDefault}, nil
	}

	return ctrl.Result{RequeueAfter: deploymentRequeueReady}, nil
}

// reconcileDeletion cleans up managed resources and removes the finalizer.
func (r *DeploymentReconciler) reconcileDeletion(ctx context.Context, deployment *deliveryv1alpha1.OCMDeployment) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(deployment, deliveryv1alpha1.OCMDeploymentFinalizer) {
		return ctrl.Result{}, nil
	}

	// Delete consumer resources (from status) in reverse order.
	for i := len(deployment.Status.Resources) - 1; i >= 0; i-- {
		ref := deployment.Status.Resources[i].ObjectReference
		if ref == nil {
			continue
		}

		obj := &unstructured.Unstructured{}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			log.Error(err, "failed to parse apiVersion for cleanup", "apiVersion", ref.APIVersion)
			continue
		}
		obj.SetGroupVersionKind(gv.WithKind(ref.Kind))
		obj.SetName(ref.Name)
		obj.SetNamespace(ref.Namespace)

		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			log.Error(err, "failed to delete managed resource", "kind", ref.Kind, "name", ref.Name)
		} else {
			log.Info("deleted managed resource", "kind", ref.Kind, "name", ref.Name)
		}
	}

	// Delete auto-created OCM objects (Repository, Component, Resource).
	if err := r.deleteAutoCreatedObjects(ctx, deployment); err != nil {
		log.Error(err, "failed to delete auto-created objects")
	}

	controllerutil.RemoveFinalizer(deployment, deliveryv1alpha1.OCMDeploymentFinalizer)
	return ctrl.Result{}, r.Update(ctx, deployment)
}

// applyResource applies an unstructured resource using server-side apply.
func (r *DeploymentReconciler) applyResource(
	ctx context.Context,
	deployment *deliveryv1alpha1.OCMDeployment,
	templateMap map[string]any,
) (*unstructured.Unstructured, error) {
	obj := &unstructured.Unstructured{Object: templateMap}

	// Default namespace to the deployment's namespace.
	if obj.GetNamespace() == "" {
		obj.SetNamespace(deployment.Namespace)
	}

	// Set owner reference so GC cleans up if the Deployment is deleted.
	gvk := obj.GroupVersionKind()
	if err := controllerutil.SetControllerReference(deployment, obj, r.Scheme); err != nil {
		logf.FromContext(ctx).V(4).Info("could not set owner reference", "error", err, "kind", obj.GetKind())
	}

	// Server-side apply.
	obj.SetManagedFields(nil)
	obj.SetResourceVersion("")
	obj.SetGroupVersionKind(gvk)

	if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(deploymentFieldManager), client.ForceOwnership); err != nil {
		return nil, fmt.Errorf("server-side apply failed for %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}

	return obj, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&deliveryv1alpha1.OCMDeployment{}).
		Named("ocmdeployment").
		Complete(r)
}

// --- Dependency Graph ---

// buildDependencyGraph parses all resource templates for ${} cross-resource
// references (non-pipeline), extracts dependencies, and builds a DAG.
func buildDependencyGraph(resources []deliveryv1alpha1.OCMDeploymentResource) (
	*dag.DirectedAcyclicGraph[string], map[string][]string, error,
) {
	graph := dag.NewDirectedAcyclicGraph[string]()

	allIDs := make(map[string]bool, len(resources))
	for _, res := range resources {
		allIDs[res.ID] = true
	}

	for _, res := range resources {
		if err := graph.AddVertex(res.ID); err != nil {
			return nil, nil, fmt.Errorf("duplicate resource ID %q: %w", res.ID, err)
		}
	}

	resourceDeps := make(map[string][]string, len(resources))
	for _, res := range resources {
		templateMap, err := templateToMap(res.Template.Raw)
		if err != nil {
			return nil, nil, fmt.Errorf("resource %q: invalid template JSON: %w", res.ID, err)
		}

		deps := extractDependencies(templateMap, allIDs, res.ID)
		resourceDeps[res.ID] = deps

		for _, dep := range deps {
			if err := graph.AddEdge(res.ID, dep); err != nil {
				return nil, nil, fmt.Errorf("dependency %q → %q: %w", res.ID, dep, err)
			}
		}
	}

	return graph, resourceDeps, nil
}

// extractDependencies walks a template map and finds all resource IDs referenced
// in cross-resource ${} expressions. Pipeline expressions like repoByRef(...)
// are naturally excluded because their first token contains "(" which never
// matches a valid resource ID.
func extractDependencies(templateMap map[string]any, allIDs map[string]bool, selfID string) []string {
	depsSet := make(map[string]bool)
	walkStrings(templateMap, func(s string) {
		for _, expr := range extractExpressions(s) {
			parts := strings.SplitN(strings.TrimSpace(expr), ".", 2)
			id := parts[0]
			if allIDs[id] && id != selfID && !depsSet[id] {
				depsSet[id] = true
			}
		}
	})

	deps := make([]string, 0, len(depsSet))
	for dep := range depsSet {
		deps = append(deps, dep)
	}
	return deps
}

// walkStrings recursively walks a map/slice structure and calls fn for each string value.
func walkStrings(val any, fn func(string)) {
	switch v := val.(type) {
	case map[string]any:
		for _, child := range v {
			walkStrings(child, fn)
		}
	case []any:
		for _, item := range v {
			walkStrings(item, fn)
		}
	case string:
		fn(v)
	}
}

// extractExpressions returns all ${expr} contents from a string.
// It handles nested braces from map literals like ${...resourceByIdentity({"name": "res"})...}.
func extractExpressions(s string) []string {
	var exprs []string
	for i := 0; i < len(s); i++ {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			// Find the matching closing brace, tracking nesting depth.
			depth := 1
			start := i + 2
			j := start
			inString := false
			for j < len(s) && depth > 0 {
				if inString {
					if s[j] == '\\' && j+1 < len(s) {
						j++ // skip escaped char
					} else if s[j] == '"' {
						inString = false
					}
				} else {
					switch s[j] {
					case '"':
						inString = true
					case '{':
						depth++
					case '}':
						depth--
					}
				}
				if depth > 0 {
					j++
				}
			}
			if depth == 0 {
				exprs = append(exprs, s[start:j])
				i = j // advance past the closing }
			}
		}
	}
	return exprs
}

// isStandaloneExpression returns true if the entire string is a single ${...} expression
// with no surrounding text.
func isStandaloneExpression(s string) bool {
	exprs := extractExpressions(s)
	if len(exprs) != 1 {
		return false
	}
	return s == "${"+exprs[0]+"}"
}

// --- Expression Resolution ---

// resolveTemplateExpressions recursively resolves all ${} expressions in a template map.
// All expressions are evaluated as CEL — both pipeline expressions and cross-resource references.
func (r *DeploymentReconciler) resolveTemplateExpressions(
	ctx context.Context,
	celEnv *cel.Env,
	templateMap map[string]any,
	resourceStates map[string]map[string]any,
) (map[string]any, error) {
	resolved, err := resolveValue(celEnv, templateMap, resourceStates)
	if err != nil {
		return nil, err
	}
	result, ok := resolved.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected map, got %T", resolved)
	}
	return result, nil
}

func resolveValue(
	celEnv *cel.Env,
	val any,
	states map[string]map[string]any,
) (any, error) {
	switch v := val.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for key, child := range v {
			resolved, err := resolveValue(celEnv, child, states)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			result[key] = resolved
		}
		return result, nil
	case []any:
		result := make([]any, len(v))
		for i, item := range v {
			resolved, err := resolveValue(celEnv, item, states)
			if err != nil {
				return nil, fmt.Errorf("index %d: %w", i, err)
			}
			result[i] = resolved
		}
		return result, nil
	case string:
		return resolveString(celEnv, v, states)
	default:
		return val, nil
	}
}

// resolveString resolves ${} expressions in a string value.
// All expressions are evaluated as CEL. Standalone expressions return native types;
// interpolated expressions return strings.
func resolveString(
	celEnv *cel.Env,
	s string,
	states map[string]map[string]any,
) (any, error) {
	// Build the activation map (CEL variables) from resource states.
	vars := make(map[string]any, len(states))
	for id, state := range states {
		vars[id] = state
	}

	if isStandaloneExpression(s) {
		expr := s[2 : len(s)-1] // trim ${ and }
		return evaluateCELExpression(celEnv, strings.TrimSpace(expr), vars)
	}

	// String interpolation: replace all ${expr} occurrences.
	expressions := extractExpressions(s)
	if len(expressions) == 0 {
		return s, nil
	}

	result := s
	for _, expr := range expressions {
		val, err := evaluateCELExpression(celEnv, strings.TrimSpace(expr), vars)
		if err != nil {
			return nil, err
		}
		result = strings.Replace(result, "${"+expr+"}", fmt.Sprintf("%v", val), 1)
	}
	return result, nil
}

// --- Helpers ---

func templateToMap(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func findResourceByID(resources []deliveryv1alpha1.OCMDeploymentResource, id string) *deliveryv1alpha1.OCMDeploymentResource {
	for i := range resources {
		if resources[i].ID == id {
			return &resources[i]
		}
	}
	return nil
}

// isResourceReady checks if a Kubernetes resource is ready by looking at standard conditions.
func isResourceReady(obj *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}

	for _, c := range conditions {
		condMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		condType, _ := condMap["type"].(string)
		condStatus, _ := condMap["status"].(string)

		if (condType == "Ready" || condType == "Available") && condStatus == "True" {
			return true
		}
	}

	return false
}
