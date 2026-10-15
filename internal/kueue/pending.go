package kueue

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// PendingWorkload is a Workload waiting in a ClusterQueue: no quota reserved,
// active, not finished. Only what the lender-waiting signal needs.
type PendingWorkload struct {
	// Priority is spec.priority; 0 when unset.
	Priority int32
	// GPUs is, per GPU resource name, what the Workload requests in all: per
	// pod set, its count times what one pod requests.
	GPUs map[string]int64
}

// pendingRead is what one read of the Workloads gave the lender signal.
type pendingRead struct {
	// byQueue holds each ClusterQueue's pending Workloads.
	byQueue map[string][]PendingWorkload
	// reservedPriority is, per ClusterQueue, the highest spec.priority of a
	// Workload Kueue reserved quota for in it.
	reservedPriority map[string]int32
}

// readPending reads the Workloads for the lender-waiting signal: the pending
// ones of each ClusterQueue, attributed through the LocalQueue spec.queueName
// names in the Workload's namespace, and the highest priority Kueue has
// reserved quota for per ClusterQueue (status.admission.clusterQueue). It
// reads every namespace the source can see, or only opts.Namespace for a
// namespace-scoped controller, whose LocalQueues are listed there only.
//
// A failed read reads as "nothing pending": no lender waits, which is the
// side that never lowers a model. Kueue's own preemption still serves a
// lender that can reclaim. The failure is logged once until a read passes.
// Callers must hold r.mu.
func (r *Reader) readPending(ctx context.Context, localQueues []unstructured.Unstructured) pendingRead {
	out := pendingRead{byQueue: map[string][]PendingWorkload{}, reservedPriority: map[string]int32{}}
	src := r.opts.Workloads
	if src == nil {
		src = WorkloadsVia(r.client)
	}
	items, err := src.Workloads(ctx, r.opts.Namespace, "")
	if err != nil {
		if !r.warnedPending && ctx.Err() == nil {
			r.warnedPending = true
			kueueLogger(ctx).Info("Cannot read Kueue Workloads; no lender counts as waiting for capacity it lent until they can be read. Logged once until a read passes.",
				"error", err.Error())
		}
		return out
	}
	r.warnedPending = false
	queueOf := make(map[[2]string]string, len(localQueues))
	for i := range localQueues {
		cq, _, _ := unstructured.NestedString(localQueues[i].Object, "spec", "clusterQueue")
		queueOf[[2]string{localQueues[i].GetNamespace(), localQueues[i].GetName()}] = cq
	}
	for i := range items {
		w := &items[i]
		priority := workloadPriority(w)
		if workloadReserved(w) {
			if cq, _, _ := unstructured.NestedString(w.Object, "status", "admission", "clusterQueue"); cq != "" {
				if p, ok := out.reservedPriority[cq]; !ok || priority > p {
					out.reservedPriority[cq] = priority
				}
			}
			continue
		}
		if !workloadWaiting(w) {
			continue
		}
		lq, _, _ := unstructured.NestedString(w.Object, "spec", "queueName")
		cq := queueOf[[2]string{w.GetNamespace(), lq}]
		if cq == "" {
			continue
		}
		if gpus := workloadGPUsByResource(w, r.gpuNames); len(gpus) > 0 {
			out.byQueue[cq] = append(out.byQueue[cq], PendingWorkload{Priority: priority, GPUs: gpus})
		}
	}
	return out
}

// workloadPriority is spec.priority, 0 when unset.
func workloadPriority(w *unstructured.Unstructured) int32 {
	p, _, _ := unstructured.NestedInt64(w.Object, "spec", "priority")
	return int32(p)
}

// workloadWaiting reports whether a Workload without reserved quota is
// waiting to be admitted: active (spec.active not false) and not finished.
func workloadWaiting(w *unstructured.Unstructured) bool {
	if active, found, _ := unstructured.NestedBool(w.Object, "spec", "active"); found && !active {
		return false
	}
	conds, _, _ := unstructured.NestedSlice(w.Object, "status", "conditions")
	for _, raw := range conds {
		cond, _ := raw.(map[string]interface{})
		if cond["type"] == WorkloadFinished && cond["status"] == string(corev1.ConditionTrue) {
			return false
		}
	}
	return true
}

// workloadGPUsByResource is what a Workload requests of each named GPU
// resource: per pod set, spec.podSets[].count (1 when unset) times what one
// pod requests (podGPUs). Resources it does not request are absent.
func workloadGPUsByResource(w *unstructured.Unstructured, gpuNames map[string]bool) map[string]int64 {
	podSets, _, _ := unstructured.NestedSlice(w.Object, "spec", "podSets")
	var out map[string]int64
	for _, raw := range podSets {
		ps, _ := raw.(map[string]interface{})
		spec, _, _ := unstructured.NestedMap(ps, "template", "spec")
		var pod corev1.PodSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod); err != nil {
			continue
		}
		count, found, err := unstructured.NestedInt64(ps, "count")
		if !found || err != nil {
			count = 1
		}
		for name := range gpuNames {
			n := max(count, 0) * podGPUs(&pod, map[string]bool{name: true})
			if n <= 0 {
				continue
			}
			if out == nil {
				out = map[string]int64{}
			}
			out[name] += n
		}
	}
	return out
}

// lenderPending is what a lender waits for of one flavor and GPU resource:
// the largest GPU request among its pending Workloads that ask for the
// resource and fit within its unused nominal quota of key (Nominal minus
// Reserved), and the highest priority among those. A pending Workload has no
// flavor assigned yet, so it counts for every flavor of the queue that offers
// the resource. 0 when none qualifies: the lender does not wait.
//
// A Workload asking for more than the unused nominal would have to borrow
// itself, and one asking for another resource (CPU work, another vendor's
// GPUs) gains nothing from a released GPU, so neither makes a lender wait.
func lenderPending(q QueueState, key ResourceKey) (gpus int, priority int32) {
	r, ok := q.Resources[key]
	if !ok {
		return 0, 0
	}
	unused := int64(max(0, r.Nominal-r.Reserved))
	found := false
	for _, w := range q.PendingWorkloads {
		n := w.GPUs[key.Resource]
		if n <= 0 || n > unused {
			continue
		}
		if !found || w.Priority > priority {
			priority = w.Priority
		}
		found = true
		gpus = max(gpus, clampQuota(n))
	}
	return gpus, priority
}
