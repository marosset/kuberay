#!/usr/bin/env bash
# Creates the kind cluster used by proto.py and prepares its nodes.
#
#   worker   pool=cpu, no topology labels, no slots (the head's pool)
#   worker2/3  pool=gpu, rack r1, zone z1, 2 slots each
#   worker4/5  pool=gpu, rack r2, zone z2, 2 slots each
#
# "example.com/slot" is a fake extended resource so capacity is exact and independent of the host.
set -euo pipefail
cd "$(dirname "$0")"
export KUBECONFIG="${KUBECONFIG:-/tmp/tas-proto.kubeconfig}"
P=tas-proto

kind create cluster --name "$P" --config kind-config.yaml --kubeconfig "$KUBECONFIG"
kubectl wait --for=condition=Ready nodes --all --timeout=240s

kubectl label node "$P-worker" pool=cpu
for n in worker2 worker3; do
  kubectl label node "$P-$n" pool=gpu topology.example.com/rack=r1 topology.example.com/zone=z1
done
for n in worker4 worker5; do
  kubectl label node "$P-$n" pool=gpu topology.example.com/rack=r2 topology.example.com/zone=z2
done
for n in worker2 worker3 worker4 worker5; do
  kubectl patch node "$P-$n" --subresource=status --type=json -p \
    '[{"op":"add","path":"/status/capacity/example.com~1slot","value":"2"},{"op":"add","path":"/status/allocatable/example.com~1slot","value":"2"}]' >/dev/null
done
echo "Ready. Run: KUBECONFIG=$KUBECONFIG ./proto.py all"
