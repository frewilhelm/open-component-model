// Package v1alpha1 implements the "ocm.software/localization" label.
// Resources carrying this label opt in to image reference localization by the OCMDeployment
// controller. The label value records the original image coordinates so the controller can
// match them against values found in a Helm chart or kustomize manifest and substitute the
// registry-relocated reference.
//
//	resources:
//	  - name: podinfo-image
//	    type: ociImage
//	    labels:
//	      - name: ocm.software/localization
//	        version: v1
//	        value:
//	          type: ociImage
//	          matchKey:
//	            registry: ghcr.io
//	            repository: stefanprodan/podinfo
//	            tag: "6.9.1"
//	            digest: sha256:abc123...
package v1alpha1

import (
	"fmt"

	descriptor "ocm.software/open-component-model/bindings/go/descriptor/runtime"
)

const (
	// LabelName is the name under which localization hints are stored on a resource.
	LabelName = "ocm.software/localization"
	// Version is the specification version of the label value implemented here.
	// Must satisfy the OCM label version pattern ^v[0-9]+$ (schema-2020-12.json).
	Version = "v1"
)

// Value is the decoded value of the localization label.
type Value struct {
	// Type identifies the resource type this localization hint applies to (e.g. "ociImage").
	Type string `json:"type"`
	// MatchKey contains the original image coordinates used to match values in the deploy artifact.
	MatchKey MatchKey `json:"matchKey"`
}

// MatchKey holds the image coordinates of the original (pre-transfer) resource location.
// The controller normalises both the MatchKey and any candidate value to a canonical form
// (default registry docker.io, implicit library/ prefix for single-segment repositories)
// before comparing.
type MatchKey struct {
	// Registry is the image registry host (e.g. "ghcr.io", "docker.io").
	// +optional
	Registry string `json:"registry,omitempty"`
	// Repository is the image repository path (e.g. "stefanprodan/podinfo").
	// +required
	Repository string `json:"repository"`
	// Tag is the image tag (e.g. "6.9.1"). Empty means any tag matches.
	// +optional
	Tag string `json:"tag,omitempty"`
	// Digest is the content-addressed digest of the image manifest (e.g. "sha256:abc...").
	// When present the controller pins the substituted reference with this digest.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// FromResourceLabels decodes the localization label out of the resource's labels.
// Returns (nil, false, nil) when the label is absent (not an error; absence means the resource
// does not participate in localization).
func FromResourceLabels(labels []descriptor.Label) (*Value, bool, error) {
	for _, label := range labels {
		if label.Name != LabelName {
			continue
		}
		if label.Version != "" && label.Version != Version {
			continue
		}
		var v Value
		if err := label.GetValue(&v); err != nil {
			return nil, false, fmt.Errorf("interpreting label %q failed: %w", LabelName, err)
		}
		return &v, true, nil
	}
	return nil, false, nil
}
