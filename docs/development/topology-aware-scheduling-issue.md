# [Discussion] Alpha: Kubernetes topology-aware workload scheduling for KubeRay

## Motivation

I would like to discuss an **alpha, opt-in** integration of Kubernetes topology-aware
workload scheduling ([KEP-5732][tas-kep]) with KubeRay's Kubernetes Workload-Aware
Scheduling (WAS) plugin. It would let kube-scheduler place each Ray worker group inside
one topology domain with enough capacity (for example a rack), while still admitting the
whole RayCluster as a gang.

Co-location can reduce cross-domain traffic but also limits capacity and can lengthen
Pending time. No performance claims are made until measured on representative workloads.

## Assumption

This builds on [PR #5266](https://github.com/ray-project/kuberay/pull/5266) (in-place
`minCount` resize, autoscaling floor, preemption policy) **and assumes it has merged**.

## Proposal

For a RayCluster that requests topology, create a native CompositePodGroup
([KEP-6012][composite-kep]) hierarchy:

| Group | Gang requirement | Topology |
| --- | --- | --- |
| Root CompositePodGroup | Requires every child. | None. |
| Head PodGroup | `minCount: 1`. | None. |
| One PodGroup per participating worker group | The group's gang floor. | Optional single hard key. |

The head stays in the gang but can run outside the workers' domains. Kubernetes selects
domains; KubeRay does not select nodes.

**Opt-in on both sides.** The Kubernetes cluster must run v1.37 with `GenericWorkload`,
`TopologyAwareWorkloadScheduling`, and `CompositePodGroup` enabled on kube-apiserver and
kube-scheduler. The RayCluster must carry the existing `ray.io/gang-scheduling-enabled`
label and request topology through an alpha annotation mapping worker group names to
node label keys. There is no new KubeRay feature gate, and RayClusters without a topology
request keep #5266's single-PodGroup layout unchanged.

## Deliberately Simple for Alpha

- Only leaf `gang.minCount` is updated in place (resize and autoscaling). Everything else
  (adding/removing/renaming worker groups, topology changes, PriorityClass changes) requires
  recreating the RayCluster, consistent with the #5266 review outcome.
- Head and worker-group priority/preemption values are copied from the head pod template;
  no lookups and no new RBAC beyond `compositepodgroups`.
- Worker groups with a floor of zero (scale-to-zero, suspended, zero replicas) are not in the
  gang and cannot request topology; proper support waits for a mutable `minGroupCount` in beta.
- Head plus at most seven worker groups (upstream template limit of eight).
- One topology key per worker group; no head/root topology, no per-replica grouping, no soft
  constraints, no label delivery to Ray, and no special RayJob/RayService handling.
- Fail fast, before creating pods, when the annotation is present but the WAS plugin or gang
  label is missing, `compositepodgroups` is not served, the annotation is invalid, or the
  persisted constraint was dropped. KubeRay cannot see scheduler gates or
  profiles; those are documented administrator prerequisites.

The full list of what was removed from the earlier, broader draft is in the
[proposal](topology-aware-scheduling-proposal.md).

## Questions

1. Is an annotation acceptable for the alpha API, with a typed worker-group field at graduation?
2. Is "recreate the RayCluster for structural changes" acceptable for alpha?
3. Is the seven-worker-group limit acceptable?
4. Which composite behaviors need SIG Scheduling confirmation? In particular, sibling groups are
   placed greedily without backtracking: in e2e, an unconstrained group stranded a feasible gang by
   taking capacity the constrained group needed. Is there a supported way to order or protect the
   constrained groups? Priority and preemption across the hierarchy are untested.
5. Can anyone share a representative training manifest, node topology labels, per-domain
   capacity, or scheduler configuration to validate against?

## Prototype Results

A hand-built hierarchy on a kind v1.37.0 cluster confirmed the core behavior: the head runs
outside worker domains, per-group constraints and an unconstrained group coexist, and an
infeasible group or head blocks the whole gang. It also showed that the API does not validate
hierarchy consistency, that a scheduler without the gates silently ignores stored constraints,
and that one mismatched pod priority blocks the whole gang. A KubeRay prototype on top of #5266
then built this hierarchy from the annotation on the same cluster and behaved as designed,
including the fail-fast checks and unchanged behavior for RayClusters without the annotation.
Details are in the [prototype README](topology-aware-scheduling-prototype/README.md).

## Validation

A key kind test: the head fits only an unlabeled CPU node, worker A requests rack locality,
and worker B requests zone locality but cannot fit in any one zone. Nothing (including the
head and A) should bind. After capacity for B exists in one zone, the gang is admitted with
each worker group local to its own domain. Also test a head that cannot fit, and a resize that
patches only `minCount`.

[tas-kep]: https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/5732-topology-aware-workload-scheduling/README.md
[composite-kep]: https://github.com/kubernetes/enhancements/blob/master/keps/sig-scheduling/6012-composite-podgroup-api/README.md
