package v1beta1

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientFake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	schedulerinterface "github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/interface"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler/kubernetes-was/shared"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	"github.com/ray-project/kuberay/ray-operator/pkg/features"
)

func newScheduler(cli client.Client) schedulerinterface.BatchScheduler {
	return (&Provider{}).NewScheduler(cli)
}

// buildWorkload / buildPodGroup construct concrete typed objects for test setup,
// unwrapping the adapter's client.Object return.
func buildWorkload(rayCluster *rayv1.RayCluster, minCount int32) *schedulingv1beta1.Workload {
	return adapter{}.BuildWorkload(rayCluster, minCount).(*schedulingv1beta1.Workload)
}

func buildPodGroup(rayCluster *rayv1.RayCluster, minCount int32) *schedulingv1beta1.PodGroup {
	return adapter{}.BuildPodGroup(rayCluster, minCount).(*schedulingv1beta1.PodGroup)
}

func TestName(t *testing.T) {
	require.Equal(t, "kubernetes-was-v1beta1", newScheduler(nil).Name())
}

func TestBuildPreemptionPolicyGatedOff(t *testing.T) {
	rayCluster := newTestRayCluster(newWorkerGroup())
	rayCluster.Annotations = map[string]string{utils.RayKubernetesWASPreemptionPolicyAnnotationKey: "PreemptLowerPriority"}

	// Gate off: the annotation is ignored and no PreemptionPolicy is set.
	workload := buildWorkload(rayCluster, 4)
	assert.Nil(t, workload.Spec.PodGroupTemplates[0].PreemptionPolicy)
	podGroup := buildPodGroup(rayCluster, 4)
	assert.Nil(t, podGroup.Spec.PreemptionPolicy)
}

func TestBuildPreemptionPolicyFromAnnotation(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.KubernetesWASPodGroupPreemptionPolicy, true)

	tests := []struct {
		name       string
		annotation string
		want       *schedulingv1beta1.PreemptionPolicy
	}{
		{name: "preempt lower priority", annotation: "PreemptLowerPriority", want: ptr(schedulingv1beta1.PreemptLowerPriority)},
		{name: "never", annotation: "Never", want: ptr(schedulingv1beta1.PreemptNever)},
		{name: "absent", annotation: "", want: nil},
		{name: "unrecognized value", annotation: "bogus", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rayCluster := newTestRayCluster(newWorkerGroup())
			if tt.annotation != "" {
				rayCluster.Annotations = map[string]string{utils.RayKubernetesWASPreemptionPolicyAnnotationKey: tt.annotation}
			}

			workload := buildWorkload(rayCluster, 4)
			assert.Equal(t, tt.want, workload.Spec.PodGroupTemplates[0].PreemptionPolicy)
			podGroup := buildPodGroup(rayCluster, 4)
			assert.Equal(t, tt.want, podGroup.Spec.PreemptionPolicy)
		})
	}
}

func ptr(p schedulingv1beta1.PreemptionPolicy) *schedulingv1beta1.PreemptionPolicy {
	return &p
}

func TestDoBatchSchedulingOnSubmissionCreatesWorkloadAndPodGroups(t *testing.T) {
	ctx := context.Background()
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	scheduler := newScheduler(fakeClient)
	rayCluster := newTestRayCluster(newWorkerGroup())

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))

	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	assert.Equal(t, "cluster", workload.Spec.PodGroupTemplates[0].Name)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang)
	// MinCount = 1 head + 3 worker replicas.
	assert.Equal(t, int32(4), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
	require.NotNil(t, workload.Spec.ControllerRef)
	assert.Equal(t, "RayCluster", workload.Spec.ControllerRef.Kind)

	podGroup := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, podGroup))
	require.NotNil(t, podGroup.Spec.SchedulingPolicy.Gang)
	assert.Equal(t, int32(4), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
	require.NotNil(t, podGroup.Spec.WorkloadRef)
	assert.Equal(t, "test-cluster", podGroup.Spec.WorkloadRef.WorkloadName)
	assert.Equal(t, "cluster", podGroup.Spec.WorkloadRef.TemplateName)
}

func TestDoBatchSchedulingOnSubmissionAllowsManyWorkerGroups(t *testing.T) {
	ctx := context.Background()
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	scheduler := newScheduler(fakeClient)
	// The single whole-cluster PodGroup uses only one template slot, so there is
	// no cap on the number of worker groups.
	workerGroupCount := schedulingv1beta1.WorkloadMaxPodGroupTemplates + 2
	rayCluster := newTestRayCluster(newWorkerGroups(workerGroupCount)...)

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))

	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang)
	// MinCount = 1 head + one replica per worker group.
	assert.Equal(t, int32(1+workerGroupCount), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)
}

func TestSyncPatchesMinCountInPlaceOnScaleUp(t *testing.T) {
	ctx := context.Background()
	rayCluster := newTestRayCluster(newWorkerGroupWithReplicas("workers", 5)) // desired minCount 6

	// Existing resources were created at the previous scale (minCount 4).
	existingWorkload := buildWorkload(rayCluster, 4)
	existingPodGroup := buildPodGroup(rayCluster, 4)
	setRayClusterControllerReference(rayCluster, existingWorkload, existingPodGroup)
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(existingWorkload, existingPodGroup).Build()
	scheduler := newScheduler(fakeClient)

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))

	// Both resources are patched in place (same UID), not deleted and recreated.
	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, workload))
	assert.Equal(t, existingWorkload.UID, workload.UID)
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang)
	assert.Equal(t, int32(6), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)

	podGroup := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, podGroup))
	assert.Equal(t, existingPodGroup.UID, podGroup.UID)
	require.NotNil(t, podGroup.Spec.SchedulingPolicy.Gang)
	assert.Equal(t, int32(6), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
}

func TestDoBatchSchedulingOnSubmissionIsIdempotentWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	scheduler := newScheduler(fakeClient)
	rayCluster := newTestRayCluster(newWorkerGroup())

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))
	workloadAfterFirst := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, workloadAfterFirst))
	podGroupAfterFirst := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, podGroupAfterFirst))

	// A second reconcile with an unchanged spec is a no-op: neither resource is
	// patched (ResourceVersion is unchanged) or recreated.
	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))

	workloadAfterSecond := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, workloadAfterSecond))
	assert.Equal(t, workloadAfterFirst.ResourceVersion, workloadAfterSecond.ResourceVersion, "Workload should not be modified on an unchanged reconcile")

	podGroupAfterSecond := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, podGroupAfterSecond))
	assert.Equal(t, podGroupAfterFirst.ResourceVersion, podGroupAfterSecond.ResourceVersion, "PodGroup should not be modified on an unchanged reconcile")
}

func TestAddMetadataToChildResourceSetsSchedulingGroup(t *testing.T) {
	scheduler := newScheduler(nil)
	rayCluster := newTestRayCluster(newWorkerGroup())

	headPod := &corev1.Pod{}
	scheduler.AddMetadataToChildResource(context.Background(), rayCluster, headPod, utils.RayNodeHeadGroupLabelValue)
	require.NotNil(t, headPod.Spec.SchedulingGroup)
	require.NotNil(t, headPod.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, "test-cluster-cluster", *headPod.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, corev1.DefaultSchedulerName, headPod.Spec.SchedulerName)

	template := &corev1.PodTemplateSpec{}
	scheduler.AddMetadataToChildResource(context.Background(), rayCluster, template, "workers")
	require.NotNil(t, template.Spec.SchedulingGroup)
	require.NotNil(t, template.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, "test-cluster-cluster", *template.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, corev1.DefaultSchedulerName, template.Spec.SchedulerName)
}

func TestDoBatchSchedulingOnSubmissionSkipsAndCleansUpWithoutGangLabel(t *testing.T) {
	assertSkipCleansUp(t, func(rayCluster *rayv1.RayCluster) {
		delete(rayCluster.Labels, utils.RayGangSchedulingEnabled)
	})
}

// An autoscaling cluster is NOT skipped; it gangs only the floor (head + minReplicas)
// so the Ray autoscaler can scale above it without deadlocking the gang.
func TestDoBatchSchedulingOnSubmissionGangsFloorWhenAutoscalingEnabled(t *testing.T) {
	ctx := context.Background()
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	scheduler := newScheduler(fakeClient)
	rayCluster := newTestRayCluster(newElasticWorkerGroup("workers", 2, 5, 8))
	enableAutoscaling := true
	rayCluster.Spec.EnableInTreeAutoscaling = &enableAutoscaling

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))

	workload := &schedulingv1beta1.Workload{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, workload))
	require.Len(t, workload.Spec.PodGroupTemplates, 1)
	require.NotNil(t, workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang)
	// Floor minCount = 1 head + 2 minReplicas (not 1 + 5 desired).
	assert.Equal(t, int32(3), workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount)

	podGroup := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, podGroup))
	require.NotNil(t, podGroup.Spec.SchedulingPolicy.Gang)
	assert.Equal(t, int32(3), podGroup.Spec.SchedulingPolicy.Gang.MinCount)
}

func TestAddMetadataToChildResourceStampsWhenAutoscalingEnabled(t *testing.T) {
	scheduler := newScheduler(nil)
	rayCluster := newTestRayCluster(newWorkerGroup())
	enableAutoscaling := true
	rayCluster.Spec.EnableInTreeAutoscaling = &enableAutoscaling

	pod := &corev1.Pod{}
	scheduler.AddMetadataToChildResource(context.Background(), rayCluster, pod, "workers")
	require.NotNil(t, pod.Spec.SchedulingGroup)
	require.NotNil(t, pod.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, "test-cluster-cluster", *pod.Spec.SchedulingGroup.PodGroupName)
	assert.Equal(t, corev1.DefaultSchedulerName, pod.Spec.SchedulerName)
}

// assertSkipCleansUp verifies that a skipped RayCluster has its previously created
// scheduling resources torn down in dependency order.
func assertSkipCleansUp(t *testing.T, makeSkipped func(*rayv1.RayCluster)) {
	t.Helper()
	ctx := context.Background()
	rayCluster := newTestRayCluster(newWorkerGroup())
	existingWorkload := buildWorkload(rayCluster, 4)
	existingPodGroup := buildPodGroup(rayCluster, 4)
	setRayClusterControllerReference(rayCluster, existingWorkload, existingPodGroup)
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(existingWorkload, existingPodGroup).Build()
	scheduler := newScheduler(fakeClient)
	makeSkipped(rayCluster)

	err := scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "waiting for PodGroup default/test-cluster-cluster")
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, &schedulingv1beta1.Workload{}))
	getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, &schedulingv1beta1.PodGroup{})
	assert.True(t, apierrors.IsNotFound(getErr))

	require.NoError(t, scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster))
	getErr = fakeClient.Get(ctx, types.NamespacedName{Name: rayCluster.Name, Namespace: rayCluster.Namespace}, &schedulingv1beta1.Workload{})
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestSyncRejectsForeignSameNameWorkload(t *testing.T) {
	ctx := context.Background()
	rayCluster := newTestRayCluster(newWorkerGroup())
	foreignRayCluster := newTestRayCluster(newWorkerGroup())
	foreignRayCluster.Name = "foreign-cluster"
	foreignRayCluster.UID = types.UID("foreign-cluster-uid")
	foreignWorkload := buildWorkload(rayCluster, 4)
	setRayClusterControllerReference(foreignRayCluster, foreignWorkload)
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(foreignWorkload).Build()
	scheduler := newScheduler(fakeClient)

	err := scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Workload default/test-cluster already exists and is not owned by this RayCluster")
	getErr := fakeClient.Get(ctx, types.NamespacedName{Name: "test-cluster-cluster", Namespace: rayCluster.Namespace}, &schedulingv1beta1.PodGroup{})
	assert.True(t, apierrors.IsNotFound(getErr))
}

func TestSyncRejectsForeignSameNamePodGroup(t *testing.T) {
	ctx := context.Background()
	rayCluster := newTestRayCluster(newWorkerGroup())
	foreignRayCluster := newTestRayCluster(newWorkerGroup())
	foreignRayCluster.Name = "foreign-cluster"
	foreignRayCluster.UID = types.UID("foreign-cluster-uid")
	existingWorkload := buildWorkload(rayCluster, 4)
	foreignPodGroup := buildPodGroup(rayCluster, 4)
	setRayClusterControllerReference(rayCluster, existingWorkload)
	setRayClusterControllerReference(foreignRayCluster, foreignPodGroup)
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(existingWorkload, foreignPodGroup).Build()
	scheduler := newScheduler(fakeClient)

	err := scheduler.DoBatchSchedulingOnSubmission(ctx, rayCluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "PodGroup default/test-cluster-cluster already exists and is not owned by this RayCluster")
}

func TestCleanupOnCompletionDeletesInDependencyOrder(t *testing.T) {
	ctx := context.Background()
	rayCluster := newTestRayCluster(newWorkerGroup())
	existingWorkload := buildWorkload(rayCluster, 4)
	existingPodGroup := buildPodGroup(rayCluster, 4)
	existingPodGroup.Finalizers = []string{shared.PodGroupProtectionFinalizer, "example.com/retain"}
	setRayClusterControllerReference(rayCluster, existingWorkload, existingPodGroup)
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(existingWorkload, existingPodGroup).Build()
	scheduler := newScheduler(fakeClient)

	didCleanup, err := scheduler.CleanupOnCompletion(ctx, rayCluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "waiting for PodGroup default/test-cluster-cluster")
	assert.True(t, didCleanup)

	// Only the protection finalizer is removed; the unrelated finalizer keeps the
	// PodGroup terminating, so the Workload must be retained this reconcile.
	podGroup := &schedulingv1beta1.PodGroup{}
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: existingPodGroup.Name, Namespace: existingPodGroup.Namespace}, podGroup))
	assert.NotContains(t, podGroup.Finalizers, shared.PodGroupProtectionFinalizer)
	assert.Contains(t, podGroup.Finalizers, "example.com/retain")
	assert.NotNil(t, podGroup.DeletionTimestamp)
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: existingWorkload.Name, Namespace: existingWorkload.Namespace}, &schedulingv1beta1.Workload{}))
}

func TestCleanupOnCompletionNotFoundIsNoop(t *testing.T) {
	ctx := context.Background()
	fakeClient := clientFake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	scheduler := newScheduler(fakeClient)

	didCleanup, err := scheduler.CleanupOnCompletion(ctx, newTestRayCluster(newWorkerGroup()))
	require.NoError(t, err)
	assert.False(t, didCleanup)
}

func TestProviderAvailable(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		wantErr     bool
		errContains string
	}{
		{
			name: "API available returns resource list",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/apis/scheduling.k8s.io/v1beta1" {
					writer.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.NewEncoder(writer).Encode(metav1.APIResourceList{
						GroupVersion: "scheduling.k8s.io/v1beta1",
						APIResources: []metav1.APIResource{
							{Name: "workloads", Kind: "Workload", Namespaced: true},
							{Name: "podgroups", Kind: "PodGroup", Namespaced: true},
						},
					}))
					return
				}
				http.NotFound(writer, request)
			},
		},
		{
			name: "API not available returns 404",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				http.NotFound(writer, request)
			},
			wantErr:     true,
			errContains: "scheduling.k8s.io/v1beta1 API is not available",
		},
		{
			name: "different group version does not satisfy v1beta1",
			handler: func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/apis/scheduling.k8s.io/v1alpha3" {
					writer.Header().Set("Content-Type", "application/json")
					assert.NoError(t, json.NewEncoder(writer).Encode(metav1.APIResourceList{GroupVersion: "scheduling.k8s.io/v1alpha3"}))
					return
				}
				http.NotFound(writer, request)
			},
			wantErr:     true,
			errContains: "scheduling.k8s.io/v1beta1 API is not available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			err := (&Provider{}).Available(&rest.Config{Host: server.URL})
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestProviderAvailableAllowsNilConfig(t *testing.T) {
	require.NoError(t, (&Provider{}).Available(nil))
}

func TestProviderAvailableUnreachableServer(t *testing.T) {
	err := (&Provider{}).Available(&rest.Config{Host: "http://127.0.0.1:1"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "scheduling.k8s.io/v1beta1 API is not available") || strings.Contains(err.Error(), "connection refused"))
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, rayv1.AddToScheme(scheme))
	require.NoError(t, schedulingv1beta1.AddToScheme(scheme))
	return scheme
}

func newTestRayCluster(workerGroups ...rayv1.WorkerGroupSpec) *rayv1.RayCluster {
	return &rayv1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
			UID:       types.UID("test-cluster-uid"),
			Labels:    map[string]string{utils.RayGangSchedulingEnabled: "true"},
		},
		Spec: rayv1.RayClusterSpec{
			HeadGroupSpec:    rayv1.HeadGroupSpec{Template: corev1.PodTemplateSpec{}},
			WorkerGroupSpecs: workerGroups,
		},
	}
}

func setRayClusterControllerReference(rayCluster *rayv1.RayCluster, objects ...metav1.Object) {
	ownerReference := *metav1.NewControllerRef(rayCluster, rayv1.GroupVersion.WithKind("RayCluster"))
	for _, object := range objects {
		if object.GetUID() == "" {
			object.SetUID(types.UID(object.GetName() + "-uid"))
		}
		object.SetOwnerReferences([]metav1.OwnerReference{ownerReference})
	}
}

func newWorkerGroup() rayv1.WorkerGroupSpec {
	return newWorkerGroupWithReplicas("workers", 3)
}

func newWorkerGroups(count int) []rayv1.WorkerGroupSpec {
	groups := make([]rayv1.WorkerGroupSpec, count)
	for i := range groups {
		groups[i] = newWorkerGroupWithReplicas("workers-"+string(rune('a'+i)), 1)
	}
	return groups
}

func newWorkerGroupWithReplicas(groupName string, replicas int32) rayv1.WorkerGroupSpec {
	return rayv1.WorkerGroupSpec{
		GroupName:   groupName,
		NumOfHosts:  1,
		Replicas:    &replicas,
		MinReplicas: &replicas,
		MaxReplicas: &replicas,
		Template:    corev1.PodTemplateSpec{},
	}
}

// newElasticWorkerGroup allows minReplicas < replicas so the gang floor differs
// from the desired size (used by the autoscaling gang-the-floor tests).
func newElasticWorkerGroup(groupName string, minReplicas, replicas, maxReplicas int32) rayv1.WorkerGroupSpec {
	return rayv1.WorkerGroupSpec{
		GroupName:   groupName,
		NumOfHosts:  1,
		Replicas:    &replicas,
		MinReplicas: &minReplicas,
		MaxReplicas: &maxReplicas,
		Template:    corev1.PodTemplateSpec{},
	}
}
