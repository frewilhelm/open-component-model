package oci

import (
	v2 "ocm.software/open-component-model/bindings/go/oci/spec/access/v1"
	"ocm.software/open-component-model/bindings/go/runtime"
)

var Scheme = runtime.NewScheme()

func init() {
	MustAddToScheme(Scheme)
}

func MustAddToScheme(scheme *runtime.Scheme) {
	ociImageLayer := &v2.OCIImageLayer{}
	scheme.MustRegisterWithAlias(ociImageLayer,
		runtime.NewVersionedType(v2.OCIImageLayerType, v2.Version),
		runtime.NewUnversionedType(v2.OCIImageLayerType),
		runtime.NewVersionedType(v2.LegacyOCIBlobAccessType, v2.LegacyOCIBlobAccessTypeVersion),
		runtime.NewUnversionedType(v2.LegacyOCIBlobAccessType),
	)

	ociArtifact := &v2.OCIImage{}
	scheme.MustRegisterWithAlias(ociArtifact,
		runtime.NewVersionedType(v2.OCIImageType, v2.Version),
		runtime.NewUnversionedType(v2.OCIImageType),
		runtime.NewVersionedType(v2.LegacyType, v2.LegacyTypeVersion),
		runtime.NewUnversionedType(v2.LegacyType),
		runtime.NewVersionedType(v2.LegacyType2, v2.LegacyType2Version),
		runtime.NewUnversionedType(v2.LegacyType2),
		runtime.NewVersionedType(v2.LegacyType3, v2.LegacyType3Version),
		runtime.NewUnversionedType(v2.LegacyType3),
	)

	scheme.MustRegisterWithAlias(&v2.RelativeOCIReference{},
		runtime.NewVersionedType(v2.RelativeOCIReferenceType, v2.Version),
		runtime.NewUnversionedType(v2.RelativeOCIReferenceType),
	)
}

// MustAddRelativeOCIReferenceToScheme registers only the relativeOciReference access type
// (both the versioned and unversioned spelling) into scheme. It lets callers compose a
// narrow "local by value" scheme (e.g. localBlob + relativeOciReference) without pulling in
// OCIImage, which must not be classified as local.
func MustAddRelativeOCIReferenceToScheme(scheme *runtime.Scheme) {
	scheme.MustRegisterWithAlias(&v2.RelativeOCIReference{},
		runtime.NewVersionedType(v2.RelativeOCIReferenceType, v2.Version),
		runtime.NewUnversionedType(v2.RelativeOCIReferenceType),
	)
}

// relativeScheme is a narrow scheme registering only the relative OCI reference type. It
// backs IsRelativeOCIReference: probing a Raw/Unstructured access against the broad Scheme
// would false-match any registered type (the Raw->Typed path does a bare json.Unmarshal
// with no type-match check), so classification must use a scheme that registers only the
// relative type.
var relativeScheme = runtime.NewScheme()

func init() {
	MustAddRelativeOCIReferenceToScheme(relativeScheme)
}

// IsRelativeOCIReference reports whether access is a relativeOciReference (either spelling,
// as a concrete type, Raw, or Unstructured).
func IsRelativeOCIReference(access runtime.Typed) bool {
	if access == nil {
		return false
	}
	var rel v2.RelativeOCIReference
	return relativeScheme.Convert(access, &rel) == nil
}

// ociImageScheme is a narrow scheme registering only the OCIImage type and all its legacy
// aliases. It backs IsOCIImageType: probing the broad Scheme is avoided because that would
// match OCIImageLayer and RelativeOCIReference too.
var ociImageScheme = runtime.NewScheme()

func init() {
	ociImage := &v2.OCIImage{}
	ociImageScheme.MustRegisterWithAlias(ociImage,
		runtime.NewVersionedType(v2.OCIImageType, v2.Version),
		runtime.NewUnversionedType(v2.OCIImageType),
		runtime.NewVersionedType(v2.LegacyType, v2.LegacyTypeVersion),
		runtime.NewUnversionedType(v2.LegacyType),
		runtime.NewVersionedType(v2.LegacyType2, v2.LegacyType2Version),
		runtime.NewUnversionedType(v2.LegacyType2),
		runtime.NewVersionedType(v2.LegacyType3, v2.LegacyType3Version),
		runtime.NewUnversionedType(v2.LegacyType3),
	)
}

// IsOCIImageType reports whether typ identifies an OCI image type — the canonical
// "OCIImage" or any of its legacy aliases ("ociArtifact", "ociRegistry", "ociImage"),
// versioned or unversioned.
func IsOCIImageType(typ runtime.Type) bool {
	return ociImageScheme.IsRegistered(typ)
}
