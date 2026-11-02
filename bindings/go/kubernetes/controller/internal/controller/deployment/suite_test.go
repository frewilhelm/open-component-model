package deployment

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/envtest/komega"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/resolution"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/test"
	ocicredentials "ocm.software/open-component-model/bindings/go/oci/credentials"
	"ocm.software/open-component-model/bindings/go/oci/repository/provider"
	v1 "ocm.software/open-component-model/bindings/go/oci/spec/identity/v1"
	"ocm.software/open-component-model/bindings/go/oci/spec/repository"
	"ocm.software/open-component-model/bindings/go/plugin/manager"
	"ocm.software/open-component-model/bindings/go/rsa/signing/handler"
	signingv1alpha1 "ocm.software/open-component-model/bindings/go/rsa/signing/v1alpha1"
	ocmruntime "ocm.software/open-component-model/bindings/go/runtime"
)

// +kubebuilder:scaffold:imports

var (
	k8sClient  client.Client
	k8sManager ctrl.Manager
	testEnv    *envtest.Environment
	recorder   record.EventRecorder
	ctx        context.Context
	cancel     context.CancelFunc
	pm         *manager.PluginManager
)

func TestOCMDeploymentController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "OCMDeployment Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	testEnv = test.NewEnvTest()

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(v1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	komega.SetClient(k8sClient)

	gracefulTimeout := 5 * time.Second
	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme.Scheme,
		GracefulShutdownTimeout: &gracefulTimeout,
		Metrics: metricserver.Options{
			BindAddress: "0",
		},
	})
	Expect(err).ToNot(HaveOccurred())

	ctx, cancel = context.WithCancel(GinkgoT().Context())

	events := make(chan string, 64)
	recorder = &record.FakeRecorder{
		Events:        events,
		IncludeObject: true,
	}
	go func() {
		for {
			select {
			case event := <-events:
				GinkgoLogr.Info("Event received", "event", event)
			case <-ctx.Done():
				return
			}
		}
	}()

	pm = manager.NewPluginManager(ctx)
	repositoryProvider := provider.NewComponentVersionRepositoryProvider(provider.WithScheme(repository.Scheme))
	Expect(pm.ComponentVersionRepositoryRegistry.RegisterInternalComponentVersionRepositoryPlugin(repositoryProvider)).To(Succeed())
	signingHandler, err := handler.New(signingv1alpha1.Scheme, true)
	Expect(err).ToNot(HaveOccurred())
	Expect(pm.SigningRegistry.RegisterInternalComponentSignatureHandler(signingHandler)).To(Succeed())
	Expect(pm.CredentialRepositoryRegistry.RegisterInternalCredentialRepositoryPlugin(
		&ocicredentials.OCICredentialRepository{},
		[]ocmruntime.Type{v1.Type},
	)).To(Succeed())

	resolutionLogger := logf.Log.WithName("resolution")
	resolver := resolution.NewResolver(&resolutionLogger)

	Expect((&Reconciler{
		BaseReconciler: &ocm.BaseReconciler{
			Client:           k8sManager.GetClient(),
			Scheme:           testEnv.Scheme,
			EventRecorder:    recorder,
			NewPluginManager: test.StaticPluginManager(pm),
		},
		Resolver: resolver,
	}).SetupWithManager(ctx, k8sManager, 1)).To(Succeed())

	mgrDone := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(mgrDone)
		Expect(k8sManager.Start(ctx)).To(Or(Succeed(), MatchError(ContainSubstring("grace period"))))
	}()

	DeferCleanup(func() {
		cancel()
		<-mgrDone
		Expect(testEnv.Stop()).To(Succeed())
	})
})
