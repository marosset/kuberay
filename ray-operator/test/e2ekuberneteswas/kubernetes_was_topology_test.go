package e2ekuberneteswas

import (
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	rayv1ac "github.com/ray-project/kuberay/ray-operator/pkg/client/applyconfiguration/ray/v1"
	. "github.com/ray-project/kuberay/ray-operator/test/support"
)

// The topology tests need the TopologyAwareWorkloadScheduling and CompositePodGroup feature gates
// (alpha in Kubernetes 1.37) on kube-apiserver and kube-scheduler; the kind configs enable them.
// The tests in this file work on any cluster: they label every node with one domain, so they check the
// layout, routing, lifecycle, and enforcement (a topology key that no node carries makes the whole gang
// unschedulable). Placement across several domains is in kubernetes_was_topology_placement_test.go.

const (
	// topologyKey is a node label key the tests add to every node, putting all nodes in one domain.
	topologyKey = "e2e.kuberay.io/all-nodes"
	// unlabeledTopologyKey is a key that no node carries until a test adds it.
	unlabeledTopologyKey = "e2e.kuberay.io/missing-rack"
)

// podGroupInitiallyScheduled reports the PodGroupInitiallyScheduled condition of a leaf PodGroup.
// In Kubernetes 1.37 the root CompositePodGroup's status stays empty, so admission is observed on
// the leaves.
func podGroupInitiallyScheduled(pg *schedulingv1alpha3.PodGroup) bool {
	return meta.IsStatusConditionTrue(pg.Status.Conditions, schedulingv1alpha3.PodGroupInitiallyScheduled)
}

// labelNodes adds key=value to every node and removes it when the test ends.
func labelNodes(test Test, key, value string) {
	test.T().Helper()
	g := NewWithT(test.T())
	nodes, err := test.Client().Core().CoreV1().Nodes().List(test.Ctx(), metav1.ListOptions{})
	g.Expect(err).NotTo(HaveOccurred())
	patch := func(v any) []byte {
		b, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": map[string]any{key: v}}})
		g.Expect(err).NotTo(HaveOccurred())
		return b
	}
	for _, node := range nodes.Items {
		_, err := test.Client().Core().CoreV1().Nodes().Patch(test.Ctx(), node.Name, types.MergePatchType, patch(value), metav1.PatchOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		name := node.Name
		test.T().Cleanup(func() {
			_, _ = test.Client().Core().CoreV1().Nodes().Patch(test.Ctx(), name, types.MergePatchType, patch(nil), metav1.PatchOptions{})
		})
	}
}

func workerGroupAC(name string, replicas int32) *rayv1ac.WorkerGroupSpecApplyConfiguration {
	return rayv1ac.WorkerGroupSpec().
		WithGroupName(name).
		WithReplicas(replicas).
		WithMinReplicas(replicas).
		WithMaxReplicas(replicas).
		WithRayStartParams(map[string]string{"num-cpus": "1"}).
		WithTemplate(WorkerPodTemplateApplyConfiguration())
}

// newTopologyRayClusterAC builds a RayCluster with worker groups "training" (topology) and "plain".
// gang=false omits the gang label and a nil topology omits the annotation.
func newTopologyRayClusterAC(name, namespace string, gang bool, topology map[string]string, training, plain int32) *rayv1ac.RayClusterApplyConfiguration {
	ac := rayv1ac.RayCluster(name, namespace).
		WithSpec(rayv1ac.RayClusterSpec().
			WithRayVersion(GetRayVersion()).
			WithHeadGroupSpec(rayv1ac.HeadGroupSpec().
				WithRayStartParams(map[string]string{"dashboard-host": "0.0.0.0"}).
				WithTemplate(HeadPodTemplateApplyConfiguration())).
			WithWorkerGroupSpecs(workerGroupAC("training", training), workerGroupAC("plain", plain)))
	if gang {
		ac = ac.WithLabels(map[string]string{utils.RayGangSchedulingEnabled: "true"})
	}
	if topology != nil {
		raw, err := json.Marshal(topology)
		if err != nil {
			panic(err)
		}
		ac = ac.WithAnnotations(map[string]string{utils.RayWorkerGroupTopologyAnnotation: string(raw)})
	}
	return ac
}

func leafTemplates(w *schedulingv1alpha3.Workload) map[string]schedulingv1alpha3.PodGroupTemplate {
	templates := map[string]schedulingv1alpha3.PodGroupTemplate{}
	for _, t := range w.Spec.CompositePodGroupTemplates[0].PodGroupTemplates {
		templates[t.Name] = t
	}
	return templates
}

func topologyKeyOf(c *schedulingv1alpha3.PodGroupSchedulingConstraints) string {
	if c == nil || len(c.Topology) == 0 {
		return ""
	}
	return c.Topology[0].Key
}

// expectNoSchedulingObjects asserts the operator creates no scheduling objects and no pods.
func expectNoSchedulingObjects(test Test, g *WithT, rayCluster *rayv1.RayCluster) {
	g.Consistently(func(inner Gomega) {
		inner.Expect(Workloads(test, rayCluster.Namespace)(inner)).To(BeEmpty())
		inner.Expect(CompositePodGroups(test, rayCluster.Namespace)(inner)).To(BeEmpty())
		inner.Expect(PodGroups(test, rayCluster.Namespace)(inner)).To(BeEmpty())
		pods, err := GetAllPods(test, rayCluster)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(pods).To(BeEmpty())
	}, 15*time.Second, 2*time.Second).Should(Succeed())
}

// TestKubernetesWASTopology_CreatesHierarchyAndCleansUp verifies that a topology request produces a
// Workload with a root CompositePodGroup and one PodGroup per member, that every pod joins its own
// PodGroup, and that deleting the RayCluster removes everything.
func TestKubernetesWASTopology_CreatesHierarchyAndCleansUp(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()
	labelNodes(test, topologyKey, "rack-1")

	rayClusterAC := newTopologyRayClusterAC("topo", namespace.Name, true, map[string]string{"training": topologyKey}, 2, 1)
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	LogWithTimestamp(test.T(), "Created RayCluster %s/%s", rayCluster.Namespace, rayCluster.Name)

	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	LogWithTimestamp(test.T(), "Verifying the Workload layout")
	workload, err := GetWorkload(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(workload.Spec.PodGroupTemplates).To(BeEmpty(), "a topology Workload has no flat PodGroup template")
	g.Expect(workload.Spec.CompositePodGroupTemplates).To(HaveLen(1))
	root := workload.Spec.CompositePodGroupTemplates[0]
	g.Expect(root.Name).To(Equal("cluster"))
	g.Expect(root.SchedulingPolicy.Gang).NotTo(BeNil())
	g.Expect(root.SchedulingPolicy.Gang.MinGroupCount).To(Equal(int32(3)), "head + 2 worker groups")
	g.Expect(root.SchedulingConstraints).To(BeNil(), "the root must not carry topology")
	templates := leafTemplates(workload)
	g.Expect(templates).To(HaveLen(3))
	g.Expect(templates["head"].SchedulingPolicy.Gang.MinCount).To(Equal(int32(1)))
	g.Expect(topologyKeyOf(templates["head"].SchedulingConstraints)).To(BeEmpty(), "the head must not carry topology")
	g.Expect(templates["wg-training"].SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))
	g.Expect(topologyKeyOf(templates["wg-training"].SchedulingConstraints)).To(Equal(topologyKey))
	g.Expect(templates["wg-plain"].SchedulingPolicy.Gang.MinCount).To(Equal(int32(1)))
	g.Expect(topologyKeyOf(templates["wg-plain"].SchedulingConstraints)).To(BeEmpty())

	LogWithTimestamp(test.T(), "Verifying the root CompositePodGroup and leaf PodGroups")
	rootGroup, err := GetCompositePodGroup(test, namespace.Name, rayCluster.Name+"-cluster")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rootGroup.Spec.SchedulingPolicy.Gang.MinGroupCount).To(Equal(int32(3)))
	g.Expect(rootGroup.Spec.WorkloadRef.WorkloadName).To(Equal(rayCluster.Name))
	g.Expect(rootGroup.Spec.WorkloadRef.TemplateName).To(Equal("cluster"))
	g.Expect(rootGroup.OwnerReferences).To(HaveLen(1))
	g.Expect(rootGroup.OwnerReferences[0].Kind).To(Equal("RayCluster"))

	for leaf, wantKey := range map[string]string{"head": "", "wg-training": topologyKey, "wg-plain": ""} {
		podGroup, err := GetPodGroup(test, namespace.Name, rayCluster.Name+"-"+leaf)
		g.Expect(err).NotTo(HaveOccurred(), leaf)
		g.Expect(podGroup.Spec.ParentCompositePodGroupName).NotTo(BeNil(), leaf)
		g.Expect(*podGroup.Spec.ParentCompositePodGroupName).To(Equal(rootGroup.Name), leaf)
		g.Expect(podGroup.Spec.WorkloadRef.TemplateName).To(Equal(leaf), leaf)
		g.Expect(topologyKeyOf(podGroup.Spec.SchedulingConstraints)).To(Equal(wantKey), leaf)
		g.Expect(podGroup.OwnerReferences).To(HaveLen(1), leaf)
		g.Eventually(PodGroup(test, namespace.Name, podGroup.Name), TestTimeoutShort).
			Should(WithTransform(podGroupInitiallyScheduled, BeTrue()), leaf)
	}

	LogWithTimestamp(test.T(), "Verifying every pod joins the PodGroup of its own group")
	for group, want := range map[string]string{
		utils.RayNodeHeadGroupLabelValue: rayCluster.Name + "-head",
		"training":                       rayCluster.Name + "-wg-training",
		"plain":                          rayCluster.Name + "-wg-plain",
	} {
		pods, err := GetGroupPods(test, rayCluster, group)
		g.Expect(err).NotTo(HaveOccurred(), group)
		g.Expect(pods).NotTo(BeEmpty(), group)
		for _, pod := range pods {
			g.Expect(pod.Spec.SchedulingGroup).NotTo(BeNil(), pod.Name)
			g.Expect(*pod.Spec.SchedulingGroup.PodGroupName).To(Equal(want), pod.Name)
		}
	}

	LogWithTimestamp(test.T(), "Deleting the RayCluster")
	g.Expect(test.Client().Ray().RayV1().RayClusters(namespace.Name).Delete(test.Ctx(), rayCluster.Name, metav1.DeleteOptions{})).To(Succeed())
	g.Eventually(func() bool {
		_, err := GetWorkload(test, namespace.Name, rayCluster.Name)
		return errors.IsNotFound(err)
	}, TestTimeoutShort).Should(BeTrue())
	g.Eventually(CompositePodGroups(test, namespace.Name), TestTimeoutShort).Should(BeEmpty())
	g.Eventually(PodGroups(test, namespace.Name), TestTimeoutShort).Should(BeEmpty())
}

// TestKubernetesWASTopology_UnsatisfiableTopologyHoldsWholeGang verifies that the scheduler enforces
// the topology constraint: while no node carries the requested key, no pod in the cluster binds, not
// even the head or the group without topology. Once a node carries the key the gang is admitted.
func TestKubernetesWASTopology_UnsatisfiableTopologyHoldsWholeGang(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()

	rayClusterAC := newTopologyRayClusterAC("topo-hold", namespace.Name, true, map[string]string{"training": unlabeledTopologyKey}, 1, 1)
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	// 1 head + 1 training + 1 plain.
	const expectedPods = 3
	g.Eventually(func() ([]corev1.Pod, error) { return GetAllPods(test, rayCluster) }, TestTimeoutMedium).
		Should(HaveLen(expectedPods))

	LogWithTimestamp(test.T(), "Verifying no pod binds while no node carries the topology key")
	g.Consistently(func(inner Gomega) {
		pods, err := GetAllPods(test, rayCluster)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(pods).To(HaveLen(expectedPods))
		for _, pod := range pods {
			inner.Expect(pod.Spec.NodeName).To(BeEmpty(), "pod %s must not be scheduled", pod.Name)
		}
	}, 20*time.Second, 2*time.Second).Should(Succeed())
	for _, leaf := range []string{"head", "wg-training", "wg-plain"} {
		g.Consistently(PodGroup(test, namespace.Name, rayCluster.Name+"-"+leaf), 6*time.Second, 2*time.Second).
			Should(WithTransform(podGroupInitiallyScheduled, BeFalse()), leaf)
	}

	LogWithTimestamp(test.T(), "Labeling the nodes so the constraint can be satisfied")
	labelNodes(test, unlabeledTopologyKey, "rack-1")

	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutLong).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))
}

// TestKubernetesWASTopology_ResizePatchesLeafInPlace verifies that scaling a worker group patches its
// leaf minCount in the Workload template and the PodGroup in place, leaving the root and siblings alone.
func TestKubernetesWASTopology_ResizePatchesLeafInPlace(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()
	labelNodes(test, topologyKey, "rack-1")

	rayClusterAC := newTopologyRayClusterAC("topo-resize", namespace.Name, true, map[string]string{"training": topologyKey}, 1, 1)
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	workload, err := GetWorkload(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())
	trainingGroup, err := GetPodGroup(test, namespace.Name, rayCluster.Name+"-wg-training")
	g.Expect(err).NotTo(HaveOccurred())
	rootGroup, err := GetCompositePodGroup(test, namespace.Name, rayCluster.Name+"-cluster")
	g.Expect(err).NotTo(HaveOccurred())

	LogWithTimestamp(test.T(), "Scaling the training group from 1 to 2")
	rayClusterAC.Spec.WorkerGroupSpecs[0].WithReplicas(2).WithMinReplicas(2).WithMaxReplicas(2)
	_, err = test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	g.Eventually(func(inner Gomega) {
		w, err := GetWorkload(test, namespace.Name, rayCluster.Name)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(w.UID).To(Equal(workload.UID), "Workload must be patched in place")
		templates := leafTemplates(w)
		inner.Expect(templates["wg-training"].SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))
		inner.Expect(templates["wg-plain"].SchedulingPolicy.Gang.MinCount).To(Equal(int32(1)))
		inner.Expect(topologyKeyOf(templates["wg-training"].SchedulingConstraints)).To(Equal(topologyKey))
	}, TestTimeoutShort).Should(Succeed())
	g.Eventually(func(inner Gomega) {
		pg, err := GetPodGroup(test, namespace.Name, rayCluster.Name+"-wg-training")
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(pg.UID).To(Equal(trainingGroup.UID), "PodGroup must be patched in place")
		inner.Expect(pg.Spec.SchedulingPolicy.Gang.MinCount).To(Equal(int32(2)))
	}, TestTimeoutShort).Should(Succeed())

	root, err := GetCompositePodGroup(test, namespace.Name, rayCluster.Name+"-cluster")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(root.UID).To(Equal(rootGroup.UID))
	g.Expect(root.Spec.SchedulingPolicy.Gang.MinGroupCount).To(Equal(int32(3)))

	g.Eventually(func(inner Gomega) {
		rc, err := GetRayCluster(test, namespace.Name, rayCluster.Name)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(RayClusterState(rc)).To(Equal(rayv1.Ready))
		inner.Expect(RayClusterDesiredWorkerReplicas(rc)).To(Equal(int32(3)))
	}, TestTimeoutMedium).Should(Succeed())
}

// TestKubernetesWASTopology_StructuralChangeIsRejected verifies that changing the topology request
// after creation does not delete, recreate, or modify the scheduling objects.
func TestKubernetesWASTopology_StructuralChangeIsRejected(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()
	labelNodes(test, topologyKey, "rack-1")

	rayClusterAC := newTopologyRayClusterAC("topo-change", namespace.Name, true, map[string]string{"training": topologyKey}, 1, 1)
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))
	workload, err := GetWorkload(test, namespace.Name, rayCluster.Name)
	g.Expect(err).NotTo(HaveOccurred())

	LogWithTimestamp(test.T(), "Changing the topology request of the training group")
	changed := newTopologyRayClusterAC("topo-change", namespace.Name, true, map[string]string{"training": unlabeledTopologyKey}, 1, 1)
	_, err = test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), changed, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	g.Consistently(func(inner Gomega) {
		w, err := GetWorkload(test, namespace.Name, rayCluster.Name)
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(w.UID).To(Equal(workload.UID), "scheduling objects must not be recreated")
		inner.Expect(topologyKeyOf(leafTemplates(w)["wg-training"].SchedulingConstraints)).To(Equal(topologyKey))
		pg, err := GetPodGroup(test, namespace.Name, rayCluster.Name+"-wg-training")
		inner.Expect(err).NotTo(HaveOccurred())
		inner.Expect(topologyKeyOf(pg.Spec.SchedulingConstraints)).To(Equal(topologyKey))
	}, 20*time.Second, 2*time.Second).Should(Succeed())
}

// TestKubernetesWASTopology_RequestWithoutGangLabelCreatesNothing verifies the fail-fast check: a
// topology request on a RayCluster that is not opted in to gang scheduling must not fall back to
// ordinary scheduling, so the operator creates neither scheduling objects nor pods.
func TestKubernetesWASTopology_RequestWithoutGangLabelCreatesNothing(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()

	rayClusterAC := newTopologyRayClusterAC("topo-nogang", namespace.Name, false, map[string]string{"training": topologyKey}, 1, 1)
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	expectNoSchedulingObjects(test, g, rayCluster)
}

// TestKubernetesWASTopology_InvalidRequestCreatesNothing verifies that an invalid request (a group
// that does not exist) is rejected before any scheduling object or pod is created.
func TestKubernetesWASTopology_InvalidRequestCreatesNothing(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()

	rayClusterAC := newTopologyRayClusterAC("topo-invalid", namespace.Name, true, map[string]string{"no-such-group": topologyKey}, 1, 1)
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	expectNoSchedulingObjects(test, g, rayCluster)
}
