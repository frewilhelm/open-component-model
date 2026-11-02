package deployment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/resolution"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/status"
)

// argoNoStatusTimeout is the default time to wait for an Argo Application to report status
// before setting Ready=False with a diagnostic message.
const argoNoStatusTimeout = 5 * time.Minute

const (
	// deployArtifactCacheSize bounds the number of cached parsed deploy artifacts. Each entry is a
	// few KB of parsed values keyed by a content digest; 256 comfortably exceeds the live set of
	// (OCMDeployment × version) in one process while staying well under a MB.
	deployArtifactCacheSize = 256
	// deployArtifactCacheTTL is 0: no time-based expiry. The cached artifact holds only immutable
	// parsed values keyed by a content-addressed digest, so an entry can never go stale — eviction
	// is bounded by LRU count alone. (Contrast resolution.Resolver's repoCache, which pins a plugin
	// manager + credential graph and therefore must expire.)
	deployArtifactCacheTTL = 0 * time.Second
)

// Reconciler reconciles an OCMDeployment object.
type Reconciler struct {
	*ocm.BaseReconciler

	Resolver *resolution.Resolver

	// MaxResourceSizeBytes is the maximum size in bytes a downloaded resource blob may contain.
	// 0 disables the limit.
	MaxResourceSizeBytes int64

	// restConfig is the base config impersonated clients are derived from.
	restConfig *rest.Config
	// resourceRESTMapper is the RESTMapper for building the ApplySet client.
	resourceRESTMapper meta.RESTMapper
	// apiReader reads cluster objects (e.g. ServiceAccounts) uncached.
	apiReader client.Reader

	// deployArtifactCache memoizes the parsed deploy artifact per deploy-resource digest so the
	// chart is downloaded + extracted once per content version, not on every spin-pass reconcile
	// while child Resource CRs resolve.
	deployArtifactCache *expirable.LRU[string, *deploySource]
}

var _ ocm.Reconciler = (*Reconciler)(nil)

// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=ocmdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=delivery.ocm.software,resources=resources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get
// The Orphan-delete fallback (releaseApplySetMembers) uses the manager client to strip the applyset
// label and owner reference from engine objects when the impersonated apply SA is gone, so patch is
// required in addition to the get;list;watch used for health checks and watches.
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=ocirepositories,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch;patch

// resourceReadyChanged enqueues the parent OCMDeployment for an owned Resource event only when the
// child's readiness actually changes — on create, delete, or a flip of the Ready condition in either
// direction — and drops the intervening status writes (ResolutionInProgress, digest updates). Without
// it, localizing N images (N child Resources, each emitting several status writes) wakes the parent
// O(N) times per child and re-scans all N each time, i.e. O(N²) churn. The parent still re-reconciles
// on every meaningful readiness transition, so it never misses a child going ready or not-ready.
func resourceReadyChanged() predicate.Predicate {
	ready := func(o client.Object) bool {
		res, ok := o.(*v1alpha1.Resource)
		if !ok {
			return false
		}

		return meta.IsStatusConditionTrue(res.GetConditions(), v1alpha1.ReadyCondition)
	}

	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return ready(e.ObjectOld) != ready(e.ObjectNew)
		},
	}
}

// engineObjectCreateIgnored drops Create events for watched engine objects
// (HelmRelease/Kustomization/Application/OCIRepository/HelmRepository). The controller creates those
// objects itself via ApplySet during reconcile, so the resulting Create event is a self-trigger that
// would re-enqueue the same OCMDeployment for a redundant pass. Update and Delete still enqueue, so
// drift (a manual edit or deletion of an engine object) and readiness transitions are never missed.
func engineObjectCreateIgnored() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, concurrency int) error {
	r.restConfig = mgr.GetConfig()
	r.resourceRESTMapper = mgr.GetRESTMapper()
	r.apiReader = mgr.GetAPIReader()
	r.deployArtifactCache = expirable.NewLRU[string, *deploySource](deployArtifactCacheSize, nil, deployArtifactCacheTTL)

	// engineObjectMapper re-enqueues the parent OCMDeployment when an engine
	// object (HelmRelease, Kustomization, Application, OCIRepository, HelmRepository) changes.
	// Engine objects are named after their parent OCMDeployment and live in
	// the same namespace, so the mapping is a direct name+namespace lookup.
	engineObjectMapper := handler.EnqueueRequestsFromMapFunc(
		func(_ context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{
				NamespacedName: types.NamespacedName{
					Namespace: obj.GetNamespace(),
					Name:      obj.GetName(),
				},
			}}
		},
	)

	// Construct unstructured GVKs for the four engine object types so we can
	// watch them without importing their SDK packages.
	makeUnstructured := func(group, version, kind string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
		return u
	}

	// isPresent returns true when the GVK is registered in the REST mapper.
	// Engine CRDs (Flux, Argo) may be absent in unit-test envtest clusters;
	// skip their watches rather than crashing the informer start.
	isPresent := func(group, version, kind string) bool {
		_, err := mgr.GetRESTMapper().RESTMapping(
			schema.GroupKind{Group: group, Kind: kind}, version)
		return err == nil
	}

	bld := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.OCMDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&v1alpha1.Repository{}).
		Owns(&v1alpha1.Component{}).
		Owns(&v1alpha1.Resource{}, builder.WithPredicates(resourceReadyChanged()))

	// Watch engine objects so a HelmRelease/Kustomization/Application flipping
	// to Ready triggers an OCMDeployment reconcile to update status.
	// Watches are skipped when the CRD is not installed (e.g. in envtest).
	for _, eng := range []struct{ group, version, kind string }{
		{"helm.toolkit.fluxcd.io", "v2", "HelmRelease"},
		{"kustomize.toolkit.fluxcd.io", "v1", "Kustomization"},
		{"source.toolkit.fluxcd.io", "v1", "OCIRepository"},
		{"source.toolkit.fluxcd.io", "v1", "HelmRepository"},
		{"argoproj.io", "v1alpha1", "Application"},
	} {
		if isPresent(eng.group, eng.version, eng.kind) {
			bld = bld.Watches(makeUnstructured(eng.group, eng.version, eng.kind), engineObjectMapper,
				builder.WithPredicates(engineObjectCreateIgnored()))
		}
	}

	return bld.WithOptions(controller.Options{
		MaxConcurrentReconciles: concurrency,
		RateLimiter:             r.RateLimit.Limiter(),
	}).Complete(r)
}

//nolint:cyclop,funlen // reconcile loop is intentionally kept in one place for readability
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, err error) {
	logger := log.FromContext(ctx)
	logger.Info("starting ocmdeployment reconciliation")

	dep := &v1alpha1.OCMDeployment{}
	if err := r.Get(ctx, req.NamespacedName, dep); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	old := dep.DeepCopy()
	defer func(ctx context.Context) {
		status.UpdateBeforePatch(dep, r.EventRecorder, 0, err)

		if !equality.Semantic.DeepEqual(dep.Status, old.Status) {
			err = errors.Join(err, r.GetClient().Status().Patch(ctx, dep, client.MergeFrom(old)))
		}
	}(ctx)

	// Handle deletion.
	if !dep.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, dep)
	}

	// Add finalizer on first reconcile.
	if updated := controllerutil.AddFinalizer(dep, v1alpha1.OCMDeploymentFinalizer); updated {
		err := r.Update(ctx, dep)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}

		return ctrl.Result{Requeue: true}, nil //nolint:staticcheck // SA1019: pending replacement, see https://github.com/open-component-model/open-component-model/issues/2120
	}

	// Suspend guard.
	if dep.Spec.Suspend {
		logger.Info("ocmdeployment is suspended, skipping reconciliation")

		return ctrl.Result{}, nil
	}

	// Run the business logic pipeline.
	result, err := r.reconcile(ctx, dep)
	if err != nil {
		return result, err
	}

	return result, nil
}

// reconcile runs the full pipeline for a non-deleted, non-suspended OCMDeployment.
func (r *Reconciler) reconcile(ctx context.Context, dep *v1alpha1.OCMDeployment) (ctrl.Result, error) {
	// Step 1–2: ensure child OCM CRs.
	repo, err := r.ensureRepository(ctx, dep)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.GetRepositoryFailedReason, err.Error())

		return ctrl.Result{}, err
	}

	comp, err := r.ensureComponent(ctx, dep, repo)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.GetComponentVersionFailedReason, err.Error())

		return ctrl.Result{}, err
	}

	deployRes, err := r.ensureDeployResource(ctx, dep, comp)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResourceIsNotAvailable, err.Error())

		return ctrl.Result{}, err
	}

	if deployRes == nil {
		// child Resource not ready yet — requeue implicitly via Owns watch.
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResolutionInProgress, "waiting for deploy resource to become ready")

		return ctrl.Result{}, nil
	}

	// Step 3: download deploy artifact.
	source, err := r.downloadDeployArtifact(ctx, dep, deployRes)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.GetOCMResourceFailedReason, err.Error())

		return ctrl.Result{}, err
	}

	// Step 4: localize (Helm) or build image transformer list (kustomize).
	localizeResult, err := r.localize(ctx, dep, source)
	if err != nil {
		// localize sets its own MarkNotReady for UnsupportedConfiguration / conflicts.
		return ctrl.Result{}, err
	}

	// Warn for each localized image that could not be digest-pinned: the chart exposes only a
	// tag, so the relocated pull rides on the unsigned access with no digest to verify it.
	for _, applied := range localizeResult.AppliedLocalizations {
		if !applied.DigestPinned {
			r.Eventf(dep, corev1.EventTypeWarning, "ImageNotDigestPinned",
				"localized image %q is not digest-pinned; the chart exposes only a tag and the pull is unverified", applied.Applied)
		}
	}

	// Step 5: config resources + merge.
	if err := r.mergeConfig(ctx, dep, comp, source, localizeResult); err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.GetOCMResourceFailedReason, err.Error())

		return ctrl.Result{}, err
	}

	// Step 6a: render engine objects.
	engineObjects, err := r.renderEngine(dep, deployRes, source, localizeResult)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ApplyFailed, err.Error())

		return ctrl.Result{}, err
	}

	// Step 6b: impersonate-apply via applyset.
	deployed, err := r.applyEngineObjects(ctx, dep, engineObjects)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ApplyFailed, err.Error())

		return ctrl.Result{}, err
	}

	// Step 7: roll up status.
	dep.Status.Deployed = deployed

	dep.Status.DeployedComponentVersion = comp.Status.Component.Version
	if deployRes.Status.Resource.Digest != nil {
		dep.Status.DeployedResourceDigest = deployRes.Status.Resource.Digest.Value
	}

	dep.Status.AppliedLocalizations = localizeResult.AppliedLocalizations
	if dep.Status.LastApplyTime == nil {
		dep.Status.LastApplyTime = &metav1.Time{Time: time.Now()}
	}

	// Check engine-object health; requeue if not yet healthy.
	healthy, notReadyMsg, err := r.checkEngineHealth(ctx, dep, deployed)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResourceIsNotAvailable, err.Error())

		return ctrl.Result{}, err
	}

	if !healthy {
		// Check Argo no-status timeout.
		if argoTimedOut(dep, time.Now()) {
			status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResourceIsNotAvailable,
				"Argo Application has not reported status after timeout. Ensure Argo CD has "+
					"'application.namespaces: \"*\"' in argocd-cmd-params-cm and the AppProject "+
					"has 'sourceNamespaces: [\"*\"]'.")

			return ctrl.Result{}, nil
		}

		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.ResourceIsNotAvailable, notReadyMsg)

		return ctrl.Result{RequeueAfter: dep.GetRequeueAfter()}, nil
	}

	status.MarkReady(r.EventRecorder, dep, "OCMDeployment reconciled successfully")

	return status.RequeueResult(dep, dep.GetRequeueAfter()), nil
}

// reconcileDelete prunes or orphans the applyset members according to the deletion policy.
func (r *Reconciler) reconcileDelete(ctx context.Context, dep *v1alpha1.OCMDeployment) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("reconciling deletion of ocmdeployment")

	err := r.pruneOnDelete(ctx, dep)
	if err != nil {
		status.MarkNotReady(r.EventRecorder, dep, v1alpha1.DeletionFailedReason, err.Error())

		return ctrl.Result{}, err
	}

	if updated := controllerutil.RemoveFinalizer(dep, v1alpha1.OCMDeploymentFinalizer); updated {
		err := r.Update(ctx, dep)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}

			return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// argoTimedOut reports whether an Argo Application has exceeded the no-status timeout.
// Extracted as a pure function so it can be unit-tested without an envtest Argo CD install.
func argoTimedOut(dep *v1alpha1.OCMDeployment, now time.Time) bool {
	return dep.Spec.Deployer == v1alpha1.DeployerEngineArgoCD &&
		dep.Status.LastApplyTime != nil &&
		now.Sub(dep.Status.LastApplyTime.Time) > argoNoStatusTimeout
}
