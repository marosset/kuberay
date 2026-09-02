package support

import (
	"github.com/onsi/gomega"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	schedulingv1alpha2 "github.com/ray-project/kuberay/ray-operator/internal/scheduling/v1alpha2"
)

// The scheduling.k8s.io/v1alpha2 types were removed from k8s.io/api in v0.37
// (Kubernetes 1.37), but KubeRay still supports the functionality on 1.36
// clusters, so the types are vendored locally. Because they are no longer part
// of the typed client-go clientset, these helpers access them via the dynamic
// client and convert the results into the vendored types. The generic
// getSchedulingObject/listSchedulingObjects helpers are version-parameterized so
// v1beta1/v1alpha3 e2e tests can reuse them with their own GVR and types.
var (
	workloadGVR = SchedulingGVR("v1alpha2", "workloads")
	podGroupGVR = SchedulingGVR("v1alpha2", "podgroups")
)

// SchedulingGVR builds a GroupVersionResource in the scheduling.k8s.io group for
// the given API version and resource (e.g. "v1beta1", "workloads").
func SchedulingGVR(version, resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: schedulingv1alpha2.GroupName, Version: version, Resource: resource}
}

// getSchedulingObject fetches a namespaced scheduling.k8s.io object via the
// dynamic client and converts it into the typed T (any API version).
func getSchedulingObject[T any](t Test, gvr schema.GroupVersionResource, namespace, name string) (*T, error) {
	u, err := t.Client().Dynamic().Resource(gvr).Namespace(namespace).Get(t.Ctx(), name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	obj := new(T)
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// listSchedulingObjects lists namespaced scheduling.k8s.io objects via the
// dynamic client and converts them into typed Ts (any API version).
func listSchedulingObjects[T any](t Test, gvr schema.GroupVersionResource, namespace string) ([]T, error) {
	list, err := t.Client().Dynamic().Resource(gvr).Namespace(namespace).List(t.Ctx(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	items := make([]T, len(list.Items))
	for i := range list.Items {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &items[i]); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func Workload(t Test, namespace, name string) func() (*schedulingv1alpha2.Workload, error) {
	return func() (*schedulingv1alpha2.Workload, error) {
		return GetWorkload(t, namespace, name)
	}
}

func GetWorkload(t Test, namespace, name string) (*schedulingv1alpha2.Workload, error) {
	return getSchedulingObject[schedulingv1alpha2.Workload](t, workloadGVR, namespace, name)
}

func PodGroup(t Test, namespace, name string) func() (*schedulingv1alpha2.PodGroup, error) {
	return func() (*schedulingv1alpha2.PodGroup, error) {
		return GetPodGroup(t, namespace, name)
	}
}

func GetPodGroup(t Test, namespace, name string) (*schedulingv1alpha2.PodGroup, error) {
	return getSchedulingObject[schedulingv1alpha2.PodGroup](t, podGroupGVR, namespace, name)
}

func Workloads(t Test, namespace string) func(g gomega.Gomega) []schedulingv1alpha2.Workload {
	return func(g gomega.Gomega) []schedulingv1alpha2.Workload {
		workloads, err := listSchedulingObjects[schedulingv1alpha2.Workload](t, workloadGVR, namespace)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return workloads
	}
}

func PodGroups(t Test, namespace string) func(g gomega.Gomega) []schedulingv1alpha2.PodGroup {
	return func(g gomega.Gomega) []schedulingv1alpha2.PodGroup {
		podGroups, err := listSchedulingObjects[schedulingv1alpha2.PodGroup](t, podGroupGVR, namespace)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return podGroups
	}
}

// scheduling.k8s.io/v1beta1 accessors (Kubernetes 1.37+). These come from
// k8s.io/api directly, so they use the same generic dynamic-client helpers.
var (
	workloadV1Beta1GVR = SchedulingGVR("v1beta1", "workloads")
	podGroupV1Beta1GVR = SchedulingGVR("v1beta1", "podgroups")
)

func WorkloadV1Beta1(t Test, namespace, name string) func() (*schedulingv1beta1.Workload, error) {
	return func() (*schedulingv1beta1.Workload, error) {
		return GetWorkloadV1Beta1(t, namespace, name)
	}
}

func GetWorkloadV1Beta1(t Test, namespace, name string) (*schedulingv1beta1.Workload, error) {
	return getSchedulingObject[schedulingv1beta1.Workload](t, workloadV1Beta1GVR, namespace, name)
}

func PodGroupV1Beta1(t Test, namespace, name string) func() (*schedulingv1beta1.PodGroup, error) {
	return func() (*schedulingv1beta1.PodGroup, error) {
		return GetPodGroupV1Beta1(t, namespace, name)
	}
}

func GetPodGroupV1Beta1(t Test, namespace, name string) (*schedulingv1beta1.PodGroup, error) {
	return getSchedulingObject[schedulingv1beta1.PodGroup](t, podGroupV1Beta1GVR, namespace, name)
}

func WorkloadsV1Beta1(t Test, namespace string) func(g gomega.Gomega) []schedulingv1beta1.Workload {
	return func(g gomega.Gomega) []schedulingv1beta1.Workload {
		workloads, err := listSchedulingObjects[schedulingv1beta1.Workload](t, workloadV1Beta1GVR, namespace)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return workloads
	}
}

func PodGroupsV1Beta1(t Test, namespace string) func(g gomega.Gomega) []schedulingv1beta1.PodGroup {
	return func(g gomega.Gomega) []schedulingv1beta1.PodGroup {
		podGroups, err := listSchedulingObjects[schedulingv1beta1.PodGroup](t, podGroupV1Beta1GVR, namespace)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return podGroups
	}
}

// scheduling.k8s.io/v1alpha3 accessors (Kubernetes 1.37+). These come from
// k8s.io/api directly, so they use the same generic dynamic-client helpers.
var (
	workloadV1Alpha3GVR = SchedulingGVR("v1alpha3", "workloads")
	podGroupV1Alpha3GVR = SchedulingGVR("v1alpha3", "podgroups")
)

func WorkloadV1Alpha3(t Test, namespace, name string) func() (*schedulingv1alpha3.Workload, error) {
	return func() (*schedulingv1alpha3.Workload, error) {
		return GetWorkloadV1Alpha3(t, namespace, name)
	}
}

func GetWorkloadV1Alpha3(t Test, namespace, name string) (*schedulingv1alpha3.Workload, error) {
	return getSchedulingObject[schedulingv1alpha3.Workload](t, workloadV1Alpha3GVR, namespace, name)
}

func PodGroupV1Alpha3(t Test, namespace, name string) func() (*schedulingv1alpha3.PodGroup, error) {
	return func() (*schedulingv1alpha3.PodGroup, error) {
		return GetPodGroupV1Alpha3(t, namespace, name)
	}
}

func GetPodGroupV1Alpha3(t Test, namespace, name string) (*schedulingv1alpha3.PodGroup, error) {
	return getSchedulingObject[schedulingv1alpha3.PodGroup](t, podGroupV1Alpha3GVR, namespace, name)
}

func WorkloadsV1Alpha3(t Test, namespace string) func(g gomega.Gomega) []schedulingv1alpha3.Workload {
	return func(g gomega.Gomega) []schedulingv1alpha3.Workload {
		workloads, err := listSchedulingObjects[schedulingv1alpha3.Workload](t, workloadV1Alpha3GVR, namespace)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return workloads
	}
}

func PodGroupsV1Alpha3(t Test, namespace string) func(g gomega.Gomega) []schedulingv1alpha3.PodGroup {
	return func(g gomega.Gomega) []schedulingv1alpha3.PodGroup {
		podGroups, err := listSchedulingObjects[schedulingv1alpha3.PodGroup](t, podGroupV1Alpha3GVR, namespace)
		g.Expect(err).NotTo(gomega.HaveOccurred())
		return podGroups
	}
}
