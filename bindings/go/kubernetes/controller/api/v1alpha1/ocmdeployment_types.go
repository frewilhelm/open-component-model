package v1alpha1

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const KindOCMDeployment = "OCMDeployment"

// DeployerEngine selects the GitOps engine used to apply the deploy resource.
// +kubebuilder:validation:Enum=FluxCD;ArgoCD
type DeployerEngine string

const (
	// DeployerEngineFluxCD uses Flux HelmRelease / Kustomization as the engine.
	DeployerEngineFluxCD DeployerEngine = "FluxCD"
	// DeployerEngineArgoCD uses an Argo CD Application as the engine.
	DeployerEngineArgoCD DeployerEngine = "ArgoCD"
)

// ComponentSource describes how to resolve the component version.
type ComponentSource struct {
	// RepositorySpec is the raw OCM repository specification used to create a child Repository CR.
	// +required
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:XPreserveUnknownFields
	RepositorySpec *apiextensionsv1.JSON `json:"repositorySpec"`

	// Component is the OCM component name.
	// +required
	Component string `json:"component"`

	// Semver is the version constraint used to resolve the component version.
	// +required
	Semver string `json:"semver"`

	// SemverFilter is an optional additional regex filter applied after semver resolution.
	// +optional
	SemverFilter string `json:"semverFilter,omitempty"`

	// DowngradePolicy controls whether the controller may switch to a lower component version
	// when the semver constraint changes.
	// +kubebuilder:validation:Enum=Allow;Deny
	// +kubebuilder:default=Deny
	// +optional
	DowngradePolicy DowngradePolicy `json:"downgradePolicy,omitempty"`

	// Interval is how often the controller checks for a new component version.
	// +required
	Interval metav1.Duration `json:"interval"`
}

// NamespaceOptions controls the target namespace for the deployed workload.
type NamespaceOptions struct {
	// TargetNamespace is the namespace into which the workload is deployed.
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// CreateNamespace, when true, creates the target namespace if it does not exist.
	// +kubebuilder:default=true
	// +optional
	CreateNamespace *bool `json:"createNamespace,omitempty"`

	// PruneNamespace, when true, deletes the target namespace when the OCMDeployment is deleted
	// (only when deletionPolicy is not Orphan).
	// +kubebuilder:default=false
	// +optional
	PruneNamespace bool `json:"pruneNamespace,omitempty"`
}

// OCMResourceRef identifies a single OCM resource via a ResourceReference and an optional verification policy.
type OCMResourceRef struct {
	// ByReference identifies the OCM resource by component + resource identity path.
	// +required
	ByReference ResourceReference `json:"byReference"`

	// VerificationPolicy controls when resource digest verification is performed.
	// +kubebuilder:validation:Enum=Always;Never
	// +kubebuilder:default=Always
	// +optional
	VerificationPolicy VerificationPolicy `json:"verificationPolicy,omitempty"`
}

// FluxOptions holds Flux-specific rendering options.
type FluxOptions struct {
	// ServiceAccountName is the service account that Flux uses when applying the deploy resource.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// SourceSecretRef is an optional reference to a secret with credentials for the deploy source.
	// For an OCI source it is stamped onto the OCIRepository's secretRef; for an HTTP Helm-repository
	// source (a Helm/v1 access) it is stamped onto the rendered HelmRepository's secretRef.
	// +optional
	SourceSecretRef *corev1.LocalObjectReference `json:"sourceSecretRef,omitempty"`
}

// ArgoOptions holds Argo CD–specific rendering options.
type ArgoOptions struct {
	// Project is the Argo CD AppProject that the Application is placed in.
	// +optional
	Project string `json:"project,omitempty"`

	// RepoCredentialsSecretRef is an optional reference to a secret holding credentials for an HTTP
	// Helm-repository source (a Helm/v1 deploy resource). Argo CD resolves repository credentials from
	// a Secret in its own namespace; this names that Secret. Ignored for OCI sources.
	// +optional
	RepoCredentialsSecretRef *corev1.LocalObjectReference `json:"repoCredentialsSecretRef,omitempty"`
}

// OCMDeploymentSpec defines the desired state of OCMDeployment.
//
// +kubebuilder:validation:XValidation:rule="self.deployer != 'FluxCD' || !has(self.argo)",message="argo options must not be set when deployer is FluxCD"
// +kubebuilder:validation:XValidation:rule="self.deployer != 'ArgoCD' || !has(self.flux)",message="flux options must not be set when deployer is ArgoCD"
// +kubebuilder:validation:XValidation:rule="self.deployer == oldSelf.deployer",message="deployer is immutable"
type OCMDeploymentSpec struct {
	// Source describes the OCM component to deploy from.
	// +required
	Source ComponentSource `json:"source"`

	// DeployResource identifies the OCM resource (Helm chart or kustomization) to deploy.
	// +required
	DeployResource OCMResourceRef `json:"deployResource"`

	// Deployer selects the GitOps engine.
	// +required
	// +kubebuilder:validation:Enum=FluxCD;ArgoCD
	Deployer DeployerEngine `json:"deployer"`

	// Namespace controls the target namespace for the deployed workload.
	// +optional
	Namespace *NamespaceOptions `json:"namespace,omitempty"`

	// ReleaseName is the Helm release name (Flux Helm only).
	// +optional
	ReleaseName string `json:"releaseName,omitempty"`

	// ApplyServiceAccountName is the service account the controller impersonates when applying
	// the rendered engine objects. If empty, the default SA of the OCMDeployment's namespace is used.
	// +optional
	ApplyServiceAccountName string `json:"applyServiceAccountName,omitempty"`

	// Flux holds Flux-specific rendering options. Must only be set when deployer is FluxCD.
	// +optional
	Flux *FluxOptions `json:"flux,omitempty"`

	// Argo holds Argo CD–specific rendering options. Must only be set when deployer is ArgoCD.
	// +optional
	Argo *ArgoOptions `json:"argo,omitempty"`

	// DeletionPolicy controls how the deployed engine objects are removed when the OCMDeployment
	// is deleted. Background and Foreground prune the applyset members; Orphan strips the applyset
	// label and owner reference from them without deleting.
	// +kubebuilder:validation:Enum=Background;Foreground;Orphan
	// +kubebuilder:default=Background
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// ConfigResources lists additional OCM resources that contain YAML configuration to merge
	// into the deploy resource's values (Helm) or pass through as the engine's patches list (kustomize).
	// +optional
	ConfigResources []OCMResourceRef `json:"configResources,omitempty"`

	// Config is inline YAML/JSON configuration merged last into the deploy resource's values
	// (Helm, highest precedence) or appended after ConfigResources as patches (kustomize).
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:XPreserveUnknownFields
	// +optional
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// OCMConfig defines references to secrets, config maps, or OCM API objects providing
	// OCM configuration (credentials, plug-ins, etc.).
	// +optional
	OCMConfig []OCMConfiguration `json:"ocmConfig,omitempty"`

	// Interval is how often the controller reconciles this OCMDeployment.
	// +required
	Interval metav1.Duration `json:"interval"`

	// Suspend, when true, pauses reconciliation.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// AppliedLocalization records a single image reference substitution performed during localization.
type AppliedLocalization struct {
	// MatchKey is the original image reference (registry/repository:tag) that was matched.
	// +required
	MatchKey string `json:"matchKey"`

	// Applied is the relocated image reference that was substituted in.
	// +required
	Applied string `json:"applied"`

	// DigestPinned is true when the substituted reference includes the content-addressed digest
	// from the signed localization label.
	// +required
	DigestPinned bool `json:"digestPinned"`
}

// OCMDeploymentStatus defines the observed state of OCMDeployment.
type OCMDeploymentStatus struct {
	// ObservedGeneration is the last observed generation.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the OCMDeployment.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// EffectiveOCMConfig is the full resolved OCM configuration that was applied during the last
	// reconciliation.
	// +optional
	EffectiveOCMConfig []OCMConfiguration `json:"effectiveOCMConfig,omitempty"`

	// Deployed lists the engine objects (OCIRepository, HelmRelease, Kustomization, Application,
	// Namespace) that were last applied by the controller.
	// +optional
	Deployed []DeployedObjectReference `json:"deployed,omitempty"`

	// DeployedComponentVersion is the component version that was last successfully deployed.
	// +optional
	DeployedComponentVersion string `json:"deployedComponentVersion,omitempty"`

	// DeployedResourceDigest is the content digest of the deploy resource that was last applied.
	// Used to gate re-localization to avoid churn every reconcile.
	// +optional
	DeployedResourceDigest string `json:"deployedResourceDigest,omitempty"`

	// AppliedLocalizations records every image reference substitution performed during the last
	// localization pass.
	// +optional
	AppliedLocalizations []AppliedLocalization `json:"appliedLocalizations,omitempty"`

	// LastApplyTime is the time at which the engine objects were last applied. Used by the Argo
	// no-status timeout.
	// +optional
	LastApplyTime *metav1.Time `json:"lastApplyTime,omitempty"`
}

// OCMDeployment is the Schema for the ocmdeployments API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,description="Indicates if the OCMDeployment is Ready",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Displays the Age of the OCMDeployment"
type OCMDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OCMDeploymentSpec   `json:"spec,omitempty"`
	Status OCMDeploymentStatus `json:"status,omitempty"`
}

func (in *OCMDeployment) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

func (in *OCMDeployment) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

func (in *OCMDeployment) GetVID() map[string]string {
	vid := fmt.Sprintf("%s:%s", in.Namespace, in.Name)
	metadata := make(map[string]string)
	metadata[GroupVersion.Group+"/ocmdeployment_version"] = vid

	return metadata
}

func (in *OCMDeployment) SetObservedGeneration(v int64) {
	in.Status.ObservedGeneration = v
}

func (in *OCMDeployment) GetObjectMeta() *metav1.ObjectMeta {
	return &in.ObjectMeta
}

func (in *OCMDeployment) GetKind() string {
	return KindOCMDeployment
}

func (in *OCMDeployment) GetSpecifiedOCMConfig() []OCMConfiguration {
	return in.Spec.OCMConfig
}

func (in *OCMDeployment) GetEffectiveOCMConfig() []OCMConfiguration {
	return in.Status.EffectiveOCMConfig
}

// GetRequeueAfter returns the duration after which the OCMDeployment must be reconciled again.
func (in *OCMDeployment) GetRequeueAfter() time.Duration {
	return in.Spec.Interval.Duration
}

// +kubebuilder:object:root=true

// OCMDeploymentList contains a list of OCMDeployment.
type OCMDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OCMDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OCMDeployment{}, &OCMDeploymentList{})
}
