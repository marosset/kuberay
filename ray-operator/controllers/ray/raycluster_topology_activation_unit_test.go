package ray

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientFake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ray-project/kuberay/ray-operator/apis/config/v1alpha1"
	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/batchscheduler"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	"github.com/ray-project/kuberay/ray-operator/pkg/features"
)

func TestValidateTopologyActivation(t *testing.T) {
	newCluster := func(annotated, gangLabel bool) *rayv1.RayCluster {
		cluster := &rayv1.RayCluster{ObjectMeta: metav1.ObjectMeta{Name: "rc", Namespace: "ns", Labels: map[string]string{}, Annotations: map[string]string{}}}
		if annotated {
			cluster.Annotations[utils.RayWorkerGroupTopologyAnnotation] = `{"g":"topology.example.com/rack"}`
		}
		if gangLabel {
			cluster.Labels[utils.RayGangSchedulingEnabled] = "true"
		}
		return cluster
	}
	newManager := func(t *testing.T, wasEnabled bool) *batchscheduler.SchedulerManager {
		features.SetFeatureGateDuringTest(t, features.KubernetesWAS, wasEnabled)
		manager, err := batchscheduler.NewSchedulerManager(context.Background(), v1alpha1.Configuration{}, nil, clientFake.NewClientBuilder().Build())
		require.NoError(t, err)
		return manager
	}

	t.Run("no annotation is never checked", func(t *testing.T) {
		r := &RayClusterReconciler{}
		require.NoError(t, r.validateTopologyActivation(newCluster(false, false)))
	})
	t.Run("no batch scheduler manager", func(t *testing.T) {
		r := &RayClusterReconciler{}
		require.ErrorContains(t, r.validateTopologyActivation(newCluster(true, true)), "KubernetesWAS")
	})
	t.Run("default scheduler", func(t *testing.T) {
		r := &RayClusterReconciler{options: RayClusterReconcilerOptions{BatchSchedulerManager: newManager(t, false)}}
		require.ErrorContains(t, r.validateTopologyActivation(newCluster(true, true)), "KubernetesWAS")
	})
	t.Run("WAS enabled but gang label missing", func(t *testing.T) {
		r := &RayClusterReconciler{options: RayClusterReconcilerOptions{BatchSchedulerManager: newManager(t, true)}}
		require.ErrorContains(t, r.validateTopologyActivation(newCluster(true, false)), utils.RayGangSchedulingEnabled)
	})
	t.Run("WAS enabled and opted in", func(t *testing.T) {
		r := &RayClusterReconciler{options: RayClusterReconcilerOptions{BatchSchedulerManager: newManager(t, true)}}
		require.NoError(t, r.validateTopologyActivation(newCluster(true, true)))
	})
}
