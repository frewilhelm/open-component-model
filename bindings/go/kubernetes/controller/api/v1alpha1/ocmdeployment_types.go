package v1alpha1

import (
	"fmt"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const KindOCMDeployment = "OCMDeployment"

// DeployerEngine selects the deployment engine that applies the rendered objects.
type DeployerEngine string

const (
	DeployerEngineFluxCD DeployerEngine = "FluxCD"
	DeployerEngineArgoCD DeployerEngine = "ArgoCD"
)

// OCMDeploymentSpec defines the desired state of OCMDeployment.
type OCMDeploymentSpec struct {
	// RepositorySpec is the config of an ocm repository containing component
	// versions, as in the Repository CR. The controller creates a Repository
	// from it. An insecure or local registry is expressed here via an http
	// baseUrl, not via a reference scheme.
	// +required
	RepositorySpec *apiextensionsv1.JSON `json:"repositorySpec"`

	// Component is the name of the ocm component; the controller creates a
	// Component from it.
	// +required
	Component string `json:"component"`

	// Semver constrains the fetched component version (exact or range).
	// +required
	Semver string `json:"semver"`

	// SemverFilter is a regex pattern to filter the versions within the Semver
	// range.
	// +optional
	SemverFilter string `json:"semverFilter,omitempty"`

	// DeployResource identifies the OCM resource to deploy (a Helm chart or a
	// kustomize resource). The artifact kind is derived from the OCM resource type.
	// +required
	DeployResource ResourceID `json:"deployResource"`

	// Deployer selects the deployment engine.
	// +kubebuilder:validation:Enum:=FluxCD;ArgoCD
	// +required
	Deployer DeployerEngine `json:"deployer"`

	// TargetNamespace is the namespace the workload is deployed into. Defaults
	// to the OCMDeployment's own namespace when unset.
	// +optional
	TargetNamespace string `json:"targetNamespace,omitempty"`

	// CreateNamespace creates TargetNamespace if it does not exist.
	// +kubebuilder:default:=true
	// +optional
	CreateNamespace *bool `json:"createNamespace,omitempty"`

	// ReleaseName overrides the Helm release name. Defaults to the OCMDeployment
	// name, so two OCMDeployments do not collide unless both override it to the
	// same value in the same TargetNamespace. Ignored for kustomize.
	// +optional
	ReleaseName string `json:"releaseName,omitempty"`

	// ServiceAccountName is the ServiceAccount the controller impersonates to
	// apply the generated deploy objects, so they are created with that
	// ServiceAccount's permissions and not the controller's. The ServiceAccount
	// must exist in the OCMDeployment's own namespace.
	// +required
	ServiceAccountName string `json:"serviceAccountName"`

	// ConfigResources references OCM resources holding yaml used as values/config.
	// +optional
	ConfigResources []ResourceID `json:"configResources,omitempty"`

	// Config is inline, schemaless configuration merged over ConfigResources.
	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:XPreserveUnknownFields
	// +optional
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// OCMConfig defines references to secrets, config maps or ocm api
	// objects providing configuration data including credentials.
	// +optional
	OCMConfig []OCMConfiguration `json:"ocmConfig,omitempty"`

	// Interval at which the OCMDeployment is reconciled.
	// +required
	Interval metav1.Duration `json:"interval"`

	// Suspend tells the controller to suspend the reconciliation of this
	// OCMDeployment.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// OCMDeploymentStatus defines the observed state of OCMDeployment.
type OCMDeploymentStatus struct {
	// ObservedGeneration is the last observed generation of the OCMDeployment
	// object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the OCMDeployment.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// EffectiveOCMConfig specifies the entirety of config maps and secrets
	// whose configuration data was applied to the OCMDeployment reconciliation,
	// in the order the configuration data was applied.
	// +optional
	EffectiveOCMConfig []OCMConfiguration `json:"effectiveOCMConfig,omitempty"`

	// Deployed references the objects the controller created and owns: the child
	// Repository/Component/Resource objects and the generated deploy objects.
	// +optional
	Deployed []DeployedObjectReference `json:"deployed,omitempty"`

	// DeployedComponentVersion is the resolved component version currently live.
	// +optional
	DeployedComponentVersion string `json:"deployedComponentVersion,omitempty"`

	// DeployedResourceDigest is the digest of the deploy resource currently live.
	// +optional
	DeployedResourceDigest string `json:"deployedResourceDigest,omitempty"`

	// AppliedLocalizations records each image localization that was applied.
	// +optional
	AppliedLocalizations []AppliedLocalization `json:"appliedLocalizations,omitempty"`
}

// AppliedLocalization records a single image localization that was applied: the
// original reference (the match key) and the relocated reference substituted in.
type AppliedLocalization struct {
	// Original is the reference the localization matched on.
	// +required
	Original string `json:"original"`
	// Applied is the relocated reference substituted into the deploy objects.
	// +required
	Applied string `json:"applied"`
	// DigestPinned is false when the chart exposed only a tag (no digest field),
	// so the substituted location is not digest-verified. The controller also
	// emits a warning Event for such images.
	// +required
	DigestPinned bool `json:"digestPinned"`
}

// GetConditions returns the conditions of the OCMDeployment.
func (in *OCMDeployment) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

// SetConditions sets the conditions of the OCMDeployment.
func (in *OCMDeployment) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

// GetVID unique identifier of the object.
func (in *OCMDeployment) GetVID() map[string]string {
	vid := fmt.Sprintf("%s:%s", in.GetNamespace(), in.GetName())
	metadata := make(map[string]string)
	metadata[GroupVersion.Group+"/resource_version"] = vid

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

// GetRequeueAfter returns the duration after which the OCMDeployment must be
// reconciled again.
func (in *OCMDeployment) GetRequeueAfter() time.Duration {
	if in == nil {
		return 0
	}
	return in.Spec.Interval.Duration
}

func (in *OCMDeployment) GetSpecifiedOCMConfig() []OCMConfiguration {
	return in.Spec.OCMConfig
}

func (in *OCMDeployment) GetEffectiveOCMConfig() []OCMConfiguration {
	return in.Status.EffectiveOCMConfig
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,description="Indicates if the OCMDeployment is Ready",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Displays the Age of the OCMDeployment"

// OCMDeployment is the Schema for the ocmdeployments API.
type OCMDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OCMDeploymentSpec   `json:"spec,omitempty"`
	Status OCMDeploymentStatus `json:"status,omitempty"`
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
