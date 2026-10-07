package v1alpha3

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
)

// Alpha topology-aware scheduling.
//
// A RayCluster that requests topology (utils.RayWorkerGroupTopologyAnnotation) is mapped to a
// Workload with one root CompositePodGroup and a PodGroup leaf per participating member:
//
//	<raycluster>-cluster          CompositePodGroup, gang over every leaf, no topology
//	  <raycluster>-head           PodGroup, minCount 1, no topology
//	  <raycluster>-wg-<group>     PodGroup per worker group, minCount = gang floor, optional topology key
//
// The layout is fixed at creation. Only each leaf's gang minCount is updated in place; any other
// change requires recreating the RayCluster.
const (
	// maxLeafPodGroups is the upstream limit on child templates of one composite template.
	maxLeafPodGroups         = 8
	headTemplateName         = "head"
	workerTemplateNamePrefix = "wg-"
)

// leaf is one member PodGroup of the composite hierarchy.
type leaf struct {
	templateName string
	// groupName is the worker group name, or utils.RayNodeHeadGroupLabelValue for the head.
	groupName   string
	topologyKey string
	minCount    int32
}

func (l leaf) podGroupName(clusterName string) string {
	return clusterName + "-" + l.templateName
}

// topologyRequest parses the topology annotation. requested is false when it is absent.
func topologyRequest(rayCluster *rayv1.RayCluster) (request map[string]string, requested bool, err error) {
	raw, ok := rayCluster.GetAnnotations()[utils.RayWorkerGroupTopologyAnnotation]
	if !ok {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		return nil, true, fmt.Errorf("annotation %s must be a JSON object mapping worker group names to node label keys: %w", utils.RayWorkerGroupTopologyAnnotation, err)
	}
	if len(request) == 0 {
		return nil, true, fmt.Errorf("annotation %s must name at least one worker group", utils.RayWorkerGroupTopologyAnnotation)
	}
	return request, true, nil
}

// workerGroupGangFloor is the number of pods of a worker group that must be admitted together.
// With the Ray autoscaler it is the group's minReplicas, matching the whole-cluster floor.
func workerGroupGangFloor(rayCluster *rayv1.RayCluster, group rayv1.WorkerGroupSpec) int32 {
	if group.Suspend != nil && *group.Suspend {
		return 0
	}
	if utils.IsAutoscalingEnabled(&rayCluster.Spec) {
		return ptr.Deref(group.MinReplicas, 0) * group.NumOfHosts
	}
	return utils.GetWorkerGroupDesiredReplicas(group)
}

// buildLeaves validates the topology request and returns the head leaf followed by one leaf per
// worker group with a gang floor of at least one. Groups with a floor of zero are not in the gang
// and cannot request topology.
func buildLeaves(rayCluster *rayv1.RayCluster, request map[string]string) ([]leaf, error) {
	leaves := []leaf{{templateName: headTemplateName, groupName: utils.RayNodeHeadGroupLabelValue, minCount: 1}}
	known := map[string]bool{}
	for _, group := range rayCluster.Spec.WorkerGroupSpecs {
		known[group.GroupName] = true
		floor := workerGroupGangFloor(rayCluster, group)
		key := request[group.GroupName]
		if floor == 0 {
			if key != "" {
				return nil, fmt.Errorf("worker group %q requests topology but has a gang floor of zero (suspended, zero replicas, or minReplicas 0); topology is not supported for it", group.GroupName)
			}
			continue
		}
		leaves = append(leaves, leaf{
			templateName: workerTemplateNamePrefix + group.GroupName,
			groupName:    group.GroupName,
			topologyKey:  key,
			minCount:     floor,
		})
	}
	for name, key := range request {
		if !known[name] {
			return nil, fmt.Errorf("annotation %s names unknown worker group %q", utils.RayWorkerGroupTopologyAnnotation, name)
		}
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return nil, fmt.Errorf("worker group %q has invalid topology key %q: %s", name, key, strings.Join(errs, "; "))
		}
	}
	if len(leaves) > maxLeafPodGroups {
		return nil, fmt.Errorf("topology scheduling supports the head plus at most %d worker groups, found %d", maxLeafPodGroups-1, len(leaves)-1)
	}
	for _, l := range leaves {
		if errs := validation.IsDNS1123Subdomain(l.podGroupName(rayCluster.Name)); len(errs) > 0 {
			return nil, fmt.Errorf("derived PodGroup name %q is invalid: %s", l.podGroupName(rayCluster.Name), strings.Join(errs, "; "))
		}
	}
	return leaves, nil
}

// topologyLeaves returns the leaves for a RayCluster that requests topology.
func topologyLeaves(rayCluster *rayv1.RayCluster) ([]leaf, bool, error) {
	request, requested, err := topologyRequest(rayCluster)
	if !requested || err != nil {
		return nil, requested, err
	}
	leaves, err := buildLeaves(rayCluster, request)
	return leaves, true, err
}

// leafPodGroupName returns the PodGroup that pods of groupName join, or "" when the group has no leaf.
func leafPodGroupName(rayCluster *rayv1.RayCluster, leaves []leaf, groupName string) string {
	for _, l := range leaves {
		if l.groupName == groupName {
			return l.podGroupName(rayCluster.Name)
		}
	}
	return ""
}

func leafTemplate(l leaf, priorityClassName string, preemptionPolicy *schedulingv1alpha3.PreemptionPolicy) schedulingv1alpha3.PodGroupTemplate {
	t := schedulingv1alpha3.PodGroupTemplate{
		Name:              l.templateName,
		PriorityClassName: priorityClassName,
		PreemptionPolicy:  preemptionPolicy,
		SchedulingPolicy:  schedulingv1alpha3.PodGroupSchedulingPolicy{Gang: &schedulingv1alpha3.GangSchedulingPolicy{MinCount: l.minCount}},
	}
	if l.topologyKey != "" {
		t.SchedulingConstraints = &schedulingv1alpha3.PodGroupSchedulingConstraints{
			Topology: []schedulingv1alpha3.TopologyConstraint{{Key: l.topologyKey}},
		}
	}
	return t
}

// buildTopologyResources builds the Workload, root CompositePodGroup, and one PodGroup per leaf.
func (k *KubernetesWASV1Alpha3Scheduler) buildTopologyResources(rayCluster *rayv1.RayCluster, leaves []leaf) (*schedulingv1alpha3.Workload, *schedulingv1alpha3.CompositePodGroup, []*schedulingv1alpha3.PodGroup, error) {
	priorityClassName, preemptionPolicy := gangPriority(rayCluster)
	labels := func() map[string]string { return map[string]string{utils.RayClusterLabelKey: rayCluster.Name} }
	rootName := clusterPodGroupName(rayCluster.Name)
	rootPolicy := schedulingv1alpha3.CompositePodGroupSchedulingPolicy{
		Gang: &schedulingv1alpha3.CompositeGangSchedulingPolicy{MinGroupCount: int32(len(leaves))},
	}

	templates := make([]schedulingv1alpha3.PodGroupTemplate, 0, len(leaves))
	podGroups := make([]*schedulingv1alpha3.PodGroup, 0, len(leaves))
	for _, l := range leaves {
		template := leafTemplate(l, priorityClassName, preemptionPolicy)
		templates = append(templates, template)
		podGroups = append(podGroups, &schedulingv1alpha3.PodGroup{
			ObjectMeta: metav1.ObjectMeta{Name: l.podGroupName(rayCluster.Name), Namespace: rayCluster.Namespace, Labels: labels()},
			Spec: schedulingv1alpha3.PodGroupSpec{
				ParentCompositePodGroupName: ptr.To(rootName),
				WorkloadRef:                 &schedulingv1alpha3.WorkloadReference{WorkloadName: rayCluster.Name, TemplateName: l.templateName},
				SchedulingPolicy:            template.SchedulingPolicy,
				SchedulingConstraints:       template.SchedulingConstraints,
				PriorityClassName:           priorityClassName,
				PreemptionPolicy:            preemptionPolicy,
			},
		})
	}

	workload := &schedulingv1alpha3.Workload{
		ObjectMeta: metav1.ObjectMeta{Name: rayCluster.Name, Namespace: rayCluster.Namespace, Labels: labels()},
		Spec: schedulingv1alpha3.WorkloadSpec{
			ControllerRef: &schedulingv1alpha3.TypedLocalObjectReference{
				APIGroup: rayv1.GroupVersion.Group,
				Kind:     "RayCluster",
				Name:     rayCluster.Name,
			},
			CompositePodGroupTemplates: []schedulingv1alpha3.CompositePodGroupTemplate{{
				Name:              clusterPodGroupTemplateName,
				SchedulingPolicy:  rootPolicy,
				PriorityClassName: priorityClassName,
				PreemptionPolicy:  preemptionPolicy,
				PodGroupTemplates: templates,
			}},
		},
	}
	root := &schedulingv1alpha3.CompositePodGroup{
		ObjectMeta: metav1.ObjectMeta{Name: rootName, Namespace: rayCluster.Namespace, Labels: labels()},
		Spec: schedulingv1alpha3.CompositePodGroupSpec{
			WorkloadRef:       &schedulingv1alpha3.WorkloadReference{WorkloadName: rayCluster.Name, TemplateName: clusterPodGroupTemplateName},
			SchedulingPolicy:  rootPolicy,
			PriorityClassName: priorityClassName,
			PreemptionPolicy:  preemptionPolicy,
		},
	}

	objects := []client.Object{workload, root}
	for _, pg := range podGroups {
		objects = append(objects, pg)
	}
	for _, object := range objects {
		if err := ctrl.SetControllerReference(rayCluster, object, k.cli.Scheme()); err != nil {
			return nil, nil, nil, err
		}
	}
	return workload, root, podGroups, nil
}

// syncTopologyResources creates the hierarchy parent first (Workload, root, leaves) and afterwards
// only patches leaf minCount values in place.
func (k *KubernetesWASV1Alpha3Scheduler) syncTopologyResources(ctx context.Context, rayCluster *rayv1.RayCluster, leaves []leaf) error {
	if !k.compositeServed {
		return fmt.Errorf("RayCluster %s/%s requests topology scheduling but the API server does not serve compositepodgroups.scheduling.k8s.io/v1alpha3; enable the CompositePodGroup and TopologyAwareWorkloadScheduling feature gates on kube-apiserver and kube-scheduler (Kubernetes 1.37+)", rayCluster.Namespace, rayCluster.Name)
	}
	workload, root, podGroups, err := k.buildTopologyResources(rayCluster, leaves)
	if err != nil {
		return fmt.Errorf("failed to build topology scheduling resources for RayCluster %s/%s: %w", rayCluster.Namespace, rayCluster.Name, err)
	}
	if err := k.syncTopologyWorkload(ctx, rayCluster, workload); err != nil {
		return err
	}
	if err := k.syncRoot(ctx, rayCluster, root); err != nil {
		return err
	}
	for _, pg := range podGroups {
		if err := k.syncLeafPodGroup(ctx, rayCluster, pg); err != nil {
			return err
		}
	}
	return nil
}

// workloadLayout summarizes the immutable parts of a topology Workload so a live object can be
// compared with the desired one. It also detects a flat (single PodGroup) Workload and a
// constraint the API server dropped.
func workloadLayout(workload *schedulingv1alpha3.Workload) []string {
	if len(workload.Spec.PodGroupTemplates) != 0 || len(workload.Spec.CompositePodGroupTemplates) != 1 {
		return []string{"not a topology layout"}
	}
	root := workload.Spec.CompositePodGroupTemplates[0]
	layout := []string{fmt.Sprintf("%s minGroupCount=%d", root.Name, minGroupCount(root.SchedulingPolicy))}
	for _, t := range root.PodGroupTemplates {
		layout = append(layout, fmt.Sprintf("%s topology=%q", t.Name, templateTopologyKey(t.SchedulingConstraints)))
	}
	return layout
}

func minGroupCount(policy schedulingv1alpha3.CompositePodGroupSchedulingPolicy) int32 {
	if policy.Gang == nil {
		return 0
	}
	return policy.Gang.MinGroupCount
}

func templateTopologyKey(constraints *schedulingv1alpha3.PodGroupSchedulingConstraints) string {
	if constraints == nil || len(constraints.Topology) == 0 {
		return ""
	}
	return constraints.Topology[0].Key
}

func errLayoutMismatch(kind string, key client.ObjectKey, want, got []string) error {
	return fmt.Errorf("%s %s does not match the requested topology layout (want %v, found %v). Either the worker groups or the %s annotation changed after creation, which is not supported (recreate the RayCluster), or the API server dropped the topology constraint (enable TopologyAwareWorkloadScheduling and CompositePodGroup on kube-apiserver)",
		kind, key, want, got, utils.RayWorkerGroupTopologyAnnotation)
}

func (k *KubernetesWASV1Alpha3Scheduler) syncTopologyWorkload(ctx context.Context, rayCluster *rayv1.RayCluster, desired *schedulingv1alpha3.Workload) error {
	key := client.ObjectKeyFromObject(desired)
	existing := &schedulingv1alpha3.Workload{}
	found, err := k.getSchedulingResource(ctx, "Workload", key, existing)
	if err != nil {
		return err
	}
	if !found {
		want := workloadLayout(desired)
		if err := k.cli.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create Workload %s: %w", key, err)
		}
		// Create returns the persisted object, so a dropped constraint is visible here.
		if got := workloadLayout(desired); !slices.Equal(want, got) {
			return errLayoutMismatch("Workload", key, want, got)
		}
		return nil
	}
	if !metav1.IsControlledBy(existing, rayCluster) {
		return fmt.Errorf("Workload %s already exists and is not owned by this RayCluster; rename it or use a different RayCluster name to avoid the collision", key)
	}
	if existing.DeletionTimestamp != nil {
		return fmt.Errorf("Workload %s is being deleted, will retry", key)
	}
	if want, got := workloadLayout(desired), workloadLayout(existing); !slices.Equal(want, got) {
		return errLayoutMismatch("Workload", key, want, got)
	}

	// Patch leaf minCount values in place; they are the only mutable fields.
	patch := client.MergeFrom(existing.DeepCopy())
	changed := false
	desiredTemplates := desired.Spec.CompositePodGroupTemplates[0].PodGroupTemplates
	for i, t := range existing.Spec.CompositePodGroupTemplates[0].PodGroupTemplates {
		gang := t.SchedulingPolicy.Gang
		want := desiredTemplates[i].SchedulingPolicy.Gang.MinCount
		if gang != nil && gang.MinCount != want {
			gang.MinCount = want
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := k.cli.Patch(ctx, existing, patch); err != nil {
		return fmt.Errorf("failed to patch Workload %s leaf minCount: %w", key, err)
	}
	return nil
}

func (k *KubernetesWASV1Alpha3Scheduler) syncRoot(ctx context.Context, rayCluster *rayv1.RayCluster, desired *schedulingv1alpha3.CompositePodGroup) error {
	key := client.ObjectKeyFromObject(desired)
	existing := &schedulingv1alpha3.CompositePodGroup{}
	found, err := k.getSchedulingResource(ctx, "CompositePodGroup", key, existing)
	if err != nil {
		return err
	}
	if !found {
		if err := k.cli.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create CompositePodGroup %s: %w", key, err)
		}
		return nil
	}
	if !metav1.IsControlledBy(existing, rayCluster) {
		return fmt.Errorf("CompositePodGroup %s already exists and is not owned by this RayCluster; rename it or use a different RayCluster name to avoid the collision", key)
	}
	// The root's policy is immutable; the Workload layout check already guards the leaf set.
	return nil
}

func (k *KubernetesWASV1Alpha3Scheduler) syncLeafPodGroup(ctx context.Context, rayCluster *rayv1.RayCluster, desired *schedulingv1alpha3.PodGroup) error {
	key := client.ObjectKeyFromObject(desired)
	existing := &schedulingv1alpha3.PodGroup{}
	found, err := k.getSchedulingResource(ctx, "PodGroup", key, existing)
	if err != nil {
		return err
	}
	if !found {
		want := templateTopologyKey(desired.Spec.SchedulingConstraints)
		if err := k.cli.Create(ctx, desired); err != nil {
			return fmt.Errorf("failed to create PodGroup %s: %w", key, err)
		}
		if got := templateTopologyKey(desired.Spec.SchedulingConstraints); want != got {
			return errLayoutMismatch("PodGroup", key, []string{want}, []string{got})
		}
		return nil
	}
	if !metav1.IsControlledBy(existing, rayCluster) {
		return fmt.Errorf("PodGroup %s already exists and is not owned by this RayCluster; rename it or use a different RayCluster name to avoid the collision", key)
	}
	return k.syncLeafMinCount(ctx, existing, desired)
}

func (k *KubernetesWASV1Alpha3Scheduler) syncLeafMinCount(ctx context.Context, existing, desired *schedulingv1alpha3.PodGroup) error {
	key := client.ObjectKeyFromObject(existing)
	if want, got := templateTopologyKey(desired.Spec.SchedulingConstraints), templateTopologyKey(existing.Spec.SchedulingConstraints); want != got {
		return errLayoutMismatch("PodGroup", key, []string{want}, []string{got})
	}
	existingGang := existing.Spec.SchedulingPolicy.Gang
	desiredMinCount := desired.Spec.SchedulingPolicy.Gang.MinCount
	if !gangNeedsMinCountPatch(existingGang, desiredMinCount) {
		return nil
	}
	return k.patchGangMinCount(ctx, "PodGroup", existing, existingGang, desiredMinCount)
}

// deleteTopologyResources removes the leaf PodGroups, then the root CompositePodGroup. It reports
// whether anything is still being torn down so the caller can requeue before deleting the Workload.
func (k *KubernetesWASV1Alpha3Scheduler) deleteTopologyResources(ctx context.Context, rayCluster *rayv1.RayCluster) (didDelete, pending bool, err error) {
	selector := client.MatchingLabels{utils.RayClusterLabelKey: rayCluster.Name}

	podGroups := &schedulingv1alpha3.PodGroupList{}
	if err := k.cli.List(ctx, podGroups, client.InNamespace(rayCluster.Namespace), selector); err != nil {
		return false, false, fmt.Errorf("failed to list PodGroups: %w", err)
	}
	for i := range podGroups.Items {
		pg := &podGroups.Items[i]
		// The flat cluster PodGroup is handled by the single-group cleanup.
		if pg.Name == clusterPodGroupName(rayCluster.Name) || !metav1.IsControlledBy(pg, rayCluster) {
			continue
		}
		deleted, err := k.deletePodGroup(ctx, pg)
		didDelete = didDelete || deleted
		if err != nil {
			return didDelete, true, err
		}
		pending = true
	}
	if pending {
		return didDelete, true, nil
	}

	roots := &schedulingv1alpha3.CompositePodGroupList{}
	if err := k.cli.List(ctx, roots, client.InNamespace(rayCluster.Namespace), selector); err != nil {
		return didDelete, false, fmt.Errorf("failed to list CompositePodGroups: %w", err)
	}
	for i := range roots.Items {
		root := &roots.Items[i]
		if !metav1.IsControlledBy(root, rayCluster) {
			continue
		}
		pending = true
		if root.DeletionTimestamp != nil {
			continue
		}
		if err := k.deleteWithUIDPrecondition(ctx, root); err != nil && !errors.IsNotFound(err) {
			return didDelete, true, fmt.Errorf("failed to delete CompositePodGroup %s/%s: %w", root.Namespace, root.Name, err)
		}
		didDelete = true
	}
	return didDelete, pending, nil
}
