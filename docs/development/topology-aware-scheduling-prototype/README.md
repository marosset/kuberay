# Prototype: Workload / CompositePodGroup hierarchy on Kubernetes v1.37

This validates the [proposal](../topology-aware-scheduling-proposal.md) in two layers:

1. **Kubernetes contract** (`proto.py`): hand-built objects, observing the real kube-scheduler.
2. **KubeRay** (`operator_scenarios.py`): the operator, modified as proposed, builds the same
   hierarchy from a RayCluster annotation. The code is on branch `tas-prototype` (uncommitted, on
   top of [#5266](https://github.com/ray-project/kuberay/pull/5266)): the new
   `kubernetes-was/v1alpha3/topology.go` and its tests, changes to the WAS provider, a fail-fast
   check in `raycluster_controller.go`, and `compositepodgroups` RBAC in Helm and the kustomize overlay.

Run on `kindest/node:v1.37.0` (2026-10-05) with `GenericWorkload`, `TopologyAwareWorkloadScheduling`,
and `CompositePodGroup` enabled on kube-apiserver, kube-controller-manager, and kube-scheduler.

## Files

| File | Purpose |
| --- | --- |
| [kind-config.yaml](kind-config.yaml) | One control plane plus five workers, gates enabled. |
| [setup.sh](setup.sh) | Creates the cluster, labels nodes, and adds an exact fake `example.com/slot` capacity. |
| [proto.py](proto.py) | Scenarios `t1`..`t9`; `./proto.py all` or `./proto.py t2 t5`. |
| [probe-misconfig.sh](probe-misconfig.sh) | Two extra clusters with the gates missing from the apiserver or only from the scheduler. |
| [operator_scenarios.py](operator_scenarios.py) | Scenarios `o1`..`o8` that run the modified operator; see its docstring for the build and deploy steps. |

Topology: `worker` is an unlabeled CPU node (the head). `worker2/3` are rack `r1`, zone `z1`;
`worker4/5` are rack `r2`, zone `z2`; each GPU node has two slots (a rack or zone holds four).

```bash
./setup.sh
KUBECONFIG=/tmp/tas-proto.kubeconfig ./proto.py all
```

## Results

| # | Scenario | Result |
| --- | --- | --- |
| t1 | Head on the unlabeled CPU node; A (2 pods) rack-local; B (3 pods) zone-local; C (1 pod) unconstrained. | All admitted. A is in one rack, B in one zone (a different one), head on the CPU node. Mixed constrained/unconstrained leaves work. |
| t2 | B needs 5 pods in one zone; each zone holds 4; 8 slots free in total. | **Nothing binds**, including the head and the feasible A. After a zone gets enough room, everything is admitted without intervention, but only after tens of seconds (~70 s observed). |
| t3 | Head cannot be scheduled; both worker groups could fit. | **Nothing binds.** The head blocks the whole gang. |
| t4 | Pods created while the root and then a sibling leaf are missing. | Nothing binds until the whole hierarchy exists; leaves do not schedule as independent gangs. |
| t5 | Add pods above the leaf's `minCount` after admission. | They land in the group's existing rack. A pod beyond that rack's capacity stays Pending even though the other rack is free. |
| t6 | API validation (details below). | See below. |
| t7 | Ownership and cleanup, with a ConfigMap standing in for the RayCluster. | Workload and CompositePodGroup have no finalizers; leaf PodGroups have `scheduling.k8s.io/podgroup-protection`. Deleting the owner garbage-collects everything, including pods. |
| t8 | One pod's priority differs from its group's. | **The whole gang is blocked**, head included: `all pods in a single pod group should have the same priority as the pod group's priority, got 1000 and 0`. |
| t9 | Same `priorityClassName` on the root, every leaf, and every pod. | Admission resolves `priority: 1000` on the CompositePodGroup and every PodGroup; admitted. |

### API behavior (t6)

- A Workload with only `compositePodGroupTemplates` (no top-level `podGroupTemplates`) is accepted.
- Leaf gang `minCount` is mutable on the runtime PodGroup and on the Workload template.
- Immutable: leaf `schedulingConstraints` (change and removal), the root's `schedulingPolicy`
  (so `minGroupCount`), and a leaf's `parentCompositePodGroupName`.
- `minCount: 0` is rejected, both at creation and on update.
- More than one topology key on a group is rejected (`must have at most 1 item`); an invalid
  label key is rejected.
- Eight child templates are accepted; nine are rejected.
- **The API does not validate hierarchy consistency**: a leaf naming a missing parent or an
  unknown template name is accepted. KubeRay must create objects in order and verify them.
- `compositepodgroups` is served only at `scheduling.k8s.io/v1alpha3`; Workload and PodGroup
  are served at both `v1alpha3` and `v1beta1`.

### Misconfiguration (probe-misconfig.sh)

| Misconfiguration | What KubeRay can observe |
| --- | --- |
| kube-apiserver gates off | `compositepodgroups` is **missing from API discovery**. A PodGroup create **succeeds but `schedulingConstraints` is silently dropped**. |
| kube-scheduler gates off, apiserver on | Everything is **persisted correctly**, but the constraint is **not enforced**: two pods of a rack-constrained leaf ran in different racks. This cannot be detected from the API. |

## KubeRay results (operator built from this branch, same cluster)

| # | Scenario | Result |
| --- | --- | --- |
| o1 | RayCluster with `training` (rack), `data` (zone), and an unconstrained group, plus the gang label. | KubeRay created Workload, root CompositePodGroup, head leaf, and three worker leaves. `minGroupCount` 4; topology only on the two constrained leaves. Every pod joined its own leaf, `training` was in one rack, `data` in one zone, and the head ran on the unlabeled CPU node. |
| o2 | Change `training` replicas 2 to 3. | `minCount` patched to 3 on both the Workload template and the PodGroup; the Workload and PodGroup UIDs were unchanged; the new pod stayed in the rack. |
| o3 | Change the topology annotation after creation. | The operator reported "recreate the RayCluster"; the Workload was untouched. |
| o4 | Annotation without the gang label; invalid JSON; unknown group; floor-zero group with topology. | Each produced its own error; **no scheduling objects and no pods were created.** |
| o5 | A worker group that cannot fit one zone. | The operator created everything; **no pod bound**, head included. |
| o6 | Autoscaling enabled. | Leaf `minCount` is `minReplicas` (training 1, data 2), head 1. |
| o7 | Delete the RayCluster. | All scheduling objects were removed. |
| o8 | No annotation (existing WAS usage). | Unchanged: one `rc-cluster` PodGroup and one Workload; every pod in it. |

Regression check, run manually on a cluster that serves only `GenericWorkload` (no
`compositepodgroups`): the operator started normally (no CompositePodGroup watch), a plain gang
RayCluster scheduled exactly as before, and a RayCluster with the annotation was refused with
`API server does not serve compositepodgroups.scheduling.k8s.io/v1alpha3`, creating no pods.

E2E tests: nine new tests in `ray-operator/test/e2ekuberneteswas/` (`kubernetes_was_topology_test.go`
and `kubernetes_was_topology_placement_test.go`), run with real Ray images. The two kind configs
(`hack/` and `ci/`) gained the `TopologyAwareWorkloadScheduling` and `CompositePodGroup` gates and two
labeled workers (rack `r1`/zone `z1` and rack `r2`/zone `z2`). All nine pass, and the existing 20 WAS
e2e tests still pass on the new multi-node cluster (29 of 29). The first six need no particular node
layout:

| Test | Checks |
| --- | --- |
| `CreatesHierarchyAndCleansUp` | Workload layout (no flat template, `minGroupCount` 3, per-leaf `minCount` and topology, none on root or head), root and leaf PodGroups with parent references, every pod in its own PodGroup, leaves `PodGroupInitiallyScheduled`, cluster Ready, then deletion removes everything. |
| `UnsatisfiableTopologyHoldsWholeGang` | While no node has the key, no pod binds (head and unconstrained group included) and no leaf is scheduled; after labeling the nodes the gang is admitted and the cluster becomes Ready. |
| `ResizePatchesLeafInPlace` | Scaling one group patches its leaf `minCount` in the Workload and PodGroup with unchanged UIDs; root and siblings untouched. |
| `StructuralChangeIsRejected` | Changing the topology request leaves the Workload and PodGroup unchanged. |
| `RequestWithoutGangLabelCreatesNothing`, `InvalidRequestCreatesNothing` | No scheduling objects and no pods. |

The other three need the two labeled racks and skip otherwise. Capacity is exact, using a fake
`example.com/slot` resource that the tests add to the nodes:

| Test | Checks |
| --- | --- |
| `PlacesEachGroupInItsOwnDomain` | A rack-constrained and a zone-constrained group of two pods each, two slots per node: each group is entirely within one domain, and capacity forces them into different racks and zones. |
| `GroupThatFitsNoDomainBlocksTheGang` | A group needing three pods in one rack when each rack holds two (four free in total): nothing binds, including the head. After one rack gets a third slot, the gang is admitted with the group in that rack. |
| `GrowthStaysInTheDomain` | Growing an admitted group while its node is full and the other rack is free leaves the new pod Pending; it schedules into the same rack once room appears. |

Findings from writing them:

- The **root CompositePodGroup's `status` stays empty** in v1.37; only leaf PodGroups report
  `PodGroupInitiallyScheduled`. Observability should use the leaves.
- **The scheduler's greedy, no-backtracking sibling placement is real.** In an earlier version of
  `GroupThatFitsNoDomainBlocksTheGang` the unconstrained `plain` group also needed one slot. The
  training group needed all three slots of one rack and `plain` fit in the other, so the layout was
  feasible, yet the gang stayed blocked in 5 of 6 runs: `plain` was evidently placed first and took a
  slot in the only rack that fits `training`. Giving `plain` extra room confirmed it: with five slots
  the same RayCluster scheduled at once. The test now keeps `plain` slot-free to stay deterministic.
  The prototype's earlier attempts to provoke this (`t2`) did not trigger it.

Unit tests: 21 new provider test functions (layout, floors, validation, creation order, priority copy,
resize in place, structural-change rejection, dropped constraint, routing, cleanup order), plus
one test with 5 cases for the controller activation check. Existing WAS and Helm tests still pass after updating the
expected RBAC rules.

## Not covered

- A real Ray workload: pods use a stub image, so only scheduling is observed, not Ray startup.
- The new e2e tests have not run in Buildkite, only on a local kind cluster built from the same config.
- RayJob and RayService propagation.
- `preemptionPolicy`: the priority class is copied, but the `PodGroupPreemptionPolicy` gate was not enabled.
- Multi-host worker groups, node loss, and replacement after a domain disappears.
- How the scheduler orders sibling groups, and whether KubeRay can influence it. The greedy
  placement strand described above was seen but its cause (sibling order) was inferred, not verified.
- Scale, performance, and real workloads.
