package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const KindDiscovery = "Discovery"

// DiscoverySpec defines the desired state of Discovery.
type DiscoverySpec struct {
	// ComponentRef is a reference to a Component object.
	// +required
	ComponentRef corev1.LocalObjectReference `json:"componentRef"`

	// ReferenceSelector filters for component references from the root component object.
	// Only references matching this selector are included in the discovery result.
	// A set selector overwrite recursive = 0 (no reference traversal).
	// +optional
	ReferenceSelector *Selector `json:"referenceSelector,omitempty"`

	// ResourceSelector selects which resources to include per discovered component.
	// Only resources matching this selector appear in the status.
	// An empty/nil selector includes all resources.
	// +optional
	ResourceSelector *Selector `json:"resourceSelector,omitempty"`

	// TODO: Discuss naming — "recursive" mirrors the CLI, "depth" might be clearer for K8s users.
	// Recursive limits how deep the controller traverses component references.
	// 0 = only the root component (no reference traversal).
	// When unset or nil, traversal is unlimited.
	// Values > 0 (specific depth levels) are not yet supported.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=0
	Recursive *int32 `json:"recursive,omitempty"`

	// OCMConfig defines references to secrets, config maps or ocm api
	// objects providing configuration data including credentials.
	// +optional
	OCMConfig []OCMConfiguration `json:"ocmConfig,omitempty"`

	// DiscoveryFields defines additional fields to extract from each discovered resource.
	// Keys are the output field names, values are JSONPath expressions relative to
	// each resource object (e.g. "access.imageReference").
	// When set, status.discovery contains a compact representation with component
	// identity, resource name, and the extracted fields.
	// When empty, the full component descriptors are returned.
	// +optional
	DiscoveryFields map[string]string `json:"discoveryFields,omitempty"`

	// Suspend tells the controller to suspend the reconciliation of this
	// Discovery.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// DiscoveryStatus defines the observed state of Resource.
type DiscoveryStatus struct {
	// ObservedGeneration is the last observed generation of the Resource
	// object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the Resource.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// EffectiveOCMConfig specifies the entirety of config maps and secrets
	// whose configuration data was applied to the Resource reconciliation,
	// in the order the configuration data was applied.
	// +optional
	EffectiveOCMConfig []OCMConfiguration `json:"effectiveOCMConfig,omitempty"`

	// Discovery contains the discovered component versions and their resources.
	// The format depends on the spec configuration:
	// - No selectors: full component descriptor(s)
	// - With referenceSelector: root identity + filtered component descriptors
	// - With resourceSelector: root identity + filtered resources
	// - With discoveryFields: compact output with extracted fields
	// +kubebuilder:validation:XPreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Discovery *apiextensionsv1.JSON `json:"discovery,omitempty"`
}

// Discovery is the Schema for the resources API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,description="Indicates if the Discovery is Ready",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Displays the Age of the Discovery"
type Discovery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DiscoverySpec   `json:"spec"`
	Status DiscoveryStatus `json:"status,omitempty"`
}

func (in *Discovery) GetEffectiveOCMConfig() []OCMConfiguration {
	return in.Status.EffectiveOCMConfig
}

func (in *Discovery) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

func (in *Discovery) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

func (in *Discovery) GetVID() map[string]string {
	vid := fmt.Sprintf("%s:%s", in.GetNamespace(), in.GetName())
	metadata := make(map[string]string)
	metadata[GroupVersion.Group+"/resource_version"] = vid

	return metadata
}

func (in *Discovery) SetObservedGeneration(v int64) {
	in.Status.ObservedGeneration = v
}

func (in *Discovery) GetObjectMeta() *metav1.ObjectMeta {
	return &in.ObjectMeta
}

func (in *Discovery) GetKind() string {
	return KindDiscovery
}

func (in *Discovery) GetSpecifiedOCMConfig() []OCMConfiguration {
	return in.Spec.OCMConfig
}

// +kubebuilder:object:root=true

// DiscoveryList contains a list of Resource.
type DiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Discovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Discovery{}, &DiscoveryList{})
}
