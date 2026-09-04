# Kubernetes Workload-Aware Scheduling

This guide explains how to use KubeRay's Kubernetes Workload-Aware Scheduling (WAS). It gang schedules an entire
RayCluster — the head and all worker groups — as a single atomic unit by integrating RayCluster pods with the in-tree
Kubernetes `scheduling.k8s.io` Workload and PodGroup APIs and the default Kubernetes scheduler.

> **Scope of this guide.** Kubernetes WAS supports multiple `scheduling.k8s.io` API versions: `v1alpha2` (served by
> Kubernetes 1.36) and `v1beta1` / `v1alpha3` (served by Kubernetes 1.37+). The operator automatically uses the most
> mature version the cluster serves; see [Choose the scheduling API version](#choose-the-scheduling-api-version).

## Overview

Distributed AI/ML workloads on Kubernetes can suffer from partial scheduling. Some pods in a group get scheduled and
hold expensive nodes idle while waiting for the remaining pods, or partially scheduled groups block other workloads
indefinitely. Gang scheduling solves this by treating a group of pods as an atomic unit.

Kubernetes WAS uses the Workload and PodGroup APIs introduced by [KEP-4671][kep-4671] and [KEP-5832][kep-5832].
Unlike Volcano, YuniKorn, or other external schedulers, it keeps pods on the Kubernetes default scheduler and sets
`spec.schedulingGroup` on each pod to connect it to its PodGroup.

## Prerequisites

Kubernetes WAS needs a cluster that serves one of the `scheduling.k8s.io` API versions with the Workload/PodGroup
feature enabled on the control plane. The exact prerequisites depend on the Kubernetes version:

**Kubernetes 1.37+ (`v1beta1` / `v1alpha3`) — recommended:**

- `scheduling.k8s.io/v1beta1` and/or `scheduling.k8s.io/v1alpha3` served by the kube-apiserver (both are served by
  default on 1.37).
- `GenericWorkload=true` on the kube-apiserver, kube-controller-manager, and kube-scheduler. (There is no separate
  `GangScheduling` gate on 1.37.)
- Optional, for the preemption policy: `PodGroupPreemptionPolicy=true` on the control plane (see
  [Preemption policy](#preemption-policy)).

**Kubernetes 1.36 (`v1alpha2`):**

- `scheduling.k8s.io/v1alpha2=true` in the kube-apiserver runtime config so the alpha API is served (it is removed in
  1.37).
- `GenericWorkload=true` on the kube-apiserver and kube-controller-manager, and `GangScheduling=true` on the
  kube-scheduler.

Autoscaling RayClusters are supported on `v1beta1` / `v1alpha3` (see [Autoscaling](#autoscaling)) but not on
`v1alpha2`.

## Enable Kubernetes WAS

Kubernetes WAS is gated by the KubeRay `KubernetesWAS` feature gate (alpha, disabled by default). **Enabling the
feature gate is all that is required to turn it on** — no other operator configuration is needed.

With Helm, add the feature gate:

```yaml
# values.yaml
featureGates:
  - name: KubernetesWAS
    enabled: true
```

```bash
helm install kuberay-operator helm-chart/kuberay-operator \
  --set 'featureGates[0].name=KubernetesWAS' \
  --set 'featureGates[0].enabled=true'
```

With operator flags, pass:

```bash
--feature-gates=KubernetesWAS=true
```

Kubernetes WAS is mutually exclusive with KubeRay's external batch scheduler integrations (Volcano, YuniKorn, KAI,
scheduler-plugins); enable only one at a time.

### Choose the scheduling API version

A Kubernetes 1.37+ cluster serves more than one `scheduling.k8s.io` version (`v1beta1` and `v1alpha3`). By default the
operator uses the **most mature** version the cluster serves (`v1beta1` before `v1alpha3` before `v1alpha2`). To pin a
specific version — for example to exercise `v1alpha3` — set the target version:

```yaml
# values.yaml
kubernetesWAS:
  targetVersion: v1alpha3
```

Or with an operator flag:

```bash
--kubernetes-was-target-version=v1alpha3
```

The value must be one of `v1alpha2`, `v1beta1`, or `v1alpha3`, must be served by the cluster, and is only valid when the
`KubernetesWAS` feature gate is enabled. Leave it empty to auto-select.

### Opt in per RayCluster

While the feature gate is enabled operator-wide, gang scheduling is applied to a RayCluster only when it carries the
opt-in label:

```yaml
metadata:
  labels:
    ray.io/gang-scheduling-enabled: "true"
```

This is the same label used by KubeRay's other gang-scheduling integrations. RayClusters without the label are
scheduled normally, pod by pod.

## Behavior

Once enabled and opted in, a RayCluster is scheduled as a single gang:

- **All-or-nothing.** The head pod and every worker-group pod are scheduled together. If the cluster cannot fit in
  full, none of its pods start — they stay `Pending` until there is room for the entire cluster. This avoids partial
  startups that hold expensive nodes idle.
- **What counts toward the gang.** One head pod plus the desired replicas of every worker group. A multi-host group
  contributes `replicas × numOfHosts` pods. Suspended worker groups contribute nothing.
- **Default scheduler.** Pods are placed by the standard Kubernetes scheduler; there is no separate scheduler to
  install or run.
- **Editing and scaling.** Changing worker groups or replica counts is picked up automatically — the gang requirement
  is updated to match the new cluster shape. On `v1beta1` / `v1alpha3` a scale change is applied by patching the gang
  size **in place** (non-disruptive); on `v1alpha2` the Workload and PodGroup are deleted and recreated.
- **Autoscaling.** On `v1beta1` / `v1alpha3`, autoscaling RayClusters are gang scheduled at their **floor** (see
  [Autoscaling](#autoscaling)). On `v1alpha2` they are skipped.
- **Suspend and resume.** Suspending a RayCluster deletes its pods but keeps the Workload and PodGroup in place;
  resuming reuses them so the recreated pods rejoin the same gang.
- **Cleanup.** The scheduling resources are garbage collected automatically when the RayCluster is deleted.

You can confirm a cluster is being gang scheduled by checking that its pods carry a scheduling group:

```bash
kubectl get pods -n <namespace> -l ray.io/cluster=<raycluster-name> \
  -o custom-columns=NAME:.metadata.name,GROUP:.spec.schedulingGroup.podGroupName
```

## Autoscaling

On `v1beta1` / `v1alpha3`, RayClusters with `enableInTreeAutoscaling: true` are gang scheduled at their **floor** — one
head pod plus the sum of each worker group's `minReplicas`. This guarantees the minimum viable cluster starts
all-or-nothing, while letting the Ray autoscaler add pods above the floor without deadlocking the gang. Editing
`minReplicas` re-patches the floor in place; the autoscaler changing the current `replicas` does not move the floor.

On `v1alpha2` the gang is fully immutable, so autoscaling RayClusters are **skipped**: they are scheduled pod by pod and
any existing Workload/PodGroup for that cluster is cleaned up.

## Preemption policy

On `v1beta1` / `v1alpha3` you can request a gang preemption policy so a higher-priority RayCluster can preempt
lower-priority workloads. Set the annotation on the RayCluster:

```yaml
metadata:
  annotations:
    ray.io/kubernetes-was-preemption-policy: PreemptLowerPriority   # or "Never"
```

This requires both:

- the `KubernetesWASPodGroupPreemptionPolicy` KubeRay feature gate (alpha, off by default) enabled on the operator, and
- the cluster-side `PodGroupPreemptionPolicy` feature gate enabled on the control plane (Kubernetes 1.37+).

If either is missing, the apiserver silently drops the field and the policy has no effect; the operator logs a one-time
startup warning when the KubeRay gate is on. The policy is applied when the PodGroup is first created and is
**immutable** afterward — editing the annotation on a live RayCluster does not change it (recreate the cluster to change
the policy). `v1alpha2` does not support this.

## Per-version behavior differences

| Aspect | `v1alpha2` (Kubernetes 1.36) | `v1beta1` / `v1alpha3` (Kubernetes 1.37+) |
| --- | --- | --- |
| Rescale on edit | delete + recreate the gang | patch the gang size in place (non-disruptive) |
| Autoscaling | skipped (scheduled pod by pod) | gang the floor (head + `minReplicas`) |
| Preemption policy | not supported | opt-in via annotation (gated) |
| Scheduled condition | `PodGroupScheduled` (may revert) | `PodGroupInitiallyScheduled` (terminal once true) |

## Limitations

- The available capabilities depend on the Kubernetes version and the served `scheduling.k8s.io` API version (see
  [Per-version behavior differences](#per-version-behavior-differences)). `v1alpha2` (Kubernetes 1.36) does not support
  autoscaling or the preemption policy.
- The entire RayCluster is scheduled as one gang. Partial scheduling of a subset of worker groups is not supported; if
  the cluster cannot be scheduled in full, none of its pods are scheduled.
- `spec.schedulingGroup` on pods is immutable. If you add the opt-in label to an already-running RayCluster, existing
  pods will not get a scheduling group until they are recreated.
- On `v1beta1` / `v1alpha3` the `PodGroupInitiallyScheduled` condition is **terminal** — once the gang is first admitted
  it never reverts, so pods replaced later (head restart, worker eviction) reschedule outside the original gang
  guarantee. Do not use this condition as a live-readiness signal.
- The preemption policy is immutable after the PodGroup is created (see [Preemption policy](#preemption-policy)).

## Troubleshooting

### Pods stay Pending

If a cluster's pods never leave `Pending`, the gang cannot be placed in full. Check that the cluster fits (enough nodes
and resources for the head plus all workers at once) and inspect pod events:

```bash
kubectl describe pod <pod-name> -n <namespace>
```

If pods are gated even though there appears to be capacity, confirm the cluster meets the [prerequisites](#prerequisites)
(the `scheduling.k8s.io` API is served and the required control-plane feature gates for your Kubernetes version are
enabled).

### A running RayCluster did not start gang scheduling

The pod scheduling group is set at pod creation and is immutable. If you add the `ray.io/gang-scheduling-enabled` label
to an already-running RayCluster, existing pods are not affected — they pick up gang scheduling only when recreated.

### Autoscaling clusters

On `v1beta1` / `v1alpha3`, an autoscaling RayCluster is gang scheduled at its floor (head + `minReplicas`); the Ray
autoscaler adds further pods above the floor. On `v1alpha2`, autoscaling is unsupported — the cluster is scheduled pod by
pod and no scheduling group is set on its pods.

### The preemption policy is not taking effect

Confirm both the operator's `KubernetesWASPodGroupPreemptionPolicy` gate and the control plane's
`PodGroupPreemptionPolicy` gate are enabled, and that the cluster is on `v1beta1` / `v1alpha3` (Kubernetes 1.37+). If a
gate is missing the apiserver drops the `preemptionPolicy` field silently. Because the field is immutable, editing the
annotation on a running cluster has no effect — recreate the RayCluster.

[kep-4671]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/4671-gang-scheduling
[kep-5832]: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/5832-decouple-podgroup-api
