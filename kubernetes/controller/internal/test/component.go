package test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"ocm.software/open-component-model/bindings/go/descriptor/normalisation/json/v4alpha1"
	"ocm.software/open-component-model/bindings/go/descriptor/normalisation"
	descruntime "ocm.software/open-component-model/bindings/go/descriptor/runtime"
	gpgcredentialsv1alpha1 "ocm.software/open-component-model/bindings/go/gpg/spec/credentials/v1alpha1"
	gpgsigningv1alpha1 "ocm.software/open-component-model/bindings/go/gpg/spec/signing/v1alpha1"
	"ocm.software/open-component-model/bindings/go/plugin/manager"
	signingv1alpha1 "ocm.software/open-component-model/bindings/go/rsa/signing/v1alpha1"
	rsacredentialsv1 "ocm.software/open-component-model/bindings/go/rsa/spec/credentials/v1"
	"ocm.software/open-component-model/bindings/go/runtime"
	"ocm.software/open-component-model/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/kubernetes/controller/internal/status"
)

type MockComponentOptions struct {
	Client             client.Client
	Recorder           record.EventRecorder
	Info               v1alpha1.ComponentInfo
	Repository         string
	Verify             []v1alpha1.Verification
	EffectiveOCMConfig []v1alpha1.OCMConfiguration
}

func MockComponent(
	ctx context.Context,
	name, namespace string,
	options *MockComponentOptions,
) *v1alpha1.Component {
	GinkgoHelper()

	component := &v1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1alpha1.ComponentSpec{
			RepositoryRef: corev1.LocalObjectReference{
				Name: options.Repository,
			},
			Component: options.Info.Component,
			Verify:    options.Verify,
		},
	}
	Expect(options.Client.Create(ctx, component)).To(Succeed())

	old := component.DeepCopy()

	component.Status.Component = options.Info
	component.Status.EffectiveOCMConfig = options.EffectiveOCMConfig

	status.MarkReady(options.Recorder, component, "applied mock component")
	component.SetObservedGeneration(component.GetGeneration())
	Expect(options.Client.Status().Patch(ctx, component, client.MergeFrom(old))).To(Succeed())

	Eventually(func(ctx context.Context) error {
		c := &v1alpha1.Component{}
		Expect(options.Client.Get(ctx, client.ObjectKeyFromObject(component), c)).To(Succeed())

		if apimeta.IsStatusConditionTrue(c.GetConditions(), v1alpha1.ReadyCondition) {
			return nil
		}

		return errors.New("component is not ready")
	}, "15s").WithContext(ctx).Should(Succeed())

	return component
}

// SignedComponent is the result of [SignComponent]: the descriptor with three signatures
// appended (one per verifier configuration supported by the controller) and the matching
// verification material for each path. Tests pick which SignatureName / PublicKey / Verifier
// combination to embed in a Component CR (and, for advisory verification, in the ocm
// signing config).
type SignedComponent struct {
	Descriptor *descruntime.Descriptor
	RSA        VerificationMaterial // AlgorithmRSASSAPSS + Plain encoding
	PEM        VerificationMaterial // AlgorithmRSASSAPSS + PEM encoding (cert chain embedded)
	GPG        VerificationMaterial // OpenPGP detached signature
}

// VerificationMaterial pairs a signature name with the trust material a verifier needs.
// PublicKey is what Component.spec.verify[].Value carries (base64-encoded by the caller).
// Verifier is the runtime.Typed configuration that selects this verifier when routed
// through signing.config.ocm.software.
type VerificationMaterial struct {
	SignatureName string
	PublicKey     string
	Verifier      runtime.Typed
}

// SignComponent signs the descriptor three ways under names {baseName}-rsa,
// {baseName}-pem, {baseName}-gpg. The descriptor is mutated in place: the three
// signatures are appended to its Signatures slice. Fresh key material is generated on
// every call.
//
// The three verifier configurations are chosen to exercise every dispatch path in the
// controller's signature verification: plain RSA, PEM-encoded RSA with an embedded
// self-signed certificate chain, and OpenPGP. Tests that only exercise one path may
// ignore the other two materials on the returned SignedComponent.
func SignComponent(ctx context.Context, baseName string, desc *descruntime.Descriptor, pm *manager.PluginManager) SignedComponent {
	GinkgoHelper()

	normalised, err := normalisation.Normalise(desc, v4alpha1.Algorithm)
	Expect(err).ToNot(HaveOccurred())

	digest := hashSHA512(normalised)

	rsaKey, cert := freshRSAKeyAndSelfSignedCert()
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))
	privPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))

	rsaPlainCfg := &signingv1alpha1.Config{
		Type:                    runtime.NewVersionedType(signingv1alpha1.ConfigType, signingv1alpha1.Version),
		SignatureAlgorithm:      signingv1alpha1.AlgorithmRSASSAPSS,
		SignatureEncodingPolicy: signingv1alpha1.SignatureEncodingPolicyPlain,
	}
	rsaPEMCfg := &signingv1alpha1.Config{
		Type:                    runtime.NewVersionedType(signingv1alpha1.ConfigType, signingv1alpha1.Version),
		SignatureAlgorithm:      signingv1alpha1.AlgorithmRSASSAPSS,
		SignatureEncodingPolicy: signingv1alpha1.SignatureEncodingPolicyPEM,
	}
	rsaCreds := &rsacredentialsv1.RSACredentials{
		Type:          rsacredentialsv1.VersionedType,
		PublicKeyPEM:  certPEM,
		PrivateKeyPEM: privPEM,
	}

	rsaSig := signOnce(ctx, pm, rsaPlainCfg, digest, rsaCreds)
	pemSig := signOnce(ctx, pm, rsaPEMCfg, digest, rsaCreds)

	entity := freshGPGEntity()
	gpgCfg := &gpgsigningv1alpha1.Config{
		Type:          runtime.NewVersionedType(gpgsigningv1alpha1.ConfigType, gpgsigningv1alpha1.Version),
		HashAlgorithm: gpgsigningv1alpha1.HashAlgorithmSHA512,
	}
	gpgPriv := armorEntity(openpgp.PrivateKeyType, func(w io.Writer) error {
		return entity.SerializePrivateWithoutSigning(w, nil)
	})
	gpgPub := armorEntity(openpgp.PublicKeyType, entity.Serialize)
	gpgSig := signOnce(ctx, pm, gpgCfg, digest, &gpgcredentialsv1alpha1.GPGCredentials{
		Type:          runtime.NewVersionedType(gpgcredentialsv1alpha1.GPGCredentialsType, gpgcredentialsv1alpha1.Version),
		PrivateKeyPGP: gpgPriv,
	})

	rsaName := baseName + "-rsa"
	pemName := baseName + "-pem"
	gpgName := baseName + "-gpg"

	desc.Signatures = append(desc.Signatures,
		descruntime.Signature{Name: rsaName, Digest: digest, Signature: rsaSig},
		descruntime.Signature{Name: pemName, Digest: digest, Signature: pemSig},
		descruntime.Signature{Name: gpgName, Digest: digest, Signature: gpgSig},
	)

	return SignedComponent{
		Descriptor: desc,
		RSA:        VerificationMaterial{SignatureName: rsaName, PublicKey: certPEM, Verifier: rsaPlainCfg},
		PEM:        VerificationMaterial{SignatureName: pemName, PublicKey: certPEM, Verifier: rsaPEMCfg},
		GPG:        VerificationMaterial{SignatureName: gpgName, PublicKey: gpgPub, Verifier: gpgCfg},
	}
}

func hashSHA512(b []byte) descruntime.Digest {
	GinkgoHelper()
	h := crypto.SHA512.New()
	_, err := h.Write(b)
	Expect(err).ToNot(HaveOccurred())
	return descruntime.Digest{
		HashAlgorithm:          crypto.SHA512.String(),
		NormalisationAlgorithm: v4alpha1.Algorithm,
		Value:                  hex.EncodeToString(h.Sum(nil)),
	}
}

func freshRSAKeyAndSelfSignedCert() (*rsa.PrivateKey, *x509.Certificate) {
	GinkgoHelper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	Expect(err).ToNot(HaveOccurred())

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	Expect(err).ToNot(HaveOccurred())
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "signer"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	Expect(err).ToNot(HaveOccurred())
	cert, err := x509.ParseCertificate(der)
	Expect(err).ToNot(HaveOccurred())
	return k, cert
}

func freshGPGEntity() *openpgp.Entity {
	GinkgoHelper()
	entity, err := openpgp.NewEntity("test", "", "test@example.com", &packet.Config{RSABits: 2048})
	Expect(err).ToNot(HaveOccurred())
	return entity
}

// armorEntity ASCII-armors an openpgp entity using the provided serializer.
func armorEntity(blockType string, serialize func(w io.Writer) error) string {
	GinkgoHelper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, blockType, nil)
	Expect(err).ToNot(HaveOccurred())
	Expect(serialize(w)).To(Succeed())
	Expect(w.Close()).To(Succeed())
	return buf.String()
}

func signOnce(ctx context.Context, pm *manager.PluginManager, cfg runtime.Typed, digest descruntime.Digest, creds runtime.Typed) descruntime.SignatureInfo {
	GinkgoHelper()
	handler, err := pm.SigningRegistry.GetPlugin(ctx, cfg)
	Expect(err).ToNot(HaveOccurred())
	sig, err := handler.Sign(ctx, digest, cfg, creds)
	Expect(err).ToNot(HaveOccurred())
	return sig
}
