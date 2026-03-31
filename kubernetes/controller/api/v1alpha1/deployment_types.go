package v1alpha1

import (
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const KindOCMDeployment = "OCMDeployment"

// OCMDeploymentSpec defines the desired state of OCMDeployment.
type OCMDeploymentSpec struct {
	// Suspend tells the controller to suspend reconciliation of this
	// OCMDeployment.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Resources is a list of Kubernetes resources to be created and managed.
	// Each resource has a unique ID and a template containing the resource
	// manifest. Templates may contain ${} CEL expressions for OCM resolution
	// and cross-resource references.
	// +required
	Resources []OCMDeploymentResource `json:"resources"`
}

// OCMDeploymentResource defines a single Kubernetes resource to be created
// as part of the OCMDeployment.
type OCMDeploymentResource struct {
	// ID is a unique identifier for this resource within the OCMDeployment.
	// Other resources can reference this resource's fields in their CEL
	// expressions using this ID (e.g., ${myId.metadata.name}).
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9]*$`
	// +required
	ID string `json:"id"`

	// Template is the Kubernetes resource manifest to create.
	// It must contain apiVersion and kind fields at minimum.
	// String values within the template may contain ${} CEL expressions
	// that the controller evaluates before creating the resource.
	//
	// Supported expression types:
	//   - Cross-resource references: ${otherId.metadata.name}, ${otherId.status.field}
	//   - OCM resolution (fluent API): ${repoByRef("oci://...").componentByReference("path").resourceByIdentity({"name": "res"}).toOCI().repository}
	//   - String interpolation: "prefix-${expr}-suffix"
	//
	// +required
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:XPreserveUnknownFields
	Template apiextensionsv1.JSON `json:"template"`
}

// OCMDeploymentResourceStatus tracks the state of a single managed resource.
type OCMDeploymentResourceStatus struct {
	// ID matches the resource ID from the spec.
	// +required
	ID string `json:"id"`

	// ObjectReference points to the created Kubernetes object.
	// +optional
	ObjectReference *DeployedObjectReference `json:"objectReference,omitempty"`

	// Ready indicates whether the managed resource has reached a ready state.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Error contains the last error message if the resource failed to
	// reconcile.
	// +optional
	Error string `json:"error,omitempty"`
}

// OCMDeploymentStatus defines the observed state of OCMDeployment.
type OCMDeploymentStatus struct {
	// ObservedGeneration is the last observed generation of the OCMDeployment
	// object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the OCMDeployment.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Resources tracks the status of each managed resource.
	// +optional
	Resources []OCMDeploymentResourceStatus `json:"resources,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,description="Indicates if the OCMDeployment is Ready",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Displays the Age of the OCMDeployment"

// OCMDeployment is the Schema for the ocmdeployments API.
// An OCMDeployment manages a set of Kubernetes resources defined as templates
// with CEL expression interpolation. Resources can reference each other
// by ID and use OCM-specific CEL functions to resolve component references,
// resource identities, and OCI image locations.
type OCMDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec OCMDeploymentSpec `json:"spec"`

	// +optional
	Status OCMDeploymentStatus `json:"status,omitempty"`
}

func (in *OCMDeployment) GetConditions() []metav1.Condition {
	return in.Status.Conditions
}

func (in *OCMDeployment) SetConditions(conditions []metav1.Condition) {
	in.Status.Conditions = conditions
}

func (in *OCMDeployment) GetVID() map[string]string {
	vid := fmt.Sprintf("%s:%s", in.GetNamespace(), in.GetName())
	metadata := make(map[string]string)
	metadata[GroupVersion.Group+"/ocmdeployment"] = vid

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
