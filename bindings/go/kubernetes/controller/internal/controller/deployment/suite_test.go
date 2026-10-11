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
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/test"
)

var (
	k8sClient client.Client
	testEnv   *envtest.Environment
	recorder  record.EventRecorder
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestControllers(t *testing.T) {
	t.Parallel()
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
	komega.SetClient(k8sClient)

	gracefulTimeout := 5 * time.Second
	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme.Scheme,
		GracefulShutdownTimeout: &gracefulTimeout,
		Metrics:                 metricserver.Options{BindAddress: "0"},
	})
	Expect(err).ToNot(HaveOccurred())

	ctx, cancel = context.WithCancel(GinkgoT().Context())
	recorder = &record.FakeRecorder{Events: make(chan string, 100), IncludeObject: true}

	// This suite starts only the OCMDeployment reconciler. The child
	// Repository/Component/Resource controllers are separate, so the specs drive
	// child readiness by patching their status directly.
	Expect((&Reconciler{
		BaseReconciler: &ocm.BaseReconciler{
			Client:        k8sManager.GetClient(),
			Scheme:        k8sManager.GetScheme(),
			EventRecorder: recorder,
		},
	}).SetupWithManager(ctx, k8sManager)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(k8sManager.Start(ctx)).To(Or(Succeed(), MatchError(ContainSubstring("grace period"))))
	}()
})

var _ = AfterSuite(func() {
	if cancel != nil {
		cancel()
	}
	By("tearing down the test environment")
	Expect(testEnv.Stop()).To(Succeed())
})
