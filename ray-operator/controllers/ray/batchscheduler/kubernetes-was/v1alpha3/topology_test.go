package v1alpha3

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientFake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
)

const (
	rackKey = "topology.example.com/rack"
	zoneKey = "topology.example.com/zone"
)

func newTopologyScheduler(t *testing.T, objects ...client.Object) (*KubernetesWASV1Alpha3Scheduler, client.Client) {
	t.Helper()
	scheduler, cli := newTestScheduler(t, objects...)
	scheduler.compositeServed = true
	return scheduler, cli
}

// newTopologyRayCluster has a head, "training" (rack, 4 pods), "data" (zone, 2 pods x 2 hosts),
// and "plain" (no topology, 1 pod).
func newTopologyRayCluster() *rayv1.RayCluster {
	rayCluster := newTestRayCluster(
		newWorkerGroupWithReplicas("training", 4),
		workerGroupWithNumOfHosts("data", 2, 2),
		newWorkerGroupWithReplicas("plain", 1),
	)
	rayCluster.Annotations = map[string]string{
		utils.RayWorkerGroupTopologyAnnotation: `{"training":"` + rackKey + `","data":"` + zoneKey + `"}`,
	}
	return rayCluster
}

func getWorkload(t *testing.T, cli client.Client) *schedulingv1alpha3.Workload {
	t.Helper()
	workload := &schedulingv1alpha3.Workload{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-cluster"}, workload))
	return workload
}

func getPodGroup(t *testing.T, cli client.Client, name string) *schedulingv1alpha3.PodGroup {
	t.Helper()
	podGroup := &schedulingv1alpha3.PodGroup{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, podGroup))
	return podGroup
}

func TestTopologyBuildLeaves(t *testing.T) {
	rayCluster := newTopologyRayCluster()
	leaves, requested, err := topologyLeaves(rayCluster)
	require.NoError(t, err)
	require.True(t, requested)
	assert.Equal(t, []leaf{
		{templateName: "head", groupName: utils.RayNodeHeadGroupLabelValue, minCount: 1},
		{templateName: "wg-training", groupName: "training", topologyKey: rackKey, minCount: 4},
		{templateName: "wg-data", groupName: "data", topologyKey: zoneKey, minCount: 4},
		{templateName: "wg-plain", groupName: "plain", minCount: 1},
	}, leaves)
}

func TestTopologyBuildLeavesUsesAutoscalingFloor(t *testing.T) {
	rayCluster := withAutoscaling(newTopologyRayCluster())
	rayCluster.Spec.WorkerGroupSpecs[0] = newAutoscalingWorkerGroup("training", 2, 4)
	rayCluster.Spec.WorkerGroupSpecs[1] = newAutoscalingWorkerGroup("data", 1, 3)
	rayCluster.Spec.WorkerGroupSpecs[1].NumOfHosts = 2
	rayCluster.Spec.WorkerGroupSpecs[2] = newAutoscalingWorkerGroup("plain", 1, 5)

	leaves, _, err := topologyLeaves(rayCluster)
	require.NoError(t, err)
	assert.Equal(t, int32(2), leaves[1].minCount)
	assert.Equal(t, int32(2), leaves[2].minCount)
	assert.Equal(t, int32(1), leaves[3].minCount)
}

func TestTopologyBuildLeavesOmitsFloorZeroGroupsWithoutTopology(t *testing.T) {
	rayCluster := newTopologyRayCluster()
	rayCluster.Spec.WorkerGroupSpecs[2].Suspend = ptr.To(true)

	leaves, _, err := topologyLeaves(rayCluster)
	require.NoError(t, err)
	require.Len(t, leaves, 3)
	assert.Empty(t, leafPodGroupName(rayCluster, leaves, "plain"))
	assert.Equal(t, "test-cluster-wg-training", leafPodGroupName(rayCluster, leaves, "training"))
	assert.Equal(t, "test-cluster-head", leafPodGroupName(rayCluster, leaves, utils.RayNodeHeadGroupLabelValue))
}

func TestTopologyBuildLeavesRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		mutate  func(*rayv1.RayCluster)
		name    string
		wantErr string
	}{
		{name: "not json", mutate: func(rc *rayv1.RayCluster) { rc.Annotations[utils.RayWorkerGroupTopologyAnnotation] = "rack" }, wantErr: "JSON object"},
		{name: "empty", mutate: func(rc *rayv1.RayCluster) { rc.Annotations[utils.RayWorkerGroupTopologyAnnotation] = "{}" }, wantErr: "at least one"},
		{name: "unknown group", mutate: func(rc *rayv1.RayCluster) {
			rc.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"nope":"` + rackKey + `"}`
		}, wantErr: `unknown worker group "nope"`},
		{name: "invalid key", mutate: func(rc *rayv1.RayCluster) {
			rc.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"training":"Not A Key!"}`
		}, wantErr: "invalid topology key"},
		{name: "floor zero with topology", mutate: func(rc *rayv1.RayCluster) { rc.Spec.WorkerGroupSpecs[0].Suspend = ptr.To(true) }, wantErr: "gang floor of zero"},
		{name: "too many groups", mutate: func(rc *rayv1.RayCluster) {
			rc.Spec.WorkerGroupSpecs = append(rc.Spec.WorkerGroupSpecs, newWorkerGroups(5)...)
		}, wantErr: "at most 7 worker groups"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rayCluster := newTopologyRayCluster()
			tt.mutate(rayCluster)
			_, requested, err := topologyLeaves(rayCluster)
			require.True(t, requested)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestTopologyNotRequestedKeepsFlatLayout(t *testing.T) {
	rayCluster := newTestRayCluster(newWorkerGroup())
	_, requested, err := topologyLeaves(rayCluster)
	require.NoError(t, err)
	assert.False(t, requested)
}

func TestTopologyCreatesHierarchy(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))

	workload := getWorkload(t, cli)
	assert.Empty(t, workload.Spec.PodGroupTemplates)
	require.Len(t, workload.Spec.CompositePodGroupTemplates, 1)
	rootTemplate := workload.Spec.CompositePodGroupTemplates[0]
	assert.Equal(t, "cluster", rootTemplate.Name)
	assert.Equal(t, int32(4), rootTemplate.SchedulingPolicy.Gang.MinGroupCount)
	assert.Nil(t, rootTemplate.SchedulingConstraints)

	templates := map[string]schedulingv1alpha3.PodGroupTemplate{}
	for _, template := range rootTemplate.PodGroupTemplates {
		templates[template.Name] = template
	}
	require.Len(t, templates, 4)
	assert.Equal(t, int32(1), templates["head"].SchedulingPolicy.Gang.MinCount)
	assert.Nil(t, templates["head"].SchedulingConstraints, "head must not be topology constrained")
	assert.Equal(t, int32(4), templates["wg-training"].SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, rackKey, templates["wg-training"].SchedulingConstraints.Topology[0].Key)
	assert.Equal(t, int32(4), templates["wg-data"].SchedulingPolicy.Gang.MinCount, "2 replicas x 2 hosts")
	assert.Equal(t, zoneKey, templates["wg-data"].SchedulingConstraints.Topology[0].Key)
	assert.Nil(t, templates["wg-plain"].SchedulingConstraints)

	root := &schedulingv1alpha3.CompositePodGroup{}
	require.NoError(t, cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-cluster-cluster"}, root))
	assert.Equal(t, int32(4), root.Spec.SchedulingPolicy.Gang.MinGroupCount)
	assert.Equal(t, "test-cluster", root.Spec.WorkloadRef.WorkloadName)
	assert.Equal(t, "cluster", root.Spec.WorkloadRef.TemplateName)
	assert.Nil(t, root.Spec.SchedulingConstraints)
	assert.True(t, metav1.IsControlledBy(root, rayCluster))

	for name, wantKey := range map[string]string{
		"test-cluster-head": "", "test-cluster-wg-training": rackKey, "test-cluster-wg-data": zoneKey, "test-cluster-wg-plain": "",
	} {
		podGroup := getPodGroup(t, cli, name)
		assert.Equal(t, "test-cluster-cluster", *podGroup.Spec.ParentCompositePodGroupName, name)
		assert.Equal(t, "test-cluster", podGroup.Spec.WorkloadRef.WorkloadName, name)
		assert.Equal(t, wantKey, templateTopologyKey(podGroup.Spec.SchedulingConstraints), name)
		assert.True(t, metav1.IsControlledBy(podGroup, rayCluster), name)
	}
	// No flat PodGroup is created for a topology layout.
	assert.Len(t, listPodGroups(t, cli), 4)
}

func listPodGroups(t *testing.T, cli client.Client) []schedulingv1alpha3.PodGroup {
	t.Helper()
	list := &schedulingv1alpha3.PodGroupList{}
	require.NoError(t, cli.List(context.Background(), list))
	return list.Items
}

func TestTopologyCopiesPriorityToEveryGroup(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	rayCluster.Spec.HeadGroupSpec.Template.Spec.PriorityClassName = "high"
	rayCluster.Spec.HeadGroupSpec.Template.Spec.PreemptionPolicy = ptr.To(corev1.PreemptNever)

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))

	rootTemplate := getWorkload(t, cli).Spec.CompositePodGroupTemplates[0]
	assert.Equal(t, "high", rootTemplate.PriorityClassName)
	assert.Equal(t, schedulingv1alpha3.PreemptNever, *rootTemplate.PreemptionPolicy)
	for _, template := range rootTemplate.PodGroupTemplates {
		assert.Equal(t, "high", template.PriorityClassName, template.Name)
	}
	for _, podGroup := range listPodGroups(t, cli) {
		assert.Equal(t, "high", podGroup.Spec.PriorityClassName, podGroup.Name)
		assert.Equal(t, schedulingv1alpha3.PreemptNever, *podGroup.Spec.PreemptionPolicy, podGroup.Name)
	}
}

func TestTopologyRequiresCompositePodGroupsServed(t *testing.T) {
	scheduler, cli := newTestScheduler(t)

	err := scheduler.DoBatchSchedulingOnSubmission(context.Background(), newTopologyRayCluster())

	require.ErrorContains(t, err, "compositepodgroups")
	assert.Empty(t, listPodGroups(t, cli))
}

func TestTopologyRejectsInvalidRequestBeforeCreatingAnything(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	rayCluster.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"nope":"` + rackKey + `"}`

	err := scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster)

	require.ErrorContains(t, err, "unknown worker group")
	assert.Empty(t, listPodGroups(t, cli))
}

func TestTopologyDetectsConstraintDroppedByAPIServer(t *testing.T) {
	dropConstraints := interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			// Simulate kube-apiserver without TopologyAwareWorkloadScheduling clearing the field.
			if workload, ok := obj.(*schedulingv1alpha3.Workload); ok {
				for i := range workload.Spec.CompositePodGroupTemplates[0].PodGroupTemplates {
					workload.Spec.CompositePodGroupTemplates[0].PodGroupTemplates[i].SchedulingConstraints = nil
				}
			}
			return c.Create(ctx, obj, opts...)
		},
	}
	cli := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).WithInterceptorFuncs(dropConstraints).Build()
	scheduler := &KubernetesWASV1Alpha3Scheduler{cli: cli, compositeServed: true}

	err := scheduler.DoBatchSchedulingOnSubmission(context.Background(), newTopologyRayCluster())

	require.ErrorContains(t, err, "dropped the topology constraint")
}

func TestTopologyResizePatchesLeafMinCountInPlace(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))
	uid := getPodGroup(t, cli, "test-cluster-wg-training").UID
	before := getWorkload(t, cli).UID

	rayCluster.Spec.WorkerGroupSpecs[0] = newWorkerGroupWithReplicas("training", 6)
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))

	assert.Equal(t, before, getWorkload(t, cli).UID, "Workload must not be recreated")
	assert.Equal(t, uid, getPodGroup(t, cli, "test-cluster-wg-training").UID, "PodGroup must not be recreated")
	assert.Equal(t, int32(6), getPodGroup(t, cli, "test-cluster-wg-training").Spec.SchedulingPolicy.Gang.MinCount)
	for _, template := range getWorkload(t, cli).Spec.CompositePodGroupTemplates[0].PodGroupTemplates {
		want := map[string]int32{"head": 1, "wg-training": 6, "wg-data": 4, "wg-plain": 1}[template.Name]
		assert.Equal(t, want, template.SchedulingPolicy.Gang.MinCount, template.Name)
	}
	// Siblings and the root are untouched.
	assert.Equal(t, int32(4), getPodGroup(t, cli, "test-cluster-wg-data").Spec.SchedulingPolicy.Gang.MinCount)
	assert.Equal(t, int32(4), getWorkload(t, cli).Spec.CompositePodGroupTemplates[0].SchedulingPolicy.Gang.MinGroupCount)
}

func TestTopologyIsIdempotentWhenUnchanged(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))
	version := getWorkload(t, cli).ResourceVersion

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))

	assert.Equal(t, version, getWorkload(t, cli).ResourceVersion)
}

func TestTopologyStructuralChangesAreRejected(t *testing.T) {
	tests := []struct {
		mutate func(*rayv1.RayCluster)
		name   string
	}{
		{name: "topology key changed", mutate: func(rc *rayv1.RayCluster) {
			rc.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"training":"` + zoneKey + `","data":"` + zoneKey + `"}`
		}},
		{name: "topology removed from a group", mutate: func(rc *rayv1.RayCluster) {
			rc.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"training":"` + rackKey + `"}`
		}},
		{name: "worker group added", mutate: func(rc *rayv1.RayCluster) {
			rc.Spec.WorkerGroupSpecs = append(rc.Spec.WorkerGroupSpecs, newWorkerGroupWithReplicas("extra", 1))
		}},
		{name: "worker group suspended", mutate: func(rc *rayv1.RayCluster) { rc.Spec.WorkerGroupSpecs[2].Suspend = ptr.To(true) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheduler, cli := newTopologyScheduler(t)
			rayCluster := newTopologyRayCluster()
			require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))
			uid := getWorkload(t, cli).UID

			tt.mutate(rayCluster)
			err := scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster)

			require.ErrorContains(t, err, "recreate the RayCluster")
			assert.Equal(t, uid, getWorkload(t, cli).UID, "scheduling objects must not be deleted or recreated")
		})
	}
}

func TestTopologyAddedToExistingFlatClusterIsRejected(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	delete(rayCluster.Annotations, utils.RayWorkerGroupTopologyAnnotation)
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))
	require.Len(t, listPodGroups(t, cli), 1)

	rayCluster.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"training":"` + rackKey + `"}`
	err := scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster)

	require.ErrorContains(t, err, "recreate the RayCluster")
}

func TestTopologyRemovedFromExistingTopologyClusterIsRejected(t *testing.T) {
	scheduler, _ := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))

	delete(rayCluster.Annotations, utils.RayWorkerGroupTopologyAnnotation)
	err := scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster)

	require.ErrorContains(t, err, "no longer requests topology")
}

func TestTopologyRoutesPodsToTheirOwnPodGroup(t *testing.T) {
	scheduler, _ := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()

	for groupName, want := range map[string]string{
		utils.RayNodeHeadGroupLabelValue: "test-cluster-head",
		"training":                       "test-cluster-wg-training",
		"data":                           "test-cluster-wg-data",
		"plain":                          "test-cluster-wg-plain",
	} {
		pod := &corev1.Pod{}
		scheduler.AddMetadataToChildResource(context.Background(), rayCluster, pod, groupName)
		require.NotNil(t, pod.Spec.SchedulingGroup, groupName)
		assert.Equal(t, want, *pod.Spec.SchedulingGroup.PodGroupName, groupName)
		assert.Equal(t, corev1.DefaultSchedulerName, pod.Spec.SchedulerName, groupName)
	}
}

func TestTopologyPodsOfGroupsWithoutLeafAreNotInAGang(t *testing.T) {
	scheduler, _ := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	rayCluster.Spec.WorkerGroupSpecs[2].Suspend = ptr.To(true)

	pod := &corev1.Pod{}
	scheduler.AddMetadataToChildResource(context.Background(), rayCluster, pod, "plain")

	assert.Nil(t, pod.Spec.SchedulingGroup)
	assert.Equal(t, corev1.DefaultSchedulerName, pod.Spec.SchedulerName)
}

func TestTopologyDoesNotChangeFlatRouting(t *testing.T) {
	scheduler, _ := newTopologyScheduler(t)
	rayCluster := newTestRayCluster(newWorkerGroup())

	pod := &corev1.Pod{}
	scheduler.AddMetadataToChildResource(context.Background(), rayCluster, pod, "workers")

	assertPodGroupMembership(t, pod)
}

func TestTopologyCleanupDeletesLeavesThenRootThenWorkload(t *testing.T) {
	scheduler, cli := newTopologyScheduler(t)
	rayCluster := newTopologyRayCluster()
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(context.Background(), rayCluster))
	// Leaves carry the Kubernetes protection finalizer, as observed on a real cluster.
	for _, podGroup := range listPodGroups(t, cli) {
		podGroup := podGroup
		podGroup.Finalizers = []string{podGroupProtectionFinalizer}
		require.NoError(t, cli.Update(context.Background(), &podGroup))
	}

	var err error
	for range 6 {
		if _, err = scheduler.CleanupOnCompletion(context.Background(), rayCluster); err == nil {
			break
		}
		require.ErrorContains(t, err, "waiting for")
	}
	require.NoError(t, err)

	assert.Empty(t, listPodGroups(t, cli))
	roots := &schedulingv1alpha3.CompositePodGroupList{}
	require.NoError(t, cli.List(context.Background(), roots))
	assert.Empty(t, roots.Items)
	workload := &schedulingv1alpha3.Workload{}
	err = cli.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "test-cluster"}, workload)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestTopologyCleanupIgnoresForeignObjects(t *testing.T) {
	rayCluster := newTopologyRayCluster()
	foreign := &schedulingv1alpha3.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Name: "someone-elses", Namespace: "default", Labels: map[string]string{utils.RayClusterLabelKey: rayCluster.Name},
	}}
	scheduler, cli := newTopologyScheduler(t, foreign)

	_, err := scheduler.CleanupOnCompletion(context.Background(), rayCluster)

	require.NoError(t, err)
	assert.Len(t, listPodGroups(t, cli), 1)
}

func TestProviderPassesCompositeAvailabilityToScheduler(t *testing.T) {
	// ConfigureReconciler needs a live manager to inspect, so verify the availability flag that gates its watch.
	provider := &Provider{}
	scheduler := provider.NewScheduler(clientFake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build())
	assert.False(t, scheduler.(*KubernetesWASV1Alpha3Scheduler).compositeServed)

	provider.compositeServed = true
	scheduler = provider.NewScheduler(clientFake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build())
	assert.True(t, scheduler.(*KubernetesWASV1Alpha3Scheduler).compositeServed)
}
