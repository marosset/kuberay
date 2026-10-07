#!/usr/bin/env bash
# Shows what KubeRay can and cannot detect when the Kubernetes prerequisites are missing.
#  1. kube-apiserver gates off: composite API missing from discovery, topology silently dropped.
#  2. kube-scheduler gates off: everything is persisted, but topology is not enforced.
set -euo pipefail
cd "$(dirname "$0")"

echo "== 1. apiserver without CompositePodGroup/TopologyAwareWorkloadScheduling"
kind create cluster --name tas-api-off --config kind-config-apiserver-gate-off.yaml --kubeconfig /tmp/tas-api-off.kubeconfig
export KUBECONFIG=/tmp/tas-api-off.kubeconfig
kubectl wait --for=condition=Ready nodes --all --timeout=240s
kubectl api-resources --api-group=scheduling.k8s.io
cat <<'EOF' | kubectl apply -f -
apiVersion: scheduling.k8s.io/v1alpha3
kind: PodGroup
metadata: {name: flat-with-topology, namespace: default}
spec:
  schedulingPolicy: {gang: {minCount: 2}}
  schedulingConstraints: {topology: [{key: topology.example.com/rack}]}
EOF
echo "persisted spec (note: no schedulingConstraints):"
kubectl get podgroup flat-with-topology -o jsonpath='{.spec}{"\n"}'

echo "== 2. scheduler without CompositePodGroup/TopologyAwareWorkloadScheduling"
kind create cluster --name tas-sched-off --config kind-config-scheduler-gate-off.yaml --kubeconfig /tmp/tas-sched-off.kubeconfig
export KUBECONFIG=/tmp/tas-sched-off.kubeconfig
kubectl wait --for=condition=Ready nodes --all --timeout=240s
P=tas-sched-off
kubectl label node $P-worker pool=gpu topology.example.com/rack=r1
kubectl label node $P-worker2 pool=gpu topology.example.com/rack=r2
for n in worker worker2; do
  kubectl patch node $P-$n --subresource=status --type=json -p \
    '[{"op":"add","path":"/status/capacity/example.com~1slot","value":"1"},{"op":"add","path":"/status/allocatable/example.com~1slot","value":"1"}]' >/dev/null
done
kubectl create ns probe
cat <<'EOF' | kubectl apply -f -
apiVersion: scheduling.k8s.io/v1alpha3
kind: Workload
metadata: {name: rc, namespace: probe}
spec:
  compositePodGroupTemplates:
  - name: cluster
    schedulingPolicy: {gang: {minGroupCount: 1}}
    podGroupTemplates:
    - name: wg-a
      schedulingPolicy: {gang: {minCount: 2}}
      schedulingConstraints: {topology: [{key: topology.example.com/rack}]}
---
apiVersion: scheduling.k8s.io/v1alpha3
kind: CompositePodGroup
metadata: {name: rc-cluster, namespace: probe}
spec:
  workloadRef: {workloadName: rc, templateName: cluster}
  schedulingPolicy: {gang: {minGroupCount: 1}}
---
apiVersion: scheduling.k8s.io/v1alpha3
kind: PodGroup
metadata: {name: rc-wg-a, namespace: probe}
spec:
  parentCompositePodGroupName: rc-cluster
  workloadRef: {workloadName: rc, templateName: wg-a}
  schedulingPolicy: {gang: {minCount: 2}}
  schedulingConstraints: {topology: [{key: topology.example.com/rack}]}
EOF
# Each node has one slot, so the two pods can only run in different racks.
for i in 0 1; do
  kubectl -n probe apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata: {name: a-$i}
spec:
  schedulingGroup: {podGroupName: rc-wg-a}
  nodeSelector: {pool: gpu}
  containers: [{name: c, image: registry.k8s.io/pause:3.10, imagePullPolicy: IfNotPresent, resources: {requests: {example.com/slot: "1"}, limits: {example.com/slot: "1"}}}]
EOF
done
sleep 25
echo "pods ran in different racks although the leaf requires one (constraint persisted, not enforced):"
kubectl -n probe get pods -o wide
