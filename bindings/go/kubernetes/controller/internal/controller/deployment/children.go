package deployment

// children.go creates and manages the child OCM CRs (Repository, Component, Resource)
// owned by an OCMDeployment.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/util"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/pkg/configuration"
)

// ensureRepository creates or updates the child Repository CR.
func (r *Reconciler) ensureRepository(ctx context.Context, dep *v1alpha1.OCMDeployment) (*v1alpha1.Repository, error) {
	logger := log.FromContext(ctx)

	repo := &v1alpha1.Repository{}
	key := client.ObjectKey{Namespace: dep.Namespace, Name: dep.Name}

	err := r.Get(ctx, key, repo)
	if apierrors.IsNotFound(err) {
		repo = &v1alpha1.Repository{
			Name:      dep.Name,
			Namespace: dep.Namespace,
			Spec: v1alpha1.RepositorySpec{
				RepositorySpec: dep.Spec.Source.RepositorySpec,
				OCMConfig:      dep.Spec.OCMConfig,
				Interval:       dep.Spec.Source.Interval,
			},
		}
		err := controllerutil.SetControllerReference(dep, repo, r.Scheme)
		if err != nil {
			return nil, fmt.Errorf("failed to set controller reference on repository: %w", err)
		}

		logger.Info("creating child repository", "name", repo.Name)

		err = r.Create(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("failed to create repository: %w", err)
		}

		return repo, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get repository: %w", err)
	}

	// Check ownership — refuse to adopt an object owned by something else.
	if !isOwnedBy(repo, dep) {
		return nil, fmt.Errorf("repository %s/%s exists but is not owned by this OCMDeployment", repo.Namespace, repo.Name)
	}

	// Propagate any spec changes from the parent (semver, interval, ocmConfig, repositorySpec).
	desiredRepoSpec := v1alpha1.RepositorySpec{
		RepositorySpec: dep.Spec.Source.RepositorySpec,
		OCMConfig:      dep.Spec.OCMConfig,
		Interval:       dep.Spec.Source.Interval,
	}
	if !equality.Semantic.DeepEqual(repo.Spec, desiredRepoSpec) {
		repo.Spec = desiredRepoSpec
		logger.Info("updating child repository", "name", repo.Name)
		if err := r.Update(ctx, repo); err != nil {
			return nil, fmt.Errorf("failed to update repository: %w", err)
		}
	}

	return repo, nil
}

// ensureComponent creates or updates the child Component CR.
func (r *Reconciler) ensureComponent(ctx context.Context, dep *v1alpha1.OCMDeployment, repo *v1alpha1.Repository) (*v1alpha1.Component, error) {
	logger := log.FromContext(ctx)

	comp := &v1alpha1.Component{}
	key := client.ObjectKey{Namespace: dep.Namespace, Name: dep.Name}

	err := r.Get(ctx, key, comp)
	if apierrors.IsNotFound(err) {
		comp = &v1alpha1.Component{
			Name:      dep.Name,
			Namespace: dep.Namespace,
			Spec: v1alpha1.ComponentSpec{
				RepositoryRef:   corev1.LocalObjectReference{Name: repo.Name},
				Component:       dep.Spec.Source.Component,
				Semver:          dep.Spec.Source.Semver,
				SemverFilter:    dep.Spec.Source.SemverFilter,
				DowngradePolicy: dep.Spec.Source.DowngradePolicy,
				OCMConfig:       dep.Spec.OCMConfig,
				Interval:        dep.Spec.Source.Interval,
			},
		}
		err := controllerutil.SetControllerReference(dep, comp, r.Scheme)
		if err != nil {
			return nil, fmt.Errorf("failed to set controller reference on component: %w", err)
		}

		logger.Info("creating child component", "name", comp.Name)

		err = r.Create(ctx, comp)
		if err != nil {
			return nil, fmt.Errorf("failed to create component: %w", err)
		}

		return comp, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get component: %w", err)
	}

	if !isOwnedBy(comp, dep) {
		return nil, fmt.Errorf("component %s/%s exists but is not owned by this OCMDeployment", comp.Namespace, comp.Name)
	}

	// Propagate any spec changes from the parent (component, semver, downgradePolicy, ocmConfig, interval).
	desiredCompSpec := v1alpha1.ComponentSpec{
		RepositoryRef:   corev1.LocalObjectReference{Name: repo.Name},
		Component:       dep.Spec.Source.Component,
		Semver:          dep.Spec.Source.Semver,
		SemverFilter:    dep.Spec.Source.SemverFilter,
		DowngradePolicy: dep.Spec.Source.DowngradePolicy,
		OCMConfig:       dep.Spec.OCMConfig,
		Interval:        dep.Spec.Source.Interval,
	}
	if !equality.Semantic.DeepEqual(comp.Spec, desiredCompSpec) {
		comp.Spec = desiredCompSpec
		logger.Info("updating child component", "name", comp.Name)
		if err := r.Update(ctx, comp); err != nil {
			return nil, fmt.Errorf("failed to update component: %w", err)
		}
	}

	return comp, nil
}

// ensureDeployResource creates or updates the child Resource CR for the deploy artifact.
// Returns nil when the Resource exists but is not yet ready (caller should requeue).
func (r *Reconciler) ensureDeployResource(ctx context.Context, dep *v1alpha1.OCMDeployment, comp *v1alpha1.Component) (*v1alpha1.Resource, error) {
	return r.ensureResourceCR(ctx, dep, comp, dep.Name+"-deploy", dep.Spec.DeployResource)
}

// ensureConfigResource creates or updates a child Resource CR for a config resource entry.
// Returns nil when the Resource exists but is not yet ready.
func (r *Reconciler) ensureConfigResource(ctx context.Context, dep *v1alpha1.OCMDeployment, comp *v1alpha1.Component, idx int, ref v1alpha1.OCMResourceRef) (*v1alpha1.Resource, error) {
	name := fmt.Sprintf("%s-config-%d", dep.Name, idx)

	return r.ensureResourceCR(ctx, dep, comp, name, ref)
}

// ensureResourceCR is the common create-or-update helper for child Resource CRs.
func (r *Reconciler) ensureResourceCR(ctx context.Context, dep *v1alpha1.OCMDeployment, comp *v1alpha1.Component, name string, ref v1alpha1.OCMResourceRef) (*v1alpha1.Resource, error) {
	logger := log.FromContext(ctx)

	res := &v1alpha1.Resource{}
	key := client.ObjectKey{Namespace: dep.Namespace, Name: name}

	err := r.Get(ctx, key, res)
	if apierrors.IsNotFound(err) {
		res = &v1alpha1.Resource{
			Name:      name,
			Namespace: dep.Namespace,
			Spec: v1alpha1.ResourceSpec{
				ComponentRef: corev1.LocalObjectReference{Name: comp.Name},
				Resource: v1alpha1.ResourceID{
					ByReference: ref.ByReference,
				},
				VerificationPolicy: ref.VerificationPolicy,
				OCMConfig:          dep.Spec.OCMConfig,
			},
		}
		err := controllerutil.SetControllerReference(dep, res, r.Scheme)
		if err != nil {
			return nil, fmt.Errorf("failed to set controller reference on resource %s: %w", name, err)
		}

		logger.Info("creating child resource", "name", name)

		err = r.Create(ctx, res)
		if err != nil {
			return nil, fmt.Errorf("failed to create resource %s: %w", name, err)
		}

		return nil, nil // created but not yet ready
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get resource %s: %w", name, err)
	}

	if !isOwnedBy(res, dep) {
		return nil, fmt.Errorf("resource %s/%s exists but is not owned by this OCMDeployment", res.Namespace, res.Name)
	}

	// Propagate any spec changes from the parent (byReference, verificationPolicy, ocmConfig).
	desiredResSpec := v1alpha1.ResourceSpec{
		ComponentRef: corev1.LocalObjectReference{Name: comp.Name},
		Resource: v1alpha1.ResourceID{
			ByReference: ref.ByReference,
		},
		VerificationPolicy: ref.VerificationPolicy,
		OCMConfig:          dep.Spec.OCMConfig,
	}
	if !equality.Semantic.DeepEqual(res.Spec, desiredResSpec) {
		res.Spec = desiredResSpec
		logger.Info("updating child resource", "name", name)
		if err := r.Update(ctx, res); err != nil {
			return nil, fmt.Errorf("failed to update resource %s: %w", name, err)
		}
	}

	// Wait for ready using GetReadyObject.
	ready, err := util.GetReadyObject[v1alpha1.Resource, *v1alpha1.Resource](ctx, r.Client, key)
	if err != nil {
		// NotReadyError or DeletionError → not ready yet, signal to caller.
		return nil, nil //nolint:nilerr // caller checks nil return as "not ready"
	}

	return ready, nil
}

// isOwnedBy reports whether obj has an owner reference to owner.
func isOwnedBy(obj metav1.Object, owner metav1.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() {
			return true
		}
	}

	return false
}

// pruneImageResourceCRs deletes stale index-named image Resource CRs whose index is
// >= desiredCount. This handles the case where a component version change exposes fewer
// image resources than the previous version, leaving orphaned index-named CRs behind.
func (r *Reconciler) pruneImageResourceCRs(ctx context.Context, dep *v1alpha1.OCMDeployment, desiredCount int) error {
	logger := log.FromContext(ctx)

	var list v1alpha1.ResourceList

	err := r.List(ctx, &list, client.InNamespace(dep.Namespace))
	if err != nil {
		return fmt.Errorf("listing image resource CRs: %w", err)
	}

	prefix := dep.Name + "-img-"

	var firstErr error

	for i := range list.Items {
		item := &list.Items[i]
		if !strings.HasPrefix(item.Name, prefix) || !isOwnedBy(item, dep) {
			continue
		}

		idxStr := strings.TrimPrefix(item.Name, prefix)

		idx, err := strconv.Atoi(idxStr)
		if err != nil || idx < desiredCount {
			continue
		}

		logger.Info("pruning stale image resource CR", "name", item.Name, "index", idx, "desiredCount", desiredCount)

		delErr := r.Delete(ctx, item)
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			logger.Error(delErr, "failed to delete stale image resource CR", "name", item.Name)

			if firstErr == nil {
				firstErr = delErr
			}
		}
	}

	return firstErr
}

// getConfiguration loads the effective OCM configuration for the OCMDeployment.
func (r *Reconciler) getConfiguration(ctx context.Context, dep *v1alpha1.OCMDeployment) (*configuration.Configuration, error) {
	configs, err := ocm.GetEffectiveConfig(ctx, r.GetClient(), dep, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get effective ocm config: %w", err)
	}

	dep.Status.EffectiveOCMConfig = configs

	cfg, err := configuration.LoadConfigurations(ctx, r.Client, dep.Namespace, configs)
	if err != nil {
		return nil, fmt.Errorf("failed to load configurations: %w", err)
	}

	return cfg, nil
}

// clientKey returns a client.ObjectKey for the given namespace and name.
func clientKey(namespace, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: name}
}

// getChildComponent fetches the child Component CR (already exists when localize runs).
func (r *Reconciler) getChildComponent(ctx context.Context, dep *v1alpha1.OCMDeployment) (*v1alpha1.Component, error) {
	comp := &v1alpha1.Component{}
	err := r.Get(ctx, clientKey(dep.Namespace, dep.Name), comp)
	if err != nil {
		return nil, nil //nolint:nilerr // not yet created
	}

	return comp, nil
}
