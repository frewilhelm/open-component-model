package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const KindDiscovery = "Discovery"

// DiscoverySpec defines the desired state of Resource.
type DiscoverySpec struct {
	// ComponentRef is a reference to a Component.
	// +required
	ComponentRef corev1.LocalObjectReference `json:"componentRef"`

	ReferenceFilter string `json:"referenceFilter,omitempty"`

	ResourceFilter string `json:"resourceFilter,omitempty"`

	// OCMConfig defines references to secrets, config maps or ocm api
	// objects providing configuration data including credentials.
	// +optional
	OCMConfig []OCMConfiguration `json:"ocmConfig,omitempty"`

	// Suspend tells the controller to suspend the reconciliation of this
	// Resource.
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

	// +kubebuilder:validation:Type=object
	// +kubebuilder:validation:XPreserveUnknownFields
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
