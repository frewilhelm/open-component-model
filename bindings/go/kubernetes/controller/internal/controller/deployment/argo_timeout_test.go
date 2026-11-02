package deployment

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
)

// TestArgoTimedOut verifies the argoTimedOut pure helper that gates the Argo no-status timeout.
func TestArgoTimedOut(t *testing.T) {
	r := require.New(t)

	now := time.Now()
	justBefore := now.Add(-(argoNoStatusTimeout - time.Second))
	justAfter := now.Add(-(argoNoStatusTimeout + time.Second))

	argoDepWithTime := func(ts time.Time) *v1alpha1.OCMDeployment {
		return &v1alpha1.OCMDeployment{
			Spec: v1alpha1.OCMDeploymentSpec{
				Deployer: v1alpha1.DeployerEngineArgoCD,
			},
			Status: v1alpha1.OCMDeploymentStatus{
				LastApplyTime: &metav1.Time{Time: ts},
			},
		}
	}

	t.Run("argo within timeout window is not timed out", func(t *testing.T) {
		r := require.New(t)
		r.False(argoTimedOut(argoDepWithTime(justBefore), now))
	})

	t.Run("argo past timeout window is timed out", func(t *testing.T) {
		r := require.New(t)
		r.True(argoTimedOut(argoDepWithTime(justAfter), now))
	})

	t.Run("argo with nil LastApplyTime is never timed out", func(t *testing.T) {
		r := require.New(t)
		dep := &v1alpha1.OCMDeployment{
			Spec: v1alpha1.OCMDeploymentSpec{Deployer: v1alpha1.DeployerEngineArgoCD},
		}
		r.False(argoTimedOut(dep, now))
	})

	t.Run("Flux deployer is never timed out regardless of time", func(t *testing.T) {
		r := require.New(t)
		dep := &v1alpha1.OCMDeployment{
			Spec: v1alpha1.OCMDeploymentSpec{Deployer: v1alpha1.DeployerEngineFluxCD},
			Status: v1alpha1.OCMDeploymentStatus{
				LastApplyTime: &metav1.Time{Time: justAfter},
			},
		}
		r.False(argoTimedOut(dep, now))
	})

	_ = r // top-level r used if needed; subtests use their own
}
