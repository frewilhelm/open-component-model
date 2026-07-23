package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const KindDiscovery = "Discovery"

// Extract configures how discovered data is projected into status.discovery.
// Exactly one of ByResources, ByComponents, or Expression must be set.
//
// The simple modes (ByResources, ByComponents) take a flat map from output
// field name to CEL expression. Paths are prefixed with the iteration binding
// (`resource.*`, `component.*`), mirroring the resource controller. Missing
// field accesses evaluate to null and the key is omitted from that entry.
//
// Expression is a CEL expression whose return value is stored verbatim as
// status.discovery. Use it when ByResources / ByComponents cannot express the
// desired projection (filtering inside the projection, custom shapes, joining
// data across the graph, etc.).
//
// CEL bindings by mode:
//   ByResources:  resource   the current resource (v2 wire format)
//                 component  the enclosing component descriptor
//   ByComponents: component  the current component descriptor
//   Expression:   components list of surviving component descriptors
//
// Switching modes changes which names are in scope; a `component` reference
// in an Expression body will not compile because Expression binds `components`
// (plural).
//
// Missing-attribute behaviour differs by evaluator, and by mode within Extract:
//   Selector.Expression:      missing attr/label => predicate is false; element
//                             does not match. Guard with has(...) for strictness.
//   Extract.ByResources /     missing attr => field is null and dropped from the
//   Extract.ByComponents:     entry; other fields in the entry still emit.
//   Extract.Expression:       missing attr => ExtractFailed; the whole payload
//                             has no per-field fallback. Guard with has(...) or
//                             CEL optional access (foo.?bar.orValue(...)).
// +kubebuilder:validation:XValidation:rule="[has(self.byResources) && size(self.byResources) > 0, has(self.byComponents) && size(self.byComponents) > 0, has(self.expression) && size(self.expression) > 0].filter(x, x).size() == 1",message="exactly one of byResources, byComponents, expression must be set and non-empty"
type Extract struct {
	// ByResources produces one output entry per resource of each surviving component.
	// Each map value is a CEL expression evaluated per resource. Bindings:
	//   resource   the current resource (v2 wire format)
	//   component  the enclosing component descriptor (v2 wire format)
	// +optional
	ByResources map[string]string `json:"byResources,omitempty"`

	// ByComponents produces one output entry per surviving component.
	// Each map value is a CEL expression evaluated per component. Binding:
	//   component  the current component descriptor (v2 wire format)
	// +optional
	ByComponents map[string]string `json:"byComponents,omitempty"`

	// Expression is a CEL expression whose return value is stored verbatim as
	// status.discovery. Binding:
	//   components list of surviving component descriptors (v2 wire format, in sort order)
	// +optional
	Expression string `json:"expression,omitempty"`
}

// DiscoverySpec defines the desired state of Discovery.
type DiscoverySpec struct {
	// ComponentRef is a reference to a Component object.
	// +required
	ComponentRef corev1.LocalObjectReference `json:"componentRef"`

	// ReferenceSelector filters the discovered component graph by matching
	// references. After traversal completes, the result contains
	// every descriptor that is the target of at least one reference (anywhere
	// in the graph) matching this selector.
	//
	// An empty selector (no MatchIdentity, no MatchLabels, no Expression) is
	// treated the same as omitting the field: this filter stage is skipped
	// entirely and the root remains in the graph. The root is only dropped
	// when this selector carries at least one predicate, because the root has
	// no incoming reference and can never match. Once referenceSelector has
	// narrowed the descriptor set, componentSelector and resourceSelector run
	// on that reduced set; they cannot re-admit descriptors this stage
	// dropped, including the root. If you want the root back, drop
	// referenceSelector.
	//
	// Traversal semantics:
	//   - By default, the full reachable component graph is traversed and this
	//     selector is applied as a post-filter. This avoids the "deep match
	//     unreachable if any ancestor edge does not match" trap of per-edge
	//     pruning.
	//   - Optimization: if this selector uniquely identifies a target (both
	//     "componentName" and "version" are set to concrete values in
	//     matchIdentity, with no other constraints), traversal short-circuits
	//     the moment that target vertex is resolved. Because component identity
	//     {name, version} is globally unique per OCM spec, at most one vertex
	//     can match. The target's own subtree is not explored.
	//
	// Matchable surface for each reference:
	//   identity keys:
	//     "name"          = local reference name (unique within a parent's references[])
	//     "componentName" = target component name (matches the wire-format JSON key)
	//     "version"       = target component version
	//     <other key>     = reference extraIdentity attribute
	//   labels: the reference's own labels (accessible via `labels.<name>` in Expression)
	//
	// Note: the target component name ("componentName") is derived from the
	// reference's canonical `componentName` field and always overlays any
	// user-supplied `extraIdentity["componentName"]` at match time.
	// +optional
	ReferenceSelector *Selector `json:"referenceSelector,omitempty"`

	// ComponentSelector filters discovered component descriptors by their own
	// identity and labels. When referenceSelector is unset or empty, it runs
	// on the full descriptor set (including the root); when referenceSelector
	// has predicates, it runs on whatever descriptors that stage kept, so it
	// cannot re-add components (including the root) that referenceSelector
	// already dropped.
	//
	// Traversal semantics mirror referenceSelector:
	//   - By default, the full reachable component graph is traversed and this
	//     selector is applied as a post-filter.
	//   - Optimization: if this selector uniquely identifies a target (both
	//     "name" and "version" are set to concrete values in matchIdentity,
	//     with no other constraints), traversal short-circuits the moment that
	//     target vertex is resolved.
	//
	// Setting this selector to exclude the root removes the root from the
	// output while keeping any matched descendants, which may look like an
	// orphan subtree.
	//
	// Matchable surface for each component descriptor:
	//   identity keys:
	//     "name"    = component name
	//     "version" = component version
	//   labels: the component's labels
	//
	// Note: ComponentMeta has no extraIdentity. matchIdentity or
	// matchExpressions on keys other than "name" and "version" (or "labels" in
	// expressions) will silently match nothing.
	// +optional
	ComponentSelector *Selector `json:"componentSelector,omitempty"`

	// ResourceSelector selects which resources to include per discovered component.
	// Applied in-place on each descriptor that survived referenceSelector and
	// componentSelector. An empty/nil selector includes all resources.
	//
	// Matchable surface for each resource:
	//   identity keys:
	//     "name"      = resource name
	//     "version"   = resource version
	//     <other key> = resource extraIdentity attribute
	//   labels: the resource's own labels
	//
	// Note: resource.type, resource.relation, and access-related fields are not
	// exposed to resource-selector filtering. To filter by them, use
	// `extract.byResources` (where the `resource` binding exposes `resource.type`,
	// `resource.relation`, `resource.access.*`, etc.) and filter inside the
	// projection, or restrict the resulting payload downstream.
	// +optional
	ResourceSelector *Selector `json:"resourceSelector,omitempty"`

	// OCMConfig defines references to secrets, config maps or ocm api
	// objects providing configuration data including credentials.
	// +optional
	OCMConfig []OCMConfiguration `json:"ocmConfig,omitempty"`

	// Extract projects fields from the discovered graph into status.discovery.
	// Exactly one of ByResources, ByComponents, or Expression must be set. When
	// Extract is nil, full component descriptors are returned (see the
	// Discovery field in DiscoveryStatus for the exact shape).
	// +optional
	Extract *Extract `json:"extract,omitempty"`

	// Suspend tells the controller to suspend the reconciliation of this
	// Discovery.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// DiscoveryStatus defines the observed state of Discovery.
type DiscoveryStatus struct {
	// ObservedGeneration is the last observed generation of the Discovery
	// object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the conditions for the Discovery.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// EffectiveOCMConfig lists the config maps and secrets applied during
	// reconciliation, in the order they were applied.
	// +optional
	EffectiveOCMConfig []OCMConfiguration `json:"effectiveOCMConfig,omitempty"`

	// Discovery contains the discovered component versions and their resources.
	// The format depends on the spec configuration:
	// - No selectors, no extract: array of surviving component descriptors
	//   (always an array, even with a single descriptor, so consumers can
	//   iterate uniformly)
	// - With referenceSelector: array of descriptors that are targets of
	//   matching references (root not included; it has no incoming reference)
	// - With componentSelector: array of matched component descriptors; runs
	//   on the referenceSelector output when both are set, so it cannot
	//   re-admit the root once referenceSelector dropped it
	// - With resourceSelector: array of descriptors with resources filtered in-place
	// - With extract.byResources: flat array of per-resource projections
	// - With extract.byComponents: flat array of per-component projections
	// - With extract.expression: the raw return value of the CEL expression
	// +kubebuilder:validation:XPreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +optional
	Discovery *apiextensionsv1.JSON `json:"discovery,omitempty"`
}

// Discovery is the Schema for the resources API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=".spec.componentRef.name",description="Name of the referenced Component"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`,description="Indicates if the Discovery is Ready"
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].message`,description="Human-readable status message"
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

// DiscoveryList contains a list of Discovery.
type DiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Discovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Discovery{}, &DiscoveryList{})
}
