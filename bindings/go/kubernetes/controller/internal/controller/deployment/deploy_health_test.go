package deployment

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
	"ocm.software/open-component-model/bindings/go/kubernetes/controller/internal/ocm"
)

func unstructuredWithStatus(kind string, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "x/v1",
		"kind":       kind,
		"status":     status,
	}}
}

func conditionsStatus(conds ...map[string]any) map[string]any {
	cs := make([]any, len(conds))
	for i, c := range conds {
		cs[i] = c
	}
	return map[string]any{"conditions": cs}
}

func condition(typ, status string, extras ...string) map[string]any {
	m := map[string]any{"type": typ, "status": status}
	if len(extras) >= 1 {
		m["reason"] = extras[0]
	}
	if len(extras) >= 2 {
		m["message"] = extras[1]
	}
	return m
}

// TestFluxReadyCondition covers fluxReadyCondition for all relevant states.
func TestFluxReadyCondition(t *testing.T) {
	tests := []struct {
		name        string
		obj         *unstructured.Unstructured
		wantHealthy bool
		wantMsgSub  string // substring that must appear in the message when not healthy
	}{
		{
			name:        "Ready=True is healthy",
			obj:         unstructuredWithStatus("HelmRelease", conditionsStatus(condition("Ready", "True"))),
			wantHealthy: true,
		},
		{
			name: "Ready=False returns reason:message",
			obj: unstructuredWithStatus("HelmRelease", conditionsStatus(
				condition("Ready", "False", "InstallFailed", "chart not found"),
			)),
			wantHealthy: false,
			wantMsgSub:  "InstallFailed",
		},
		{
			name:        "no Ready condition returns false",
			obj:         unstructuredWithStatus("OCIRepository", conditionsStatus(condition("Reconciling", "True"))),
			wantHealthy: false,
			wantMsgSub:  "no Ready condition",
		},
		{
			name:        "empty status returns false",
			obj:         unstructuredWithStatus("OCIRepository", map[string]any{}),
			wantHealthy: false,
			wantMsgSub:  "no Ready condition",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			healthy, msg := fluxReadyCondition(tc.obj)
			r.Equal(tc.wantHealthy, healthy)
			if !tc.wantHealthy && tc.wantMsgSub != "" {
				r.Contains(msg, tc.wantMsgSub)
			}
			if tc.wantHealthy {
				r.Empty(msg)
			}
		})
	}
}

// TestArgoHealthy covers argoHealthy for all relevant states.
func TestArgoHealthy(t *testing.T) {
	tests := []struct {
		name        string
		obj         *unstructured.Unstructured
		wantHealthy bool
		wantMsgSub  string
	}{
		{
			name: "Healthy+Synced is healthy",
			obj: unstructuredWithStatus("Application", map[string]any{
				"health": map[string]any{"status": "Healthy"},
				"sync":   map[string]any{"status": "Synced"},
			}),
			wantHealthy: true,
		},
		{
			name: "Degraded is not healthy",
			obj: unstructuredWithStatus("Application", map[string]any{
				"health": map[string]any{"status": "Degraded"},
				"sync":   map[string]any{"status": "Synced"},
			}),
			wantHealthy: false,
			wantMsgSub:  "Degraded",
		},
		{
			name: "Healthy but not Synced is not healthy",
			obj: unstructuredWithStatus("Application", map[string]any{
				"health": map[string]any{"status": "Healthy"},
				"sync":   map[string]any{"status": "OutOfSync"},
			}),
			wantHealthy: false,
			wantMsgSub:  "OutOfSync",
		},
		{
			name: "Progressing is not healthy",
			obj: unstructuredWithStatus("Application", map[string]any{
				"health": map[string]any{"status": "Progressing"},
				"sync":   map[string]any{"status": "Synced"},
			}),
			wantHealthy: false,
			wantMsgSub:  "Progressing",
		},
		{
			name: "Healthy+Synced but operation Running is not healthy",
			obj: unstructuredWithStatus("Application", map[string]any{
				"health":         map[string]any{"status": "Healthy"},
				"sync":           map[string]any{"status": "Synced"},
				"operationState": map[string]any{"phase": "Running"},
			}),
			wantHealthy: false,
			wantMsgSub:  "still running",
		},
		{
			name: "Healthy+Synced with completed operation is healthy",
			obj: unstructuredWithStatus("Application", map[string]any{
				"health":         map[string]any{"status": "Healthy"},
				"sync":           map[string]any{"status": "Synced"},
				"operationState": map[string]any{"phase": "Succeeded"},
			}),
			wantHealthy: true,
		},
		{
			name:        "no status is not healthy",
			obj:         unstructuredWithStatus("Application", map[string]any{}),
			wantHealthy: false,
			wantMsgSub:  "no status",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			healthy, msg := argoHealthy(tc.obj)
			r.Equal(tc.wantHealthy, healthy)
			if !tc.wantHealthy && tc.wantMsgSub != "" {
				r.Contains(msg, tc.wantMsgSub)
			}
			if tc.wantHealthy {
				r.Empty(msg)
			}
		})
	}
}

// TestCheckEngineHealthDispatch verifies that checkEngineHealth routes OCIRepository
// objects through fluxReadyCondition (the same path as HelmRelease/Kustomization).
func TestCheckEngineHealthDispatch(t *testing.T) {
	r := require.New(t)
	ctx := t.Context()

	scheme := runtime.NewScheme()
	r.NoError(v1alpha1.AddToScheme(scheme))

	// Build an OCIRepository unstructured object with Ready=True.
	ociRepo := &unstructured.Unstructured{}
	ociRepo.SetAPIVersion("source.toolkit.fluxcd.io/v1")
	ociRepo.SetKind("OCIRepository")
	ociRepo.SetName("test-oci")
	ociRepo.SetNamespace("default")
	_ = unstructured.SetNestedSlice(ociRepo.Object, []any{
		map[string]any{"type": "Ready", "status": "True"},
	}, "status", "conditions")

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ociRepo).Build()

	rec := &Reconciler{
		BaseReconciler: &ocm.BaseReconciler{Client: fc, Scheme: scheme},
	}

	dep := &v1alpha1.OCMDeployment{ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: "default"}}

	deployed := []v1alpha1.DeployedObjectReference{
		{APIVersion: "source.toolkit.fluxcd.io/v1", Kind: "OCIRepository", Name: "test-oci", Namespace: "default"},
	}

	healthy, msg, err := rec.checkEngineHealth(ctx, dep, deployed)
	r.NoError(err)
	r.True(healthy, "OCIRepository Ready=True should be healthy")
	r.Empty(msg)

	// Now flip Ready=False and confirm it propagates.
	ociRepoNotReady := ociRepo.DeepCopy()
	_ = unstructured.SetNestedSlice(ociRepoNotReady.Object, []any{
		map[string]any{"type": "Ready", "status": "False", "reason": "FetchFailed", "message": "connection refused"},
	}, "status", "conditions")
	r.NoError(fc.Update(ctx, ociRepoNotReady))

	healthy, msg, err = rec.checkEngineHealth(ctx, dep, deployed)
	r.NoError(err)
	r.False(healthy)
	r.Contains(msg, "FetchFailed")
}
