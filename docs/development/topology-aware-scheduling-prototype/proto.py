#!/usr/bin/env python3
"""Hand-built prototype of the proposed Workload / CompositePodGroup / PodGroup hierarchy.

Builds the objects KubeRay would build (no operator involved) on a kind v1.37 cluster
(see kind-config.yaml and setup-nodes.sh) and reports what the scheduler does.

Usage: KUBECONFIG=... ./proto.py <scenario> [<scenario> ...]   (or "all")
"""
import json
import subprocess
import sys
import time

API = "scheduling.k8s.io/v1alpha3"
RACK = "topology.example.com/rack"
ZONE = "topology.example.com/zone"
IMAGE = "registry.k8s.io/pause:3.10"


def kubectl(*args, stdin=None, check=True):
    p = subprocess.run(["kubectl", *args], input=stdin, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"kubectl {' '.join(args)}\n{p.stdout}{p.stderr}")
    return p


def apply(obj, check=True):
    return kubectl("apply", "-f", "-", stdin=json.dumps(obj), check=check)


def create(obj, check=True):
    return kubectl("create", "-f", "-", stdin=json.dumps(obj), check=check)


def ns_fresh(ns):
    # Scenarios share node capacity, so remove every earlier scenario namespace first.
    names = kubectl("get", "ns", "-o", "name").stdout.split()
    old = [n for n in names if n[10:11] in ("t", "o") and n[11:12].isdigit()]
    if old:
        kubectl("delete", *old, "--wait=true", "--timeout=180s", check=False)
    kubectl("create", "ns", ns)


def meta(name, ns, **extra):
    return {"name": name, "namespace": ns, **extra}


class Group:
    """One worker group = one leaf PodGroup."""

    def __init__(self, name, pods, key=None, min_count=None):
        self.name, self.pods, self.key = name, pods, key
        self.min_count = pods if min_count is None else min_count

    @property
    def tmpl(self):
        return f"wg-{self.name}"


def leaf_template(name, min_count, key=None):
    t = {"name": name, "schedulingPolicy": {"gang": {"minCount": min_count}}}
    if key:
        t["schedulingConstraints"] = {"topology": [{"key": key}]}
    return t


def build_workload(ns, groups, head_pods=1, name="rc", use_composite=True):
    leaves = [leaf_template("head", head_pods)] + [leaf_template(g.tmpl, g.min_count, g.key) for g in groups]
    return {
        "apiVersion": API,
        "kind": "Workload",
        "metadata": meta(name, ns),
        "spec": {
            "compositePodGroupTemplates": [
                {
                    "name": "cluster",
                    "schedulingPolicy": {"gang": {"minGroupCount": len(leaves)}},
                    "podGroupTemplates": leaves,
                }
            ]
        },
    }


def build_root(ns, n_leaves, name="rc", wl="rc"):
    return {
        "apiVersion": API,
        "kind": "CompositePodGroup",
        "metadata": meta(f"{name}-cluster", ns),
        "spec": {
            "workloadRef": {"workloadName": wl, "templateName": "cluster"},
            "schedulingPolicy": {"gang": {"minGroupCount": n_leaves}},
        },
    }


def build_leaf(ns, tmpl, min_count, key=None, name="rc", wl="rc"):
    spec = {
        "parentCompositePodGroupName": f"{name}-cluster",
        "workloadRef": {"workloadName": wl, "templateName": tmpl},
        "schedulingPolicy": {"gang": {"minCount": min_count}},
    }
    if key:
        spec["schedulingConstraints"] = {"topology": [{"key": key}]}
    return {"apiVersion": API, "kind": "PodGroup", "metadata": meta(f"{name}-{tmpl}", ns), "spec": spec}


def build_pod(ns, name, group, pool, slot=True, name_prefix="rc"):
    c = {"name": "c", "image": IMAGE, "imagePullPolicy": "IfNotPresent"}
    if slot:
        c["resources"] = {"requests": {"example.com/slot": "1"}, "limits": {"example.com/slot": "1"}}
    return {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": meta(name, ns, labels={"proto/group": group}),
        "spec": {
            "schedulingGroup": {"podGroupName": f"{name_prefix}-{group}"},
            "nodeSelector": {"pool": pool},
            "containers": [c],
        },
    }


def pods_for(ns, groups, head_pool="cpu"):
    out = [build_pod(ns, "head-0", "head", head_pool, slot=False)]
    for g in groups:
        for i in range(g.pods):
            out.append(build_pod(ns, f"{g.name}-{i}", g.tmpl, "gpu"))
    return out


def create_all(ns, groups, head_pool="cpu", pods=True):
    """Parent-first creation order: Workload, root, leaves, then pods."""
    create(build_workload(ns, groups))
    create(build_root(ns, len(groups) + 1))
    create(build_leaf(ns, "head", 1))
    for g in groups:
        create(build_leaf(ns, g.tmpl, g.min_count, g.key))
    if pods:
        for p in pods_for(ns, groups, head_pool):
            create(p)


def snapshot(ns):
    """Return {pod: (node, rack, zone, phase)}"""
    nodes = json.loads(kubectl("get", "nodes", "-o", "json").stdout)["items"]
    nl = {n["metadata"]["name"]: n["metadata"].get("labels", {}) for n in nodes}
    pods = json.loads(kubectl("-n", ns, "get", "pods", "-o", "json").stdout)["items"]
    res = {}
    for p in pods:
        node = p["spec"].get("nodeName")
        lab = nl.get(node, {})
        res[p["metadata"]["name"]] = (node, lab.get(RACK), lab.get(ZONE), p["status"].get("phase"))
    return res


def wait_settle(ns, expect_bound=None, timeout=60, quiet=15):
    """Wait until expect_bound pods are bound, or until nothing changes for `quiet` seconds."""
    start, last, last_change = time.time(), None, time.time()
    while time.time() - start < timeout:
        s = snapshot(ns)
        bound = {k for k, v in s.items() if v[0]}
        if expect_bound is not None and len(bound) >= expect_bound:
            return s
        if last != bound:
            last, last_change = bound, time.time()
        elif expect_bound is None and time.time() - last_change > quiet:
            return s
        elif expect_bound is not None and time.time() - last_change > quiet and time.time() - start > quiet:
            pass
        time.sleep(2)
    return snapshot(ns)


def show(ns, title):
    s = snapshot(ns)
    print(f"  [{title}]")
    for k in sorted(s):
        node, rack, zone, phase = s[k]
        print(f"    {k:10s} node={node or '-':22s} rack={rack or '-':3s} zone={zone or '-':3s} {phase}")
    return s


def groups_ok(s, groups):
    """Check per-group locality; return list of strings."""
    out = []
    for g in groups:
        ps = [v for k, v in s.items() if k.startswith(g.name + "-")]
        if not g.key:
            out.append(f"{g.name}: unconstrained")
            continue
        idx = 1 if g.key == RACK else 2
        vals = {p[idx] for p in ps if p[0]}
        out.append(f"{g.name}: {g.key.split('/')[-1]} values={sorted(v for v in vals if v)} -> {'OK' if len(vals) <= 1 else 'VIOLATION'}")
    return out


def set_slots(node, n):
    kubectl(
        "patch", "node", node, "--subresource=status", "--type=json",
        "-p", json.dumps([
            {"op": "add", "path": "/status/capacity/example.com~1slot", "value": str(n)},
            {"op": "add", "path": "/status/allocatable/example.com~1slot", "value": str(n)},
        ]),
    )


def reset_slots():
    for n in ("worker2", "worker3", "worker4", "worker5"):
        set_slots(f"tas-proto-{n}", 2)


# ---------------------------------------------------------------- scenarios

def t1_happy():
    """Head on unlabeled CPU node; A rack-local, B zone-local, C unconstrained; all admitted."""
    ns = "t1"
    ns_fresh(ns)
    gs = [Group("a", 2, RACK), Group("b", 3, ZONE), Group("c", 1)]
    create_all(ns, gs)
    s = wait_settle(ns, expect_bound=7)
    show(ns, "t1 result")
    print("  ", groups_ok(s, gs))
    print("   head on cpu node:", s["head-0"][0] == "tas-proto-worker")


def t2_leaf_blocks():
    """B needs 5 in one zone but each zone has 4 slots (8 free in total): nothing should bind."""
    ns = "t2"
    ns_fresh(ns)
    reset_slots()
    gs = [Group("a", 2, RACK), Group("b", 5, ZONE)]
    create_all(ns, gs)
    s = wait_settle(ns, quiet=30)
    show(ns, "t2 infeasible B: expect nothing bound")
    print("   bound pods:", sum(1 for v in s.values() if v[0]))
    # Free capacity is now enough for B in z1 (5 slots) only if A is not placed there first.
    set_slots("tas-proto-worker2", 3)
    s = wait_settle(ns, expect_bound=8, timeout=180)
    show(ns, "t2 after z1 gets 5 slots (z1=5, z2=4; A needs 2, B needs 5)")
    print("  ", groups_ok(s, gs), "bound:", sum(1 for v in s.values() if v[0]), "/ 8")
    # Enough room in z1 for B even after A takes 2 slots there.
    set_slots("tas-proto-worker2", 5)
    s = wait_settle(ns, expect_bound=8, timeout=90)
    show(ns, "t2 after z1 gets 7 slots")
    print("  ", groups_ok(s, gs), "bound:", sum(1 for v in s.values() if v[0]), "/ 8")
    reset_slots()


def t3_head_blocks():
    """Head cannot be scheduled (no such pool): the feasible workers must not start either."""
    ns = "t3"
    ns_fresh(ns)
    reset_slots()
    gs = [Group("a", 2, RACK), Group("b", 3, ZONE)]
    create_all(ns, gs, head_pool="nonexistent")
    s = wait_settle(ns, quiet=30)
    show(ns, "t3 unschedulable head: expect nothing bound")
    print("   bound pods:", sum(1 for v in s.values() if v[0]))


def t4_order():
    """Pods created before the whole hierarchy exists must not schedule as independent gangs."""
    ns = "t4"
    ns_fresh(ns)
    reset_slots()
    gs = [Group("a", 2, RACK), Group("b", 3, ZONE)]
    # Workload + head leaf + a leaf only, with pods for head and a. No root, no b leaf.
    create(build_workload(ns, gs))
    create(build_leaf(ns, "head", 1))
    create(build_leaf(ns, "wg-a", 2, RACK))
    for p in pods_for(ns, gs):
        if p["metadata"]["name"].startswith(("head", "a-")):
            create(p, check=False)
    s = wait_settle(ns, quiet=20)
    show(ns, "t4 phase 1: root and b missing; expect nothing bound")
    # Add the root, still no b leaf/pods.
    create(build_root(ns, 3))
    s = wait_settle(ns, quiet=20)
    show(ns, "t4 phase 2: root exists, b leaf and pods missing; expect nothing bound")
    create(build_leaf(ns, "wg-b", 3, ZONE))
    for p in pods_for(ns, gs):
        if p["metadata"]["name"].startswith("b-"):
            create(p)
    s = wait_settle(ns, expect_bound=6, timeout=60)
    show(ns, "t4 phase 3: all present; expect all bound")
    print("  ", groups_ok(s, gs))


def t5_growth():
    """After admission, extra pods in group A stay in A's domain; a full domain leaves them Pending."""
    ns = "t5"
    ns_fresh(ns)
    reset_slots()
    gs = [Group("a", 2, RACK)]
    create_all(ns, gs)
    s = wait_settle(ns, expect_bound=3)
    show(ns, "t5 initial")
    # Patch minCount stays 2 (the floor). Add pods above the floor.
    for i in (2, 3):
        create(build_pod(ns, f"a-{i}", "wg-a", "gpu"))
    s = wait_settle(ns, expect_bound=5, timeout=40)
    show(ns, "t5 +2 pods (rack has 4 slots, should all fit in the same rack)")
    print("  ", groups_ok(s, gs))
    create(build_pod(ns, "a-4", "wg-a", "gpu"))
    s = wait_settle(ns, quiet=25)
    show(ns, "t5 +1 pod beyond the rack's capacity (other rack is free): expect Pending")
    print("  ", groups_ok(s, gs))


def t6_api():
    """API validation: immutability, patching minCount, template limit, missing top-level podGroupTemplates."""
    ns = "t6"
    ns_fresh(ns)
    gs = [Group("a", 2, RACK)]
    create_all(ns, gs, pods=False)
    wl = json.loads(kubectl("-n", ns, "get", "workload", "rc", "-o", "json").stdout)
    print("   workload.spec.podGroupTemplates:", wl["spec"].get("podGroupTemplates"))

    def attempt(label, *args):
        p = kubectl(*args, check=False)
        msg = (p.stdout + p.stderr).strip().splitlines()
        print(f"   {label}: {'OK' if p.returncode == 0 else 'REJECTED'} {'' if p.returncode == 0 else msg[-1][:200]}")

    attempt("patch leaf PodGroup minCount 2->3", "-n", ns, "patch", "podgroup", "rc-wg-a", "--type=merge",
            "-p", '{"spec":{"schedulingPolicy":{"gang":{"minCount":3}}}}')
    attempt("patch Workload leaf template minCount 3->3 via json patch (2->3)", "-n", ns, "patch", "workload", "rc", "--type=json",
            "-p", '[{"op":"replace","path":"/spec/compositePodGroupTemplates/0/podGroupTemplates/1/schedulingPolicy/gang/minCount","value":3}]')
    attempt("patch leaf topology key", "-n", ns, "patch", "podgroup", "rc-wg-a", "--type=merge",
            "-p", f'{{"spec":{{"schedulingConstraints":{{"topology":[{{"key":"{ZONE}"}}]}}}}}}')
    attempt("patch leaf remove topology", "-n", ns, "patch", "podgroup", "rc-wg-a", "--type=json",
            "-p", '[{"op":"remove","path":"/spec/schedulingConstraints"}]')
    attempt("patch root minGroupCount 2->1", "-n", ns, "patch", "compositepodgroup", "rc-cluster", "--type=merge",
            "-p", '{"spec":{"schedulingPolicy":{"gang":{"minGroupCount":1}}}}')
    attempt("patch leaf parent reference", "-n", ns, "patch", "podgroup", "rc-wg-a", "--type=merge",
            "-p", '{"spec":{"parentCompositePodGroupName":"other"}}')
    attempt("patch leaf minCount to 0", "-n", ns, "patch", "podgroup", "rc-wg-a", "--type=merge",
            "-p", '{"spec":{"schedulingPolicy":{"gang":{"minCount":0}}}}')

    # Two topology keys on one leaf.
    bad = build_leaf(ns, "wg-a", 1, RACK)
    bad["metadata"]["name"] = "two-keys"
    bad["spec"]["schedulingConstraints"]["topology"].append({"key": ZONE})
    p = create(bad, check=False)
    print("   two topology keys on one leaf:", "OK" if p.returncode == 0 else "REJECTED " + p.stderr.strip().splitlines()[-1][:160])
    bad2 = build_leaf(ns, "wg-a", 1, "Not A Valid Key!")
    bad2["metadata"]["name"] = "bad-key"
    p = create(bad2, check=False)
    print("   invalid label key:", "OK" if p.returncode == 0 else "REJECTED " + p.stderr.strip().splitlines()[-1][:160])

    # Template limits: 8 leaves ok, 9 rejected.
    for n in (8, 9):
        w = build_workload(ns, [Group(f"g{i}", 1) for i in range(n - 1)], name=f"w{n}")
        p = create(w, check=False)
        print(f"   {n} leaf templates:", "OK" if p.returncode == 0 else "REJECTED " + p.stderr.strip().splitlines()[-1][:160])

    # Leaf with minCount 0 at creation.
    z = build_leaf(ns, "wg-a", 0)
    z["metadata"]["name"] = "zero"
    p = create(z, check=False)
    print("   leaf minCount 0 at creation:", "OK" if p.returncode == 0 else "REJECTED " + p.stderr.strip().splitlines()[-1][:160])

    # Leaf referencing a missing parent / workload template.
    m = build_leaf(ns, "wg-a", 1)
    m["metadata"]["name"] = "orphan"
    m["spec"]["parentCompositePodGroupName"] = "does-not-exist"
    p = create(m, check=False)
    print("   leaf with missing parent:", "OK" if p.returncode == 0 else "REJECTED " + p.stderr.strip().splitlines()[-1][:160])
    m = build_leaf(ns, "wg-a", 1)
    m["metadata"]["name"] = "badtmpl"
    m["spec"]["workloadRef"]["templateName"] = "nope"
    p = create(m, check=False)
    print("   leaf with unknown template name:", "OK" if p.returncode == 0 else "REJECTED " + p.stderr.strip().splitlines()[-1][:160])


def t7_cleanup():
    """Finalizers, ownership and deletion order, with a ConfigMap standing in for the RayCluster."""
    ns = "t7"
    ns_fresh(ns)
    reset_slots()
    owner = json.loads(kubectl("-n", ns, "create", "configmap", "fake-raycluster", "-o", "json").stdout)
    ref = [{"apiVersion": "v1", "kind": "ConfigMap", "name": "fake-raycluster", "uid": owner["metadata"]["uid"],
            "controller": True, "blockOwnerDeletion": True}]
    gs = [Group("a", 2, RACK)]
    objs = [build_workload(ns, gs), build_root(ns, 2), build_leaf(ns, "head", 1), build_leaf(ns, "wg-a", 2, RACK)]
    for o in objs:
        o["metadata"]["ownerReferences"] = ref
        create(o)
    for p in pods_for(ns, gs):
        p["metadata"]["ownerReferences"] = ref
        create(p)
    wait_settle(ns, expect_bound=3)
    for kind in ("workload", "compositepodgroup", "podgroup"):
        out = kubectl("-n", ns, "get", kind, "-o", "jsonpath={range .items[*]}{.metadata.name}{\" finalizers=\"}{.metadata.finalizers}{\"\\n\"}{end}").stdout
        print(f"   {kind}:\n" + "".join("      " + l + "\n" for l in out.splitlines()))
    kubectl("-n", ns, "delete", "configmap", "fake-raycluster", "--wait=false")
    for _ in range(12):
        time.sleep(5)
        left = kubectl("-n", ns, "get", "workload,compositepodgroup,podgroup,pod", "-o", "name").stdout.split()
        if not left:
            break
    print("   after deleting owner, remaining objects after ~60s max:", left)
    if left:
        out = kubectl("-n", ns, "get", "workload,compositepodgroup,podgroup", "-o",
                      "jsonpath={range .items[*]}{.kind}/{.metadata.name} del={.metadata.deletionTimestamp} fin={.metadata.finalizers}{\"\\n\"}{end}").stdout
        print(out)


def t8_priority():
    """Pods whose priority differs from the hierarchy's (single common priority)."""
    ns = "t8"
    ns_fresh(ns)
    reset_slots()
    kubectl("apply", "-f", "-", stdin=json.dumps({
        "apiVersion": "scheduling.k8s.io/v1", "kind": "PriorityClass", "metadata": {"name": "proto-high"}, "value": 1000}))
    gs = [Group("a", 2, RACK)]
    create(build_workload(ns, gs))
    create(build_root(ns, 2))
    create(build_leaf(ns, "head", 1))
    create(build_leaf(ns, "wg-a", 2, RACK))
    for g in json.loads(kubectl("-n", ns, "get", "compositepodgroup,podgroup", "-o", "json").stdout)["items"]:
        print("   ", g["kind"], g["metadata"]["name"], "priority=", g["spec"].get("priority"), "class=", g["spec"].get("priorityClassName"))
    pods = pods_for(ns, gs)
    for p in pods:
        if p["metadata"]["name"] == "a-1":
            p["spec"]["priorityClassName"] = "proto-high"
        create(p)
    s = wait_settle(ns, quiet=30)
    show(ns, "t8 one pod (a-1) has a different priority than its group")
    kubectl("delete", "priorityclass", "proto-high", "--ignore-not-found")


def t9_priority_class():
    """A common PriorityClass on the root, head leaf and worker leaf, with matching pods: admitted."""
    ns = "t9"
    ns_fresh(ns)
    reset_slots()
    kubectl("apply", "-f", "-", stdin=json.dumps({
        "apiVersion": "scheduling.k8s.io/v1", "kind": "PriorityClass", "metadata": {"name": "proto-high"}, "value": 1000}))
    gs = [Group("a", 2, RACK)]
    wl = build_workload(ns, gs)
    root_t = wl["spec"]["compositePodGroupTemplates"][0]
    root_t["priorityClassName"] = "proto-high"
    for t in root_t["podGroupTemplates"]:
        t["priorityClassName"] = "proto-high"
    create(wl)
    root = build_root(ns, 2)
    root["spec"]["priorityClassName"] = "proto-high"
    create(root)
    for tmpl, mc, key in (("head", 1, None), ("wg-a", 2, RACK)):
        leaf = build_leaf(ns, tmpl, mc, key)
        leaf["spec"]["priorityClassName"] = "proto-high"
        create(leaf)
    for g in json.loads(kubectl("-n", ns, "get", "compositepodgroup,podgroup", "-o", "json").stdout)["items"]:
        print("   ", g["kind"], g["metadata"]["name"], "priority=", g["spec"].get("priority"), "class=", g["spec"].get("priorityClassName"))
    for p in pods_for(ns, gs):
        p["spec"]["priorityClassName"] = "proto-high"
        create(p)
    s = wait_settle(ns, expect_bound=3, timeout=60)
    show(ns, "t9 uniform PriorityClass")
    kubectl("delete", "priorityclass", "proto-high", "--ignore-not-found")


SCENARIOS = {
    "t1": t1_happy, "t2": t2_leaf_blocks, "t3": t3_head_blocks, "t4": t4_order,
    "t5": t5_growth, "t6": t6_api, "t7": t7_cleanup, "t8": t8_priority, "t9": t9_priority_class,
}

if __name__ == "__main__":
    names = sys.argv[1:] or ["all"]
    if names == ["all"]:
        names = list(SCENARIOS)
    for n in names:
        print(f"== {n}: {SCENARIOS[n].__doc__.strip().splitlines()[0]}")
        SCENARIOS[n]()
