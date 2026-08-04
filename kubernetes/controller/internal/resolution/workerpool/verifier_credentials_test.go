package workerpool

import (
	"testing"

	"github.com/stretchr/testify/require"

	gpgcredentialsv1alpha1 "ocm.software/open-component-model/bindings/go/gpg/spec/credentials/v1alpha1"
	gpgsigningv1alpha1 "ocm.software/open-component-model/bindings/go/gpg/spec/signing/v1alpha1"
	signingv1alpha1 "ocm.software/open-component-model/bindings/go/rsa/signing/v1alpha1"
	rsacredentialsv1 "ocm.software/open-component-model/bindings/go/rsa/spec/credentials/v1"
	"ocm.software/open-component-model/bindings/go/runtime"
	sigstoresigningv1alpha1 "ocm.software/open-component-model/bindings/go/sigstore/signing/v1alpha1"
	trustedrootv1alpha1 "ocm.software/open-component-model/bindings/go/sigstore/spec/credentials/trustedroot/v1alpha1"
)

// TestCredentialsForVerifier covers the routing from a verifier config type to the credential wrapper handed to the
// signing handler. Each verifier config type in the signing registry maps to exactly one credential type; a change to
// this switch is almost always a bug.
func TestCredentialsForVerifier(t *testing.T) {
	tests := []struct {
		name         string
		verifierType runtime.Type
		publicKey    []byte
		assert       func(t *testing.T, cred runtime.Typed)
	}{
		{
			name:         "empty public key returns nil credentials",
			verifierType: runtime.NewVersionedType(signingv1alpha1.ConfigType, signingv1alpha1.Version),
			publicKey:    nil,
			assert: func(t *testing.T, cred runtime.Typed) {
				require.Nil(t, cred)
			},
		},
		{
			name:         "rsa config -> RSA credentials",
			verifierType: runtime.NewVersionedType(signingv1alpha1.ConfigType, signingv1alpha1.Version),
			publicKey:    []byte("-----BEGIN PUBLIC KEY-----\nrsa\n-----END PUBLIC KEY-----"),
			assert: func(t *testing.T, cred runtime.Typed) {
				rsa, ok := cred.(*rsacredentialsv1.RSACredentials)
				require.True(t, ok, "expected RSACredentials, got %T", cred)
				require.Contains(t, rsa.PublicKeyPEM, "BEGIN PUBLIC KEY")
			},
		},
		{
			name:         "gpg config -> GPG credentials",
			verifierType: runtime.NewVersionedType(gpgsigningv1alpha1.ConfigType, gpgsigningv1alpha1.Version),
			publicKey:    []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\ngpg\n-----END PGP PUBLIC KEY BLOCK-----"),
			assert: func(t *testing.T, cred runtime.Typed) {
				gpg, ok := cred.(*gpgcredentialsv1alpha1.GPGCredentials)
				require.True(t, ok, "expected GPGCredentials, got %T", cred)
				require.Contains(t, gpg.PublicKeyPGP, "BEGIN PGP PUBLIC KEY BLOCK")
			},
		},
		{
			name:         "sigstore verify config -> TrustedRoot credentials",
			verifierType: runtime.NewVersionedType(sigstoresigningv1alpha1.VerifyConfigType, sigstoresigningv1alpha1.Version),
			publicKey:    []byte(`{"mediaType":"application/vnd.dev.sigstore.trustedroot+json;version=0.1"}`),
			assert: func(t *testing.T, cred runtime.Typed) {
				root, ok := cred.(*trustedrootv1alpha1.TrustedRoot)
				require.True(t, ok, "expected TrustedRoot, got %T", cred)
				require.Contains(t, root.TrustedRootJSON, "trustedroot")
			},
		},
		{
			name:         "unknown verifier type surfaces an error",
			verifierType: runtime.NewVersionedType("SomethingElse", "v1"),
			publicKey:    []byte("data"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			cred, err := credentialsForVerifier(tc.verifierType, tc.publicKey)
			if tc.assert == nil {
				r.Error(err)
				return
			}
			r.NoError(err)
			tc.assert(t, cred)
		})
	}
}
