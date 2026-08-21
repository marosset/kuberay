// Package shared holds the version-independent Kubernetes workload-aware
// scheduling (WAS) reconcile logic shared by the relaxed-immutability providers
// (scheduling.k8s.io/v1beta1 and /v1alpha3). The frozen v1alpha2 provider is
// intentionally standalone and does not use this package.
//
// The Scheduler drives create-or-patch reconciliation of a single whole-cluster
// Workload + PodGroup, delegating the type-specific construction and gang minCount
// access to a per-version APIVersionAdapter.
package shared

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	schedulerinterface "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/interface"
	batchschedulerutils "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/utils"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
)

const (
	// PodGroupProtectionFinalizer is the finalizer Kubernetes adds to protect a
	// PodGroup while Pods still reference it. It is removed before deleting an
	// owned PodGroup because replacement or cleanup may occur before those Pods
	// terminate.
	PodGroupProtectionFinalizer = "scheduling.k8s.io/podgroup-protection"

	// ClusterPodGroupTemplateName is the name of the single PodGroupTemplate that
	// gang schedules the entire RayCluster (head + all worker groups) together.
	ClusterPodGroupTemplateName = "cluster"

	skipReasonGangSchedulingDisabled = "gang scheduling not enabled on RayCluster"
)

// APIVersionAdapter supplies the per-version type wiring the shared Scheduler
// needs for one scheduling.k8s.io API version. Its methods take and return
// client.Object; each implementation asserts the concrete Workload/PodGroup type
// internally (safe: it is paired with that version's NewWorkload/NewPodGroup).
type APIVersionAdapter interface {
	// NewWorkload / NewPodGroup return empty objects to receive Get results into.
	NewWorkload() client.Object
	NewPodGroup() client.Object

	// BuildWorkload / BuildPodGroup construct the desired whole-cluster resources
	// gang scheduling head + all desired workers at the given minCount.
	BuildWorkload(rayCluster *rayv1.RayCluster, minCount int32) client.Object
	BuildPodGroup(rayCluster *rayv1.RayCluster, minCount int32) client.Object

	// WorkloadGangMinCount / PodGroupGangMinCount read the current gang minCount;
	// ok is false when the object does not carry the expected single-gang shape.
	WorkloadGangMinCount(workload client.Object) (minCount int32, ok bool)
	PodGroupGangMinCount(podGroup client.Object) (minCount int32, ok bool)

	// SetWorkloadGangMinCount / SetPodGroupGangMinCount mutate the gang minCount
	// in place for a patch; they no-op when the gang shape is absent.
	SetWorkloadGangMinCount(workload client.Object, minCount int32)
	SetPodGroupGangMinCount(podGroup client.Object, minCount int32)
}

// Scheduler is the WAS batch scheduler for a relaxed-immutability API version. It
// satisfies schedulerinterface.BatchScheduler, delegating the type-specific wiring
// to a per-version APIVersionAdapter.
type Scheduler struct {
	cli     client.Client
	adapter APIVersionAdapter
	name    string
}

var _ schedulerinterface.BatchScheduler = &Scheduler{}

// NewScheduler builds a Scheduler with the given versioned plugin name, client,
// and per-version adapter.
func NewScheduler(name string, cli client.Client, adapter APIVersionAdapter) *Scheduler {
	return &Scheduler{cli: cli, adapter: adapter, name: name}
}

func (s *Scheduler) Name() string { return s.name }

func (s *Scheduler) DoBatchSchedulingOnSubmission(ctx context.Context, object metav1.Object) error {
	rayCluster, ok := object.(*rayv1.RayCluster)
	if !ok {
		return nil
	}
	if reason := SchedulingSkipReason(rayCluster); reason != "" {
		ctrl.LoggerFrom(ctx).WithName(s.name).Info("Skipping Kubernetes workload-aware scheduling", "reason", reason)
		_, err := s.CleanupOnCompletion(ctx, rayCluster)
		return err
	}
	minCount := WorkloadMinCount(rayCluster)
	if err := s.syncWorkload(ctx, rayCluster, minCount); err != nil {
		return err
	}
	return s.syncPodGroup(ctx, rayCluster, minCount)
}

func (s *Scheduler) AddMetadataToChildResource(_ context.Context, parent metav1.Object, child metav1.Object, _ string) {
	rayCluster, ok := parent.(*rayv1.RayCluster)
	if !ok || SchedulingSkipReason(rayCluster) != "" {
		return
	}
	batchschedulerutils.AddSchedulerNameToObject(child, corev1.DefaultSchedulerName)
	// The entire RayCluster (head + every worker group) is gang scheduled as a
	// single PodGroup, so all pods reference the same PodGroup regardless of group.
	SetSchedulingGroup(child, ClusterPodGroupName(rayCluster.Name))
}

func (s *Scheduler) CleanupOnCompletion(ctx context.Context, object metav1.Object) (bool, error) {
	rayCluster, ok := object.(*rayv1.RayCluster)
	if !ok {
		return false, nil
	}
	return s.deleteResources(ctx, rayCluster)
}

// syncWorkload creates the Workload when missing, otherwise patches its gang
// minCount in place. The relaxed versions allow updating the template's minCount,
// so there is no delete-and-recreate for the founding gang subset (the builder
// only ever varies minCount; template add/remove/policy-flip cannot occur).
func (s *Scheduler) syncWorkload(ctx context.Context, rayCluster *rayv1.RayCluster, minCount int32) error {
	desired := s.adapter.BuildWorkload(rayCluster, minCount)
	if err := ctrl.SetControllerReference(rayCluster, desired, s.cli.Scheme()); err != nil {
		return err
	}
	existing := s.adapter.NewWorkload()
	found, err := s.getResource(ctx, "Workload", client.ObjectKeyFromObject(desired), existing)
	if err != nil {
		return err
	}
	if !found {
		if err := s.cli.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create Workload %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		return nil
	}
	// A same-named Workload we do not own is a name collision; fail loudly rather
	// than fight its real owner every reconcile.
	if !metav1.IsControlledBy(existing, rayCluster) {
		return fmt.Errorf("Workload %s/%s already exists and is not owned by this RayCluster; rename it or use a different RayCluster name to avoid the collision", existing.GetNamespace(), existing.GetName())
	}
	if existing.GetDeletionTimestamp() != nil {
		return fmt.Errorf("Workload %s/%s is being deleted, will retry", existing.GetNamespace(), existing.GetName())
	}
	if current, ok := s.adapter.WorkloadGangMinCount(existing); ok && current == minCount {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopyObject().(client.Object))
	s.adapter.SetWorkloadGangMinCount(existing, minCount)
	if err := s.cli.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("failed to patch Workload %s/%s minCount: %w", existing.GetNamespace(), existing.GetName(), err)
	}
	return nil
}

// syncPodGroup creates the runtime PodGroup when missing, otherwise patches its
// gang minCount in place (mutable on the relaxed versions).
func (s *Scheduler) syncPodGroup(ctx context.Context, rayCluster *rayv1.RayCluster, minCount int32) error {
	desired := s.adapter.BuildPodGroup(rayCluster, minCount)
	if err := ctrl.SetControllerReference(rayCluster, desired, s.cli.Scheme()); err != nil {
		return err
	}
	existing := s.adapter.NewPodGroup()
	found, err := s.getResource(ctx, "PodGroup", client.ObjectKeyFromObject(desired), existing)
	if err != nil {
		return err
	}
	if !found {
		if err := s.cli.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create PodGroup %s/%s: %w", desired.GetNamespace(), desired.GetName(), err)
		}
		return nil
	}
	if !metav1.IsControlledBy(existing, rayCluster) {
		return fmt.Errorf("PodGroup %s/%s already exists and is not owned by this RayCluster; rename it or use a different RayCluster name to avoid the collision", existing.GetNamespace(), existing.GetName())
	}
	if existing.GetDeletionTimestamp() != nil {
		return fmt.Errorf("PodGroup %s/%s is being deleted, will retry", existing.GetNamespace(), existing.GetName())
	}
	if current, ok := s.adapter.PodGroupGangMinCount(existing); ok && current == minCount {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopyObject().(client.Object))
	s.adapter.SetPodGroupGangMinCount(existing, minCount)
	if err := s.cli.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("failed to patch PodGroup %s/%s minCount: %w", existing.GetNamespace(), existing.GetName(), err)
	}
	return nil
}

// deleteResources removes the owned PodGroup first (releasing its protection
// finalizer), then the Workload it references, one reconcile at a time.
func (s *Scheduler) deleteResources(ctx context.Context, rayCluster *rayv1.RayCluster) (bool, error) {
	podGroup := s.adapter.NewPodGroup()
	podGroupKey := client.ObjectKey{Name: ClusterPodGroupName(rayCluster.Name), Namespace: rayCluster.Namespace}
	podGroupFound, err := s.getResource(ctx, "PodGroup", podGroupKey, podGroup)
	if err != nil {
		return false, err
	}
	// Only act on resources we own; a same-named foreign object is ignored.
	podGroupExists := podGroupFound && metav1.IsControlledBy(podGroup, rayCluster)

	workload := s.adapter.NewWorkload()
	workloadKey := client.ObjectKey{Name: rayCluster.Name, Namespace: rayCluster.Namespace}
	workloadFound, err := s.getResource(ctx, "Workload", workloadKey, workload)
	if err != nil {
		return false, err
	}
	workloadExists := workloadFound && metav1.IsControlledBy(workload, rayCluster)

	didDelete := false
	if podGroupExists {
		didDelete, err = s.deletePodGroup(ctx, podGroup)
		if err != nil {
			return didDelete, err
		}
		return didDelete, fmt.Errorf("waiting for PodGroup %s/%s to finish deleting", podGroupKey.Namespace, podGroupKey.Name)
	}

	if !workloadExists {
		return didDelete, nil
	}
	if workload.GetDeletionTimestamp() != nil {
		return didDelete, fmt.Errorf("Workload %s/%s is being deleted, will retry", workload.GetNamespace(), workload.GetName())
	}
	if err := s.deleteWithUIDPrecondition(ctx, workload); err != nil {
		if !apierrors.IsNotFound(err) {
			return didDelete, fmt.Errorf("failed to delete Workload %s/%s: %w", workload.GetNamespace(), workload.GetName(), err)
		}
	} else {
		didDelete = true
	}
	return didDelete, nil
}

func (s *Scheduler) deletePodGroup(ctx context.Context, podGroup client.Object) (bool, error) {
	didDelete := controllerutil.RemoveFinalizer(podGroup, PodGroupProtectionFinalizer)
	if didDelete {
		if err := s.cli.Update(ctx, podGroup); err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, fmt.Errorf("failed to remove finalizer from PodGroup %s/%s: %w", podGroup.GetNamespace(), podGroup.GetName(), err)
		}
	}
	if podGroup.GetDeletionTimestamp() != nil {
		return didDelete, nil
	}
	if err := s.deleteWithUIDPrecondition(ctx, podGroup); err != nil {
		if apierrors.IsNotFound(err) {
			return didDelete, nil
		}
		return didDelete, fmt.Errorf("failed to delete PodGroup %s/%s: %w", podGroup.GetNamespace(), podGroup.GetName(), err)
	}
	return true, nil
}

func (s *Scheduler) getResource(ctx context.Context, kind string, key client.ObjectKey, object client.Object) (bool, error) {
	if err := s.cli.Get(ctx, key, object); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to get %s %s: %w", kind, key, err)
	}
	return true, nil
}

func (s *Scheduler) deleteWithUIDPrecondition(ctx context.Context, object client.Object) error {
	uid := object.GetUID()
	return s.cli.Delete(ctx, object, client.Preconditions{UID: &uid})
}

// SchedulingSkipReason returns a non-empty reason when workload-aware scheduling
// should be skipped for the RayCluster.
func SchedulingSkipReason(rayCluster *rayv1.RayCluster) string {
	// Gang scheduling is opt-in per RayCluster via the gang-scheduling label.
	if !strings.EqualFold(rayCluster.GetLabels()[utils.RayGangSchedulingEnabled], "true") {
		return skipReasonGangSchedulingDisabled
	}
	return ""
}

// WorkloadMinCount is the gang floor for the whole cluster. Without autoscaling it
// is the full desired size (1 head + all desired workers). With Ray autoscaling it
// is the reduced floor (1 head + minReplicas) so the autoscaler can add pods above
// the floor without deadlocking the gang; the relaxed versions patch minCount in
// place as minReplicas changes.
func WorkloadMinCount(rayCluster *rayv1.RayCluster) int32 {
	if utils.IsAutoscalingEnabled(&rayCluster.Spec) {
		return int32(1) + utils.CalculateMinReplicas(rayCluster)
	}
	return int32(1) + utils.CalculateDesiredReplicas(rayCluster)
}

// ClusterPodGroupName is the name of the whole-cluster PodGroup for a RayCluster.
func ClusterPodGroupName(clusterName string) string {
	return clusterName + "-" + ClusterPodGroupTemplateName
}

// SetSchedulingGroup points a Pod or PodTemplateSpec at the given PodGroup.
func SetSchedulingGroup(obj metav1.Object, podGroupName string) {
	switch obj := obj.(type) {
	case *corev1.Pod:
		obj.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: &podGroupName}
	case *corev1.PodTemplateSpec:
		obj.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: &podGroupName}
	}
}
