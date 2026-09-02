package shared

import (
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	"github.com/ray-project/kuberay/ray-operator/pkg/features"
)

// Recognized scheduling.k8s.io preemption-policy values. These mirror the string
// values of the per-version PreemptionPolicy enum (identical across the served API
// versions); callers convert a non-empty result to their version's type.
const (
	preemptLowerPriority = "PreemptLowerPriority"
	preemptNever         = "Never"
)

// ResolvePreemptionPolicy returns the preemption policy the RayCluster requests via the
// ray.io/kubernetes-was-preemption-policy annotation, or "" (leave the field unset) when the
// KubernetesWASPodGroupPreemptionPolicy gate is off, the annotation is absent, or its value is not a
// recognized policy. The field is immutable on the live objects, so it only takes effect at create.
func ResolvePreemptionPolicy(rayCluster *rayv1.RayCluster) string {
	if !features.Enabled(features.KubernetesWASPodGroupPreemptionPolicy) {
		return ""
	}
	switch value := rayCluster.GetAnnotations()[utils.RayKubernetesWASPreemptionPolicyAnnotationKey]; value {
	case preemptLowerPriority, preemptNever:
		return value
	default:
		return ""
	}
}
