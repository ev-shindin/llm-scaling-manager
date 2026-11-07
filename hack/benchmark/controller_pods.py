#!/usr/bin/env python3
"""Find the WVA controller's own Pods, and only those.

Labels cannot do it. Warm-pool Pods carry app.kubernetes.io/name, the label
the benchmark scripts used to select the controller by, so `kubectl logs -l`
read the pool's logs as the controller's. The e2e relay Pods carry every
controller label on purpose, control-plane=controller-manager included, and
so does anything else that wants the controller's NetworkPolicy to admit it.
`kubectl logs deployment/<name>` does not help either: it picks a Pod by the
Deployment's selector, which is those same labels.

Ownership does identify it. A Deployment's Pod is controlled by a ReplicaSet
named <deployment>-<pod-template-hash>, and the hash is also a label on the
Pod. This is the check test/utils/controller_pods.go makes in Go
(scaletarget.OwnedByDeployment).

The result is a label selector that names the controller Pods' hashes, so a
caller still makes ONE `kubectl logs -l` call and still gets every controller
Pod: the old and the new one during a rollout, or several replicas.

As a command, it reads a Pod list (`kubectl get pods -o json`) on stdin and
prints that selector. It exits 1, printing nothing on stdout, when the list
holds no controller Pod or cannot be read.

With --pods it prints one line per controller Pod instead, tab-separated:
its name, its phase, the learner recording directory the manager was given
(LEARNER_RECORDING_DIR or --learner-recording-dir), and the container that can
read it (the `recordings` sidecar of the recorder patch). An absent field is
`-`. hack/benchmark/learner_recordings.sh reads it. A list with no controller
Pod prints nothing and exits 0; only a list it cannot read exits 1.

Usage
-----
  kubectl get pods -n NS -l control-plane=controller-manager -o json \\
      | python3 hack/benchmark/controller_pods.py
  ... | python3 hack/benchmark/controller_pods.py --pods [deployment]
"""
import json
import subprocess
import sys

# The deploy/ install and every overlay name the Deployment this.
CONTROLLER_DEPLOYMENT = "wva-controller-manager"
# Narrows the list before ownership is checked. It does not identify the
# controller on its own.
CONTROLLER_LABEL = "control-plane=controller-manager"
HASH_LABEL = "pod-template-hash"


def is_controller_pod(pod, deployment=CONTROLLER_DEPLOYMENT):
    """Reports whether pod was created by the named Deployment."""
    meta = pod.get("metadata") or {}
    pod_hash = (meta.get("labels") or {}).get(HASH_LABEL, "")
    if not pod_hash:
        return False
    for ref in meta.get("ownerReferences") or []:
        if (ref.get("controller") is True and ref.get("kind") == "ReplicaSet"
                and ref.get("name") == f"{deployment}-{pod_hash}"):
            return True
    return False


def controller_hashes(pod_list, deployment=CONTROLLER_DEPLOYMENT):
    """Returns the sorted pod-template-hash values of the controller's Pods."""
    return sorted({
        p["metadata"]["labels"][HASH_LABEL]
        for p in pod_list.get("items") or []
        if is_controller_pod(p, deployment)
    })


def log_selector(pod_list, deployment=CONTROLLER_DEPLOYMENT):
    """Returns a label selector matching only the controller's Pods, or None."""
    hashes = controller_hashes(pod_list, deployment)
    if not hashes:
        return None
    return f"{CONTROLLER_LABEL},{HASH_LABEL} in ({','.join(hashes)})"


def controller_log_selector(namespace, deployment=CONTROLLER_DEPLOYMENT):
    """Lists the Pods in namespace and returns log_selector for them.

    Raises RuntimeError when the list fails or holds no controller Pod, so a
    caller cannot read "no Pod" as "the controller logged nothing".
    """
    cmd = ["kubectl", "get", "pods", "-n", namespace,
           "-l", CONTROLLER_LABEL, "-o", "json"]
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"{' '.join(cmd)} failed (exit {proc.returncode}): "
                           f"{proc.stderr.strip()}")
    try:
        pods = json.loads(proc.stdout)
    except json.JSONDecodeError as e:
        raise RuntimeError(f"{' '.join(cmd)} did not return a Pod list: {e}") from e
    selector = log_selector(pods, deployment)
    if selector is None:
        raise RuntimeError(f"no Pod of Deployment {deployment} in namespace "
                           f"{namespace!r}")
    return selector


MANAGER_CONTAINER = "manager"
RECORDINGS_CONTAINER = "recordings"
RECORDING_DIR_ENV = "LEARNER_RECORDING_DIR"
RECORDING_DIR_FLAG = "--learner-recording-dir"


def recording_dir(pod):
    """Returns the recording directory the manager container was given, or ""."""
    for c in (pod.get("spec") or {}).get("containers") or []:
        if c.get("name") != MANAGER_CONTAINER:
            continue
        found = ""
        for e in c.get("env") or []:
            if e.get("name") == RECORDING_DIR_ENV:
                found = e.get("value") or ""
        # The flag wins over the variable, as it does in the controller.
        args = list(c.get("command") or []) + list(c.get("args") or [])
        for i, a in enumerate(args):
            if a.startswith(RECORDING_DIR_FLAG + "="):
                found = a.split("=", 1)[1]
            elif a == RECORDING_DIR_FLAG and i + 1 < len(args):
                found = args[i + 1]
        return found
    return ""


def pod_rows(pod_list, deployment=CONTROLLER_DEPLOYMENT):
    """Returns (name, phase, recording dir, reader container) per controller Pod."""
    rows = []
    for p in pod_list.get("items") or []:
        if not is_controller_pod(p, deployment):
            continue
        names = [c.get("name") for c in (p.get("spec") or {}).get("containers") or []]
        rows.append((
            p["metadata"].get("name") or "-",
            (p.get("status") or {}).get("phase") or "-",
            recording_dir(p) or "-",
            RECORDINGS_CONTAINER if RECORDINGS_CONTAINER in names else "-",
        ))
    return sorted(rows)


def main():
    args = sys.argv[1:]
    rows_mode = bool(args) and args[0] == "--pods"
    if rows_mode:
        args = args[1:]
    deployment = args[0] if args else CONTROLLER_DEPLOYMENT
    try:
        pods = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"controller_pods.py: not a Pod list: {e}", file=sys.stderr)
        return 1
    if rows_mode:
        # No controller Pod is an answer, not a failure: exit 0 with no rows,
        # so a caller can tell it from a list it could not read (exit 1).
        if not isinstance(pods, dict):
            print("controller_pods.py: not a Pod list", file=sys.stderr)
            return 1
        for r in pod_rows(pods, deployment):
            print("\t".join(r))
        return 0
    selector = log_selector(pods, deployment)
    if selector is None:
        print(f"controller_pods.py: no Pod of Deployment {deployment}", file=sys.stderr)
        return 1
    print(selector)
    return 0


if __name__ == "__main__":
    sys.exit(main())
