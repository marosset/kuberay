package e2ekuberneteswas

import (
	"encoding/json"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	rayv1ac "github.com/ray-project/kuberay/ray-operator/pkg/client/applyconfiguration/ray/v1"
	. "github.com/ray-project/kuberay/ray-operator/test/support"
)

// These tests need a cluster with at least two nodes in different racks and zones, as created by the
// kind configs (node labels below). Capacity is controlled exactly with a fake extended resource that
// the tests add to the nodes, so results do not depend on the host's CPU or memory.

const (
	rackKey = "e2e.kuberay.io/rack"
	zoneKey = "e2e.kuberay.io/zone"

	slotResource corev1.ResourceName = "example.com/slot"
)

// requireTwoDomains returns the names of two nodes in different racks, or skips the test.
func requireTwoDomains(test Test) (first, second string) {
	test.T().Helper()
	nodes, err := test.Client().Core().CoreV1().Nodes().List(test.Ctx(), metav1.ListOptions{})
	NewWithT(test.T()).Expect(err).NotTo(HaveOccurred())
	byRack := map[string]string{}
	for _, node := range nodes.Items {
		if rack, ok := node.Labels[rackKey]; ok {
			byRack[rack] = node.Name
		}
	}
	if len(byRack) < 2 {
		test.T().Skipf("need nodes in two racks labeled %s (see the kind configs); found %d", rackKey, len(byRack))
	}
	for _, name := range byRack {
		if first == "" {
			first = name
		} else if second == "" {
			second = name
		}
	}
	return first, second
}

// setNodeSlots sets the node's capacity of the fake slot resource and removes it when the test ends.
func setNodeSlots(test Test, node string, slots int) {
	test.T().Helper()
	g := NewWithT(test.T())
	value := resource.NewQuantity(int64(slots), resource.DecimalSI).String()
	patch, err := json.Marshal([]map[string]any{
		{"op": "add", "path": "/status/capacity/example.com~1slot", "value": value},
		{"op": "add", "path": "/status/allocatable/example.com~1slot", "value": value},
	})
	g.Expect(err).NotTo(HaveOccurred())
	_, err = test.Client().Core().CoreV1().Nodes().Patch(test.Ctx(), node, types.JSONPatchType, patch, metav1.PatchOptions{}, "status")
	g.Expect(err).NotTo(HaveOccurred())
	test.T().Cleanup(func() {
		remove := []byte(`[{"op":"remove","path":"/status/capacity/example.com~1slot"},{"op":"remove","path":"/status/allocatable/example.com~1slot"}]`)
		_, _ = test.Client().Core().CoreV1().Nodes().Patch(test.Ctx(), node, types.JSONPatchType, remove, metav1.PatchOptions{}, "status")
	})
}

// slotWorkerGroupAC is a worker group whose pods each need one slot.
func slotWorkerGroupAC(name string, replicas int32) *rayv1ac.WorkerGroupSpecApplyConfiguration {
	template := WorkerPodTemplateApplyConfiguration()
	template.Spec.Containers[0].Resources.
		WithRequests(corev1.ResourceList{slotResource: resource.MustParse("1")}).
		WithLimits(corev1.ResourceList{slotResource: resource.MustParse("1")})
	return rayv1ac.WorkerGroupSpec().
		WithGroupName(name).
		WithReplicas(replicas).
		WithMinReplicas(replicas).
		WithMaxReplicas(replicas).
		WithRayStartParams(map[string]string{"num-cpus": "1"}).
		WithTemplate(template)
}

func newPlacementRayClusterAC(name, namespace string, topology map[string]string, groups ...*rayv1ac.WorkerGroupSpecApplyConfiguration) *rayv1ac.RayClusterApplyConfiguration {
	raw, err := json.Marshal(topology)
	if err != nil {
		panic(err)
	}
	return rayv1ac.RayCluster(name, namespace).
		WithLabels(map[string]string{utils.RayGangSchedulingEnabled: "true"}).
		WithAnnotations(map[string]string{utils.RayWorkerGroupTopologyAnnotation: string(raw)}).
		WithSpec(rayv1ac.RayClusterSpec().
			WithRayVersion(GetRayVersion()).
			WithHeadGroupSpec(rayv1ac.HeadGroupSpec().
				WithRayStartParams(map[string]string{"dashboard-host": "0.0.0.0"}).
				WithTemplate(HeadPodTemplateApplyConfiguration())).
			WithWorkerGroupSpecs(groups...))
}

// groupDomains returns the set of values of the node label key over the group's scheduled pods, and
// the set of nodes they run on.
func groupDomains(test Test, g Gomega, rayCluster *rayv1.RayCluster, group, key string) (domains, nodes map[string]bool) {
	test.T().Helper()
	pods, err := GetGroupPods(test, rayCluster, group)
	g.Expect(err).NotTo(HaveOccurred())
	domains, nodes = map[string]bool{}, map[string]bool{}
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		node, err := test.Client().Core().CoreV1().Nodes().Get(test.Ctx(), pod.Spec.NodeName, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		domains[node.Labels[key]] = true
		nodes[pod.Spec.NodeName] = true
	}
	return domains, nodes
}

func scheduledPodCount(test Test, g Gomega, rayCluster *rayv1.RayCluster) (scheduled, total int) {
	pods, err := GetAllPods(test, rayCluster)
	g.Expect(err).NotTo(HaveOccurred())
	for _, pod := range pods {
		if pod.Spec.NodeName != "" {
			scheduled++
		}
	}
	return scheduled, len(pods)
}

// TestKubernetesWASTopology_PlacesEachGroupInItsOwnDomain verifies that each topology-constrained group
// is placed entirely within one domain, and that different groups can use different domains. Each of
// the two nodes has two slots, so each two-pod group needs a whole node and the groups must land in
// different racks and zones.
func TestKubernetesWASTopology_PlacesEachGroupInItsOwnDomain(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()
	first, second := requireTwoDomains(test)
	setNodeSlots(test, first, 2)
	setNodeSlots(test, second, 2)

	rayClusterAC := newPlacementRayClusterAC("topo-place", namespace.Name,
		map[string]string{"training": rackKey, "data": zoneKey},
		slotWorkerGroupAC("training", 2), slotWorkerGroupAC("data", 2))
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))

	trainingRacks, trainingNodes := groupDomains(test, g, rayCluster, "training", rackKey)
	dataZones, dataNodes := groupDomains(test, g, rayCluster, "data", zoneKey)
	LogWithTimestamp(test.T(), "training racks %v nodes %v; data zones %v nodes %v", trainingRacks, trainingNodes, dataZones, dataNodes)
	g.Expect(trainingRacks).To(HaveLen(1), "all training pods must share one rack")
	g.Expect(dataZones).To(HaveLen(1), "all data pods must share one zone")
	g.Expect(trainingNodes).To(HaveLen(1))
	g.Expect(dataNodes).To(HaveLen(1))
	for node := range trainingNodes {
		g.Expect(dataNodes).NotTo(HaveKey(node), "capacity forces the groups into different domains")
	}
}

// TestKubernetesWASTopology_GroupThatFitsNoDomainBlocksTheGang verifies that a group needing more
// capacity than any single domain holds blocks the whole RayCluster, even though the free capacity of
// the cluster as a whole would be enough, and that the gang is admitted once one domain has room.
func TestKubernetesWASTopology_GroupThatFitsNoDomainBlocksTheGang(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()
	first, second := requireTwoDomains(test)
	setNodeSlots(test, first, 2)
	setNodeSlots(test, second, 2)

	// Three pods must share a rack, but each rack has two slots (four in total). The unconstrained
	// "plain" group needs no slots: the scheduler places sibling groups one after another without
	// backtracking, so a sibling that took slots in the only rack that fits the training group would
	// strand the gang and make this test order dependent.
	rayClusterAC := newPlacementRayClusterAC("topo-block", namespace.Name,
		map[string]string{"training": rackKey},
		slotWorkerGroupAC("training", 3), workerGroupAC("plain", 1))
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())

	// 1 head + 3 training + 1 plain.
	const expectedPods = 5
	g.Eventually(func() int { _, total := scheduledPodCount(test, g, rayCluster); return total }, TestTimeoutMedium).
		Should(Equal(expectedPods))
	LogWithTimestamp(test.T(), "Verifying nothing binds while no rack can hold the training group")
	g.Consistently(func(inner Gomega) {
		scheduled, total := scheduledPodCount(test, inner, rayCluster)
		inner.Expect(total).To(Equal(expectedPods))
		inner.Expect(scheduled).To(BeZero(), "no pod, including the head and the unconstrained group, may be scheduled")
	}, 20*time.Second, 2*time.Second).Should(Succeed())

	LogWithTimestamp(test.T(), "Giving one rack room for the whole training group")
	setNodeSlots(test, first, 3)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutLong).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))
	racks, nodes := groupDomains(test, g, rayCluster, "training", rackKey)
	g.Expect(racks).To(HaveLen(1))
	g.Expect(nodes).To(HaveLen(1))
	g.Expect(nodes).To(HaveKey(first))
}

// TestKubernetesWASTopology_GrowthStaysInTheDomain verifies that a pod added to an already admitted
// group joins that group's domain: when the domain has no room the pod stays Pending although the other
// rack has capacity, and it is scheduled into the same rack once room appears.
func TestKubernetesWASTopology_GrowthStaysInTheDomain(t *testing.T) {
	test := With(t)
	g := NewWithT(t)
	namespace := test.NewTestNamespace()
	first, second := requireTwoDomains(test)
	setNodeSlots(test, first, 2)
	setNodeSlots(test, second, 2)

	rayClusterAC := newPlacementRayClusterAC("topo-grow", namespace.Name,
		map[string]string{"training": rackKey}, slotWorkerGroupAC("training", 2))
	rayCluster, err := test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutMedium).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))
	_, nodes := groupDomains(test, g, rayCluster, "training", rackKey)
	g.Expect(nodes).To(HaveLen(1))
	var home string
	for node := range nodes {
		home = node
	}

	LogWithTimestamp(test.T(), "Growing the training group while its node is full and the other rack is free")
	rayClusterAC.Spec.WorkerGroupSpecs[0].WithReplicas(3).WithMinReplicas(3).WithMaxReplicas(3)
	_, err = test.Client().Ray().RayV1().RayClusters(namespace.Name).Apply(test.Ctx(), rayClusterAC, TestApplyOptions)
	g.Expect(err).NotTo(HaveOccurred())
	g.Eventually(func() int { _, total := scheduledPodCount(test, g, rayCluster); return total }, TestTimeoutMedium).
		Should(Equal(4))
	g.Consistently(func(inner Gomega) {
		scheduled, total := scheduledPodCount(test, inner, rayCluster)
		inner.Expect(total).To(Equal(4))
		inner.Expect(scheduled).To(Equal(3), "the new pod must stay Pending instead of moving to the other rack")
	}, 20*time.Second, 2*time.Second).Should(Succeed())

	LogWithTimestamp(test.T(), "Giving the home rack room for the new pod")
	setNodeSlots(test, home, 3)
	g.Eventually(RayCluster(test, namespace.Name, rayCluster.Name), TestTimeoutLong).
		Should(WithTransform(RayClusterState, Equal(rayv1.Ready)))
	racks, nodesAfter := groupDomains(test, g, rayCluster, "training", rackKey)
	g.Expect(racks).To(HaveLen(1))
	g.Expect(nodesAfter).To(HaveLen(1))
	g.Expect(nodesAfter).To(HaveKey(home))
}
