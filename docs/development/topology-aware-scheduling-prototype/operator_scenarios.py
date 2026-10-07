#!/usr/bin/env python3
"""Runs the KubeRay operator (built from this branch) against the kind cluster from setup.sh.

Unlike proto.py, the Workload / CompositePodGroup / PodGroup objects here are created by KubeRay
from a RayCluster annotation. Needs the operator deployed with the KubernetesWAS feature gate:

    cd ray-operator && IMG=kuberay/operator:tas-proto make docker-image
    kind load docker-image kuberay/operator:tas-proto --name tas-proto
    IMG=kuberay/operator:tas-proto make deploy-kubernetes-was-v1alpha3

Usage: KUBECONFIG=/tmp/tas-proto.kubeconfig ./operator_scenarios.py [o1 o2 ...|all]
"""
import json
import subprocess
import sys
import time

import proto
from proto import RACK, ZONE, kubectl

IMAGE = "registry.k8s.io/pause:3.10"
SLOT = {"requests": {"example.com/slot": "1"}, "limits": {"example.com/slot": "1"}}


def worker(name, replicas, slots=True, min_replicas=None, max_replicas=None, pool="gpu"):
    spec = {
        "nodeSelector": {"pool": pool},
        "containers": [{"name": "ray-worker", "image": IMAGE, "imagePullPolicy": "IfNotPresent"}],
    }
    if slots:
        spec["containers"][0]["resources"] = SLOT
    return {
        "groupName": name,
        "replicas": replicas,
        "minReplicas": replicas if min_replicas is None else min_replicas,
        "maxReplicas": replicas if max_replicas is None else max_replicas,
        "rayStartParams": {},
        "template": {"spec": spec},
    }


def raycluster(ns, topology, workers, gang=True, autoscaling=False, name="rc"):
    annotations = {}
    if topology is not None:
        annotations["ray.io/worker-group-topology"] = topology if isinstance(topology, str) else json.dumps(topology)
    labels = {"ray.io/gang-scheduling-enabled": "true"} if gang else {}
    spec = {
        "rayVersion": "2.46.0",
        "headGroupSpec": {
            "rayStartParams": {},
            "template": {"spec": {
                "nodeSelector": {"pool": "cpu"},
                "containers": [{"name": "ray-head", "image": IMAGE, "imagePullPolicy": "IfNotPresent"}],
            }},
        },
        "workerGroupSpecs": workers,
    }
    if autoscaling:
        spec["enableInTreeAutoscaling"] = True
    return {
        "apiVersion": "ray.io/v1", "kind": "RayCluster",
        "metadata": {"name": name, "namespace": ns, "labels": labels, "annotations": annotations},
        "spec": spec,
    }


def fresh(ns):
    proto.ns_fresh(ns)
    proto.reset_slots()


def apply(obj):
    proto.apply(obj)


def pods(ns):
    """{group: [(pod, node, rack, zone)]} for Ray pods."""
    nodes = json.loads(kubectl("get", "nodes", "-o", "json").stdout)["items"]
    nl = {n["metadata"]["name"]: n["metadata"].get("labels", {}) for n in nodes}
    items = json.loads(kubectl("-n", ns, "get", "pods", "-o", "json").stdout)["items"]
    out = {}
    for p in items:
        group = p["metadata"].get("labels", {}).get("ray.io/group")
        if not group:
            continue
        node = p["spec"].get("nodeName")
        lab = nl.get(node, {})
        out.setdefault(group, []).append((p["metadata"]["name"], node, lab.get(RACK), lab.get(ZONE), p["spec"].get("schedulingGroup", {}).get("podGroupName")))
    return out


def wait_pods(ns, total, bound, timeout=90):
    """Wait until `total` pods exist and `bound` of them are bound."""
    start = time.time()
    while time.time() - start < timeout:
        ps = [x for v in pods(ns).values() for x in v]
        if len(ps) >= total and sum(1 for x in ps if x[1]) >= bound:
            break
        time.sleep(3)
    return pods(ns)


def show(ns, title):
    print(f"  [{title}]")
    ps = pods(ns)
    for group in sorted(ps):
        for name, node, rack, zone, pg in sorted(ps[group]):
            print(f"    {group:9s} {name:34s} node={node or '-':20s} rack={rack or '-':3s} zone={zone or '-':3s} podGroup={pg}")
    return ps


def locality(ps, group, key):
    idx = 2 if key == RACK else 3
    vals = {p[idx] for p in ps.get(group, []) if p[1]}
    return f"{group}: {key.split('/')[-1]} {sorted(v for v in vals if v)} -> {'OK' if len(vals) <= 1 else 'VIOLATION'}"


def tree(ns):
    out = kubectl("-n", ns, "get", "workload,compositepodgroup,podgroup", "-o", "name", check=False).stdout.split()
    return sorted(out)


def operator_errors(since="3m", needle=None):
    log = kubectl("logs", "deploy/kuberay-operator", f"--since={since}", check=False).stdout
    lines = [json.loads(l) for l in log.splitlines() if l.startswith("{")]
    msgs = []
    for l in lines:
        if l.get("level") == "error" or "error" in l:
            m = l.get("error") or l.get("msg")
            if m and (needle is None or needle in str(m)):
                msgs.append(str(m))
    return msgs


def wait_gone(ns, timeout=90):
    start = time.time()
    while time.time() - start < timeout:
        if not tree(ns):
            return True
        time.sleep(3)
    return False


# ------------------------------------------------------------------ scenarios

def o1_happy():
    """KubeRay builds the hierarchy from the annotation; groups are placed per their own domain."""
    ns = "o1"
    fresh(ns)
    apply(raycluster(ns, {"training": RACK, "data": ZONE},
                     [worker("training", 2), worker("data", 3), worker("plain", 1, slots=False, pool="cpu")]))
    ps = wait_pods(ns, 7, 7)
    show(ns, "o1 placement")
    print("   objects:", [t.split("/")[0] + "/" + t.split("/")[1].replace("rc-", "") for t in tree(ns)])
    print("  ", locality(ps, "training", RACK), "|", locality(ps, "data", ZONE))
    print("   head on cpu node:", ps["headgroup"][0][1] == "tas-proto-worker")
    wl = json.loads(kubectl("-n", ns, "get", "workload", "rc", "-o", "json").stdout)
    root = wl["spec"]["compositePodGroupTemplates"][0]
    print("   root minGroupCount:", root["schedulingPolicy"]["gang"]["minGroupCount"],
          "| leaves:", {t["name"]: (t["schedulingPolicy"]["gang"]["minCount"], (t.get("schedulingConstraints") or {}).get("topology")) for t in root["podGroupTemplates"]})


def o2_resize():
    """Resizing a worker group patches minCount in place (same UIDs); the new pod stays in the rack."""
    ns = "o2"
    fresh(ns)
    apply(raycluster(ns, {"training": RACK}, [worker("training", 2, max_replicas=4), worker("data", 1)]))
    wait_pods(ns, 4, 4)
    uid = lambda kind, name: kubectl("-n", ns, "get", kind, name, "-o", "jsonpath={.metadata.uid}").stdout
    before = (uid("workload", "rc"), uid("podgroup", "rc-wg-training"))
    kubectl("-n", ns, "patch", "raycluster", "rc", "--type=json", "-p",
            '[{"op":"replace","path":"/spec/workerGroupSpecs/0/replicas","value":3}]')
    ps = wait_pods(ns, 5, 5, timeout=90)
    after = (uid("workload", "rc"), uid("podgroup", "rc-wg-training"))
    mc = kubectl("-n", ns, "get", "podgroup", "rc-wg-training", "-o", "jsonpath={.spec.schedulingPolicy.gang.minCount}").stdout
    wmc = kubectl("-n", ns, "get", "workload", "rc", "-o",
                  "jsonpath={.spec.compositePodGroupTemplates[0].podGroupTemplates[?(@.name=='wg-training')].schedulingPolicy.gang.minCount}").stdout
    show(ns, "o2 after replicas 2 -> 3")
    print("   same Workload and PodGroup UID:", before == after, "| PodGroup minCount:", mc, "| Workload template minCount:", wmc)
    print("  ", locality(ps, "training", RACK))


def o3_structural():
    """Changing the topology request after creation is rejected without touching scheduling objects."""
    ns = "o3"
    fresh(ns)
    apply(raycluster(ns, {"training": RACK}, [worker("training", 2), worker("data", 2)]))
    wait_pods(ns, 5, 5)
    uid = kubectl("-n", ns, "get", "workload", "rc", "-o", "jsonpath={.metadata.uid}").stdout
    kubectl("-n", ns, "annotate", "raycluster", "rc", "--overwrite", f"ray.io/worker-group-topology={json.dumps({'training': ZONE})}")
    time.sleep(15)
    errs = operator_errors("1m", "recreate the RayCluster")
    print("   operator error reported:", bool(errs))
    if errs:
        print("   ", errs[-1][:260])
    print("   Workload untouched:", uid == kubectl("-n", ns, "get", "workload", "rc", "-o", "jsonpath={.metadata.uid}").stdout)


def o4_failfast():
    """A topology request that cannot be honored fails before any pod is created."""
    ns = "o4"
    fresh(ns)
    apply(raycluster(ns, {"training": RACK}, [worker("training", 2)], gang=False, name="no-gang"))
    apply(raycluster(ns, '{"training":', [worker("training", 2)], name="bad-json"))
    apply(raycluster(ns, {"nope": RACK}, [worker("training", 2)], name="bad-group"))
    apply(raycluster(ns, {"training": RACK}, [worker("training", 0, min_replicas=0)], name="floor-zero"))
    time.sleep(20)
    for needle in ("not opted in to gang scheduling", "JSON object", "unknown worker group", "gang floor of zero"):
        errs = operator_errors("2m", needle)
        print(f"   error mentioning '{needle}':", bool(errs))
    print("   scheduling objects created:", tree(ns))
    print("   pods created:", len(json.loads(kubectl("-n", ns, "get", "pods", "-o", "json").stdout)["items"]))


def o5_infeasible():
    """An infeasible worker group blocks the whole RayCluster, head included."""
    ns = "o5"
    fresh(ns)
    apply(raycluster(ns, {"a": RACK, "b": ZONE}, [worker("a", 2), worker("b", 5)]))
    ps = wait_pods(ns, 8, 8, timeout=40)
    show(ns, "o5 b needs 5 in one zone, zones hold 4: expect nothing bound")
    print("   bound pods:", sum(1 for v in ps.values() for x in v if x[1]), "of", sum(len(v) for v in ps.values()))


def o6_autoscaling():
    """With the autoscaler, each leaf's gang floor is minReplicas, not replicas."""
    ns = "o6"
    fresh(ns)
    apply(raycluster(ns, {"training": RACK}, [worker("training", 3, min_replicas=1, max_replicas=4), worker("data", 2, min_replicas=2, max_replicas=2)], autoscaling=True))
    ps = wait_pods(ns, 6, 6)
    mcs = {n: kubectl("-n", ns, "get", "podgroup", f"rc-{n}", "-o", "jsonpath={.spec.schedulingPolicy.gang.minCount}", check=False).stdout for n in ("head", "wg-training", "wg-data")}
    print("   leaf minCounts (expect head 1, training 1, data 2):", mcs)
    print("  ", locality(ps, "training", RACK), "| pods:", {g: len(v) for g, v in ps.items()})


def o7_cleanup():
    """Deleting the RayCluster removes every scheduling object."""
    ns = "o7"
    fresh(ns)
    apply(raycluster(ns, {"training": RACK}, [worker("training", 2)]))
    wait_pods(ns, 3, 3)
    print("   before:", len(tree(ns)), "objects")
    kubectl("-n", ns, "delete", "raycluster", "rc", "--wait=false")
    print("   all scheduling objects removed:", wait_gone(ns))


def o8_flat_regression():
    """Without the annotation the existing single-PodGroup layout is unchanged."""
    ns = "o8"
    fresh(ns)
    apply(raycluster(ns, None, [worker("training", 2), worker("data", 3)]))
    ps = wait_pods(ns, 6, 6)
    print("   objects:", tree(ns))
    print("   every pod in rc-cluster:", all(x[4] == "rc-cluster" for v in ps.values() for x in v))


SCENARIOS = {"o1": o1_happy, "o2": o2_resize, "o3": o3_structural, "o4": o4_failfast,
             "o5": o5_infeasible, "o6": o6_autoscaling, "o7": o7_cleanup, "o8": o8_flat_regression}

if __name__ == "__main__":
    names = sys.argv[1:] or ["all"]
    if names == ["all"]:
        names = list(SCENARIOS)
    for n in names:
        print(f"== {n}: {SCENARIOS[n].__doc__.strip()}")
        SCENARIOS[n]()
