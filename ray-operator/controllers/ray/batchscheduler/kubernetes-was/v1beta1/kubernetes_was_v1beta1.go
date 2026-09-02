// Package v1beta1 implements the Kubernetes workload-aware scheduling provider
// for scheduling.k8s.io/v1beta1. This package supplies the v1beta1 type wiring
// (APIVersionAdapter) and provider glue, plus the gang preemption policy behind
// its feature gate.
package v1beta1

import (
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	schedulerinterface "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/interface"
	kuberneteswas "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/kubernetes-was"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/kubernetes-was/shared"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
)

// schedulerName is this provider's explicit per-version plugin identity, surfaced
// in logs/events/status (WAS pods use corev1.DefaultSchedulerName, not this).
const schedulerName = kuberneteswas.PluginName + "-v1beta1"

// Provider implements kuberneteswas.Provider for scheduling.k8s.io/v1beta1.
type Provider struct{}

func init() {
	kuberneteswas.RegisterProvider(&Provider{})
}

func (p *Provider) GroupVersion() schema.GroupVersion {
	return schedulingv1beta1.SchemeGroupVersion
}

func (p *Provider) Available(config *rest.Config) error {
	return shared.APIVersionAvailable(config, schedulingv1beta1.SchemeGroupVersion)
}

func (p *Provider) AddToScheme(scheme *runtime.Scheme) {
	utilruntime.Must(schedulingv1beta1.AddToScheme(scheme))
}

func (p *Provider) NewScheduler(cli client.Client) schedulerinterface.BatchScheduler {
	return shared.NewScheduler(schedulerName, cli, adapter{})
}

func (p *Provider) ConfigureReconciler(b *builder.Builder) *builder.Builder {
	return b.Owns(&schedulingv1beta1.Workload{}).
		Owns(&schedulingv1beta1.PodGroup{})
}

// adapter supplies the v1beta1 type wiring for shared.Scheduler.
type adapter struct{}

var _ shared.APIVersionAdapter = adapter{}

func (adapter) NewWorkload() client.Object { return &schedulingv1beta1.Workload{} }
func (adapter) NewPodGroup() client.Object { return &schedulingv1beta1.PodGroup{} }

func (adapter) BuildWorkload(rayCluster *rayv1.RayCluster, minCount int32) client.Object {
	template := schedulingv1beta1.PodGroupTemplate{
		Name:             shared.ClusterPodGroupTemplateName,
		SchedulingPolicy: gangPolicy(minCount),
		PreemptionPolicy: resolvePreemptionPolicy(rayCluster),
	}
	return &schedulingv1beta1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rayCluster.Name,
			Namespace: rayCluster.Namespace,
			Labels:    map[string]string{utils.RayClusterLabelKey: rayCluster.Name},
		},
		Spec: schedulingv1beta1.WorkloadSpec{
			// ControllerRef is a back-reference to the owning RayCluster for tooling; it is
			// distinct from the owner reference set via SetControllerReference (used for GC).
			ControllerRef: &schedulingv1beta1.TypedLocalObjectReference{
				APIGroup: rayv1.GroupVersion.Group,
				Kind:     "RayCluster",
				Name:     rayCluster.Name,
			},
			PodGroupTemplates: []schedulingv1beta1.PodGroupTemplate{template},
		},
	}
}

func (adapter) BuildPodGroup(rayCluster *rayv1.RayCluster, minCount int32) client.Object {
	return &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      shared.ClusterPodGroupName(rayCluster.Name),
			Namespace: rayCluster.Namespace,
			Labels:    map[string]string{utils.RayClusterLabelKey: rayCluster.Name},
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			WorkloadRef: &schedulingv1beta1.WorkloadReference{
				WorkloadName: rayCluster.Name,
				TemplateName: shared.ClusterPodGroupTemplateName,
			},
			SchedulingPolicy: gangPolicy(minCount),
			PreemptionPolicy: resolvePreemptionPolicy(rayCluster),
		},
	}
}

func (adapter) WorkloadGangMinCount(obj client.Object) (int32, bool) {
	workload, ok := obj.(*schedulingv1beta1.Workload)
	if !ok || len(workload.Spec.PodGroupTemplates) != 1 {
		return 0, false
	}
	gang := workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang
	if gang == nil {
		return 0, false
	}
	return gang.MinCount, true
}

func (adapter) PodGroupGangMinCount(obj client.Object) (int32, bool) {
	podGroup, ok := obj.(*schedulingv1beta1.PodGroup)
	if !ok || podGroup.Spec.SchedulingPolicy.Gang == nil {
		return 0, false
	}
	return podGroup.Spec.SchedulingPolicy.Gang.MinCount, true
}

func (adapter) SetWorkloadGangMinCount(obj client.Object, minCount int32) {
	workload, ok := obj.(*schedulingv1beta1.Workload)
	if !ok || len(workload.Spec.PodGroupTemplates) != 1 || workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang == nil {
		return
	}
	workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount = minCount
}

func (adapter) SetPodGroupGangMinCount(obj client.Object, minCount int32) {
	podGroup, ok := obj.(*schedulingv1beta1.PodGroup)
	if !ok || podGroup.Spec.SchedulingPolicy.Gang == nil {
		return
	}
	podGroup.Spec.SchedulingPolicy.Gang.MinCount = minCount
}

func gangPolicy(minCount int32) schedulingv1beta1.PodGroupSchedulingPolicy {
	return schedulingv1beta1.PodGroupSchedulingPolicy{
		Gang: &schedulingv1beta1.GangSchedulingPolicy{MinCount: minCount},
	}
}

// resolvePreemptionPolicy converts the shared-resolved preemption policy value to the
// v1beta1 PreemptionPolicy type, or nil when none applies (see shared.ResolvePreemptionPolicy).
func resolvePreemptionPolicy(rayCluster *rayv1.RayCluster) *schedulingv1beta1.PreemptionPolicy {
	if value := shared.ResolvePreemptionPolicy(rayCluster); value != "" {
		policy := schedulingv1beta1.PreemptionPolicy(value)
		return &policy
	}
	return nil
}
