package e2ekuberneteswas

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	rayv1ac "github.com/ray-project/kuberay/ray-operator/pkg/client/applyconfiguration/ray/v1"
	. "github.com/ray-project/kuberay/ray-operator/test/support"
)

// These tests exercise the scheduling.k8s.io/v1beta1 provider and require a
// Kubernetes 1.37+ cluster serving that API. They are named with a "V1Beta1"
// segment so the `test-e2e-kubernetes-was-v1beta1` Makefile target selects them
// with `-run`; the v1alpha2 suite is excluded by the same mechanism.

func TestKubernetesWASV1Beta1_CreatesWorkloadAndPodGroups(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	rayClusterAC := newWASRayClusterAC("native-sched", namespace.Name).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created RayCluster %s/%s successfully", rayCluster.Namespace, rayCluster.Name)

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	LogWithTimestamp(test.T(), "Verifying Workload %s/%s exists", namespace.Name, rayCluster.Name)
	workload, err := GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(workload.Spec.ControllerRef).NotTo(BeNil())
	g.Expect(workload.Spec.ControllerRef.APIGroup).To(Equal("ray.io"))
	g.Expect(workload.Spec.ControllerRef.Kind).To(Equal("RayCluster"))
	g.Expect(workload.Spec.ControllerRef.Name).To(Equal(rayCluster.Name))

	g.Expect(workload.Spec.PodGroupTemplates).To(HaveLen(1))
	g.Expect(workload.Spec.PodGroupTemplates[0].Name).To(Equal("cluster"))
	g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(BeNil())
	// MinCount = 1 head + 1 worker replica.
	g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))

	g.Expect(workload.OwnerReferences).To(HaveLen(1))
	g.Expect(workload.OwnerReferences[0].Kind).To(Equal("RayCluster"))
	g.Expect(workload.OwnerReferences[0].Name).To(Equal(rayCluster.Name))
	g.Expect(*workload.OwnerReferences[0].Controller).To(BeTrue())
	g.Expect(workload.Labels[utils.RayClusterLabelKey]).To(Equal(rayCluster.Name))

	LogWithTimestamp(test.T(), "Verifying the whole-cluster PodGroup exists")
	clusterPodGroup, err := GetPodGroupV1Beta1(test, namespace.Name, rayCluster.Name+"-cluster")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(clusterPodGroup.Spec.WorkloadRef).NotTo(BeNil())
	g.Expect(clusterPodGroup.Spec.WorkloadRef.WorkloadName).To(Equal(rayCluster.Name))
	g.Expect(clusterPodGroup.Spec.WorkloadRef.TemplateName).To(Equal("cluster"))
	g.Expect(clusterPodGroup.Spec.SchedulingPolicy.Gang).NotTo(BeNil())
	g.Expect(clusterPodGroup.Spec.SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))
	g.Expect(clusterPodGroup.OwnerReferences).To(HaveLen(1))
	g.Expect(clusterPodGroup.OwnerReferences[0].Kind).To(Equal("RayCluster"))
	g.Expect(clusterPodGroup.OwnerReferences[0].Name).To(Equal(rayCluster.Name))
	g.Expect(*clusterPodGroup.OwnerReferences[0].Controller).To(BeTrue())

	LogWithTimestamp(test.T(), "Verifying PodGroupInitiallyScheduled condition on the PodGroup")
	g.Eventually(PodGroupV1Beta1(test, namespace.Name, rayCluster.Name+"-cluster"), TestTimeoutShort).
		Should(WithTransform(func(pg *schedulingv1beta1.PodGroup) bool {
			return meta.IsStatusConditionTrue(pg.Status.Conditions, schedulingv1beta1.PodGroupInitiallyScheduled)
		}, BeTrue()))
}

func TestKubernetesWASV1Beta1_PodSchedulingGroup(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	rayClusterAC := newWASRayClusterAC("sched-group", namespace.Name).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	headPod, err := GetHeadPod(test, rayCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(headPod.Spec.SchedulerName).To(Equal(corev1.DefaultSchedulerName))
	g.Expect(headPod.Spec.SchedulingGroup).NotTo(BeNil())
	g.Expect(headPod.Spec.SchedulingGroup.PodGroupName).NotTo(BeNil())
	g.Expect(*headPod.Spec.SchedulingGroup.PodGroupName).To(Equal(rayCluster.Name + "-cluster"))

	workerPods, err := GetWorkerPods(test, rayCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(workerPods).NotTo(BeEmpty())
	for _, pod := range workerPods {
		g.Expect(pod.Spec.SchedulerName).To(Equal(corev1.DefaultSchedulerName))
		g.Expect(pod.Spec.SchedulingGroup).NotTo(BeNil())
		g.Expect(pod.Spec.SchedulingGroup.PodGroupName).NotTo(BeNil())
		g.Expect(*pod.Spec.SchedulingGroup.PodGroupName).To(Equal(rayCluster.Name + "-cluster"))
	}
}

func TestKubernetesWASV1Beta1_AutoscalingGangsFloor(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	// Autoscaling cluster with an elastic worker group (min 1, max 3). With the
	// KubernetesWASAutoscaling gate on (set by the deploy overlay), WAS gangs only
	// the floor: 1 head + 1 minReplica.
	rayClusterAC := newWASRayClusterAC("autoscale-floor", namespace.Name).
		WithSpec(rayv1ac.RayClusterSpec().
			WithEnableInTreeAutoscaling(true).
			WithRayVersion(GetRayVersion()).
			WithHeadGroupSpec(rayv1ac.HeadGroupSpec().
				WithRayStartParams(map[string]string{"dashboard-host": "0.0.0.0"}).
				WithTemplate(HeadPodTemplateApplyConfiguration())).
			WithWorkerGroupSpecs(rayv1ac.WorkerGroupSpec().
				WithReplicas(1).
				WithMinReplicas(1).
				WithMaxReplicas(3).
				WithGroupName("workers").
				WithRayStartParams(map[string]string{"num-cpus": "1"}).
				WithTemplate(WorkerPodTemplateApplyConfiguration())))

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created autoscaling RayCluster %s/%s", rayCluster.Namespace, rayCluster.Name)

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s head pod to be ready", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(StatusCondition(rayv1.HeadPodReady), MatchCondition(metav1.ConditionTrue, rayv1.HeadPodRunningAndReady)))

	// Not skipped: a Workload exists and gangs the floor (1 head + 1 minReplica).
	workload, err := GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(workload.Spec.PodGroupTemplates).To(HaveLen(1))
	g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(BeNil())
	g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))

	// Head pod is stamped with the scheduling group (autoscaling is no longer skipped).
	headPod, err := GetHeadPod(test, rayCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(headPod.Spec.SchedulerName).To(Equal(corev1.DefaultSchedulerName))
	g.Expect(headPod.Spec.SchedulingGroup).NotTo(BeNil())
	g.Expect(headPod.Spec.SchedulingGroup.PodGroupName).NotTo(BeNil())
	g.Expect(*headPod.Spec.SchedulingGroup.PodGroupName).To(Equal(rayCluster.Name + "-cluster"))

	LogWithTimestamp(test.T(), "Verifying the gang floor is scheduled (PodGroupInitiallyScheduled)")
	g.Eventually(PodGroupV1Beta1(test, namespace.Name, rayCluster.Name+"-cluster"), TestTimeoutShort).
		Should(WithTransform(func(pg *schedulingv1beta1.PodGroup) bool {
			return meta.IsStatusConditionTrue(pg.Status.Conditions, schedulingv1beta1.PodGroupInitiallyScheduled)
		}, BeTrue()))
}

func TestKubernetesWASV1Beta1_GangSchedules(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	rayClusterAC := newWASRayClusterAC("gang-sched", namespace.Name).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created RayCluster %s/%s successfully", rayCluster.Namespace, rayCluster.Name)

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready (gang scheduling)", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	allPods, err := GetAllPods(test, rayCluster)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(allPods).NotTo(BeEmpty())
	for _, pod := range allPods {
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
	}

	_, err = GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())

	LogWithTimestamp(test.T(), "Verifying PodGroupInitiallyScheduled condition on the whole-cluster PodGroup")
	g.Eventually(PodGroupV1Beta1(test, namespace.Name, rayCluster.Name+"-cluster"), TestTimeoutShort).
		Should(WithTransform(func(pg *schedulingv1beta1.PodGroup) bool {
			return meta.IsStatusConditionTrue(pg.Status.Conditions, schedulingv1beta1.PodGroupInitiallyScheduled)
		}, BeTrue()))
}

// TestKubernetesWASV1Beta1_ScaleUpPatchesInPlace verifies the relaxed-immutability
// behavior: scaling updates gang minCount in place (same UID) rather than the
// delete-and-recreate the v1alpha2 provider must do.
func TestKubernetesWASV1Beta1_ScaleUpPatchesInPlace(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	rayClusterAC := newWASRayClusterAC("scale-up", namespace.Name).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created RayCluster %s/%s successfully", rayCluster.Namespace, rayCluster.Name)

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	workload, err := GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())
	workloadUID := workload.UID
	// MinCount = 1 head + 1 worker replica.
	g.Expect(workload.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))

	podGroup, err := GetPodGroupV1Beta1(test, namespace.Name, rayCluster.Name+"-cluster")
	g.Expect(err).NotTo(HaveOccurred())
	podGroupUID := podGroup.UID

	LogWithTimestamp(test.T(), "Scaling up worker replicas from 1 to 3")
	rayClusterAC.Spec.WorkerGroupSpecs[0].WithReplicas(3).WithMinReplicas(3).WithMaxReplicas(3)
	_, err = test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready after scale-up", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(func(inner Gomega) {
		rc, err := GetRayCluster(test, namespace.Name, rayCluster.Name)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(RayClusterState(rc)).To(Equal(rayv1.Ready))
		inner.Expect(RayClusterDesiredWorkerReplicas(rc)).To(Equal(int32(3)))
	}, TestTimeoutMedium).Should(Succeed())

	LogWithTimestamp(test.T(), "Verifying Workload was patched in place with updated minCount (same UID)")
	g.Eventually(func(inner Gomega) {
		w, err := GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(w.UID).To(Equal(workloadUID), "Workload should be patched in place, not recreated")
		inner.Expect(w.Spec.PodGroupTemplates).To(HaveLen(1))
		inner.Expect(w.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang).NotTo(BeNil())
		// MinCount = 1 head + 3 worker replicas.
		inner.Expect(w.Spec.PodGroupTemplates[0].SchedulingPolicy.Gang.MinCount).To(Equal(int32(4)))
	}, TestTimeoutShort).Should(Succeed())

	LogWithTimestamp(test.T(), "Verifying PodGroup was patched in place with updated minCount (same UID)")
	g.Eventually(func(inner Gomega) {
		pg, err := GetPodGroupV1Beta1(test, namespace.Name, rayCluster.Name+"-cluster")
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(pg.UID).To(Equal(podGroupUID), "PodGroup should be patched in place, not recreated")
		inner.Expect(pg.Spec.SchedulingPolicy.Gang).NotTo(BeNil())
		inner.Expect(pg.Spec.SchedulingPolicy.Gang.MinCount).To(Equal(int32(4)))
	}, TestTimeoutShort).Should(Succeed())
}

func TestKubernetesWASV1Beta1_Idempotent(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	rayClusterAC := newWASRayClusterAC("idempotent", namespace.Name).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created RayCluster %s/%s successfully", rayCluster.Namespace, rayCluster.Name)

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	g.Eventually(WorkloadsV1Beta1(test, namespace.Name), TestTimeoutShort).Should(HaveLen(1))
	g.Eventually(PodGroupsV1Beta1(test, namespace.Name), TestTimeoutShort).Should(HaveLen(1))

	LogWithTimestamp(test.T(), "Verifying resource counts remain stable over time")
	g.Consistently(WorkloadsV1Beta1(test, namespace.Name), 10*time.Second, time.Second).Should(HaveLen(1))
	g.Consistently(PodGroupsV1Beta1(test, namespace.Name), 10*time.Second, time.Second).Should(HaveLen(1))
}

func TestKubernetesWASV1Beta1_OwnerReferenceGC(t *testing.T) {
	test := With(t)
	g := NewWithT(t)

	namespace := test.NewTestNamespace()

	rayClusterAC := newWASRayClusterAC("gc-test", namespace.Name).
		WithSpec(NewRayClusterSpec())

	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created RayCluster %s/%s successfully", rayCluster.Namespace, rayCluster.Name)

	LogWithTimestamp(test.T(), "Waiting for RayCluster %s/%s to become ready", rayCluster.Namespace, rayCluster.Name)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	_, err = GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(PodGroupsV1Beta1(test, namespace.Name), TestTimeoutShort).Should(HaveLen(1))

	LogWithTimestamp(test.T(), "Deleting RayCluster %s/%s", rayCluster.Namespace, rayCluster.Name)
	err = test.Client().Ray().RayV1().RayClusters(namespace.Name).Delete(test.Ctx(), rayCluster.Name, metav1.DeleteOptions{})
	g.Expect(err).NotTo(HaveOccurred())

	LogWithTimestamp(test.T(), "Waiting for Workload to be deleted")
	g.Eventually(func() bool {
		_, err := GetWorkloadV1Beta1(test, namespace.Name, rayCluster.Name)
		return errors.IsNotFound(err)
	}, TestTimeoutShort).Should(BeTrue())

	LogWithTimestamp(test.T(), "Waiting for PodGroups to be deleted")
	g.Eventually(PodGroupsV1Beta1(test, namespace.Name), TestTimeoutShort).Should(BeEmpty())
}
