package verification

import (
	"context"
	"encoding/base64"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	genericv1 "ocm.software/open-component-model/bindings/go/configuration/generic/v1/spec"
	"ocm.software/open-component-model/bindings/go/runtime"
	signingspec "ocm.software/open-component-model/bindings/go/signing/v1alpha1/spec"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
)

// Verification is an internal representation of v1alpha1.Verification where the public key is already extracted from
// the value or secret. VerifierSpec carries the signing-config entry that applies to this signature, resolved from the
// ocm signing config (signing.config.ocm.software) at construction time; nil means "fall back to the RSASSA-PSS
// default". Attaching it here keeps per-signature verifier selection encapsulated with the signature it applies to and
// makes the choice part of the resolver cache key without threading extra fields through the workerpool.
type Verification struct {
	Signature    string        `json:"signature"`
	PublicKey    []byte        `json:"publicKey"`
	VerifierSpec *runtime.Raw  `json:"verifierSpec,omitempty"`
}

func GetVerifications(ctx context.Context, client ctrl.Reader, cfg *genericv1.Config,
	obj v1alpha1.VerificationProvider,
) ([]Verification, error) {
	verifications := obj.GetVerifications()

	var secret corev1.Secret
	v := make([]Verification, 0, len(verifications))
	for _, ver := range verifications {
		internal := Verification{
			Signature: ver.Signature,
		}
		signingCfg, err := signingspec.LookupConfigForSignature(cfg, ver.Signature)
		if err != nil {
			return nil, reconcile.TerminalError(fmt.Errorf("failed to look up signing config for signature %q: %w", ver.Signature, err))
		}
		if signingCfg != nil {
			internal.VerifierSpec = signingCfg.Verifier
		}
		if ver.Value == "" && ver.SecretRef.Name == "" {
			// No inline value and no secret reference: leave PublicKey nil. This is legal for
			// verifiers that need no trust material at verification time (public-good Sigstore
			// is the current example). Verifiers that require key material (RSA, GPG) will
			// surface their own "missing public key" error when they run.
			v = append(v, internal)
			continue
		}
		if ver.Value != "" && ver.SecretRef.Name != "" {
			return nil, reconcile.TerminalError(fmt.Errorf("value and secret ref cannot both be set for signature: %s", ver.Signature))
		}
		if ver.Value != "" {
			decoded, err := base64.StdEncoding.DecodeString(ver.Value)
			if err != nil {
				return nil, reconcile.TerminalError(fmt.Errorf("failed to decode public key value for signature %q: %w", ver.Signature, err))
			}
			internal.PublicKey = decoded
		}
		if ver.SecretRef.Name != "" {
			if err := client.Get(ctx, ctrl.ObjectKey{Namespace: obj.GetNamespace(), Name: ver.SecretRef.Name}, &secret); err != nil {
				return nil, err
			}
			certBytes, ok := secret.Data[ver.Signature]
			if !ok {
				return nil, fmt.Errorf("secret %q does not contain key %q for signature verification", ver.SecretRef.Name, ver.Signature)
			}
			internal.PublicKey = certBytes
		}

		v = append(v, internal)
	}

	return v, nil
}
