package deployment

// deploy_health.go checks the health of deployed engine objects (Flux / Argo).

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"ocm.software/open-component-model/bindings/go/kubernetes/controller/api/v1alpha1"
)

// checkEngineHealth reads the applied engine object from the cluster and checks its health.
func (r *Reconciler) checkEngineHealth(ctx context.Context, dep *v1alpha1.OCMDeployment, deployed []v1alpha1.DeployedObjectReference) (healthy bool, notReadyMsg string, err error) {
	logger := log.FromContext(ctx)

	for _, ref := range deployed {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(ref.APIVersion)
		u.SetKind(ref.Kind)

		key := clientKey(ref.Namespace, ref.Name)
		if ref.Namespace == "" && ref.Kind != "Namespace" {
			key.Namespace = dep.Namespace
		}

		err := r.Get(ctx, key, u)
		if err != nil {
			return false, fmt.Sprintf("failed to get %s/%s: %v", ref.Kind, ref.Name, err), nil
		}

		switch ref.Kind {
		case "HelmRelease", "Kustomization", "OCIRepository", "HelmRepository":
			if ready, msg := fluxReadyCondition(u); !ready {
				logger.Info("flux object not ready", "kind", ref.Kind, "name", ref.Name, "reason", msg)

				return false, msg, nil
			}
		case "Application":
			if healthy, msg := argoHealthy(u); !healthy {
				logger.Info("argo application not healthy", "name", ref.Name, "reason", msg)

				return false, msg, nil
			}
		case "Namespace":
			// A Namespace has no readiness condition; it is healthy once it exists (Get above
			// fails-closed on missing) unless it is currently terminating.
			phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
			if phase == "Terminating" {
				return false, fmt.Sprintf("namespace %s is terminating", ref.Name), nil
			}
		}
	}

	return true, "", nil
}

// fluxReadyCondition returns (ready, message) from a Flux object's status.conditions.
func fluxReadyCondition(u *unstructured.Unstructured) (bool, string) {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}

		if cm["type"] == "Ready" {
			if cm["status"] == "True" {
				return true, ""
			}

			msg, _ := cm["message"].(string)
			reason, _ := cm["reason"].(string)

			return false, fmt.Sprintf("%s: %s", reason, msg)
		}
	}
	// No Ready condition yet.
	return false, "no Ready condition found"
}

// argoHealthy returns (healthy, message) from an Argo Application's status.
// It requires health=Healthy, sync=Synced, and no sync operation currently running.
// The running-operation guard prevents a race where Argo marks Healthy after a first
// sync but immediately starts a second self-heal sync that tears down the workload.
func argoHealthy(u *unstructured.Unstructured) (bool, string) {
	health, _, _ := unstructured.NestedString(u.Object, "status", "health", "status")
	sync, _, _ := unstructured.NestedString(u.Object, "status", "sync", "status")

	if health == "" && sync == "" {
		return false, "Argo Application has no status yet"
	}

	if health != "Healthy" || sync != "Synced" {
		return false, fmt.Sprintf("health=%s sync=%s", health, sync)
	}

	// Guard against an in-progress self-heal sync that may transiently delete workloads.
	opPhase, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "phase")
	if opPhase == "Running" {
		return false, fmt.Sprintf("sync operation still running (health=%s sync=%s)", health, sync)
	}

	return true, ""
}
