package steadystate

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// DefaultPreemptionWatchCapacity bounds the preemptions a PreemptionWatch
// holds between two engine cycles. A cycle drains it; past the bound the
// oldest are dropped and counted.
const DefaultPreemptionWatchCapacity = 1024

// kindPod is the Pod kind, as owner references and Events name it.
const kindPod = "Pod"

// observedPreemption is one Kueue-managed pod seen carrying Kueue's
// TerminationTarget condition for a preemption, as the pod informer delivered
// it: on an update, or in its final state on delete.
type observedPreemption struct {
	Namespace string
	Name      string
	UID       types.UID
	Labels    map[string]string
	OwnerRefs []metav1.OwnerReference
	At        time.Time
}

// PreemptionWatch records Kueue's preemptions of managed pods from the
// manager's pod informer, so a preemption is seen however briefly the pod and
// its Workload live: measured against Kueue v0.20.0 on kind, the preempted pod
// and its Workload were both gone 1.4s after Kueue set TerminationTarget, well
// inside one 15s engine cycle, so polling either missed it. Kueue sets the
// condition (reason WorkloadEvictedDueToPreempted) before it deletes the pod,
// and the delete event carries it in the pod's final state.
//
// The handler only buffers; the engine drains the buffer once per cycle
// (observePreemptions), where the pods are matched to models. Safe for
// concurrent use.
type PreemptionWatch struct {
	mu       sync.Mutex
	capacity int
	items    []observedPreemption
	index    map[types.UID]bool
	dropped  int
}

// NewPreemptionWatch returns a watch holding at most capacity preemptions
// between drains; capacity <= 0 means DefaultPreemptionWatchCapacity.
func NewPreemptionWatch(capacity int) *PreemptionWatch {
	if capacity <= 0 {
		capacity = DefaultPreemptionWatchCapacity
	}
	return &PreemptionWatch{capacity: capacity, index: map[types.UID]bool{}}
}

// Handler is the informer event handler: updates and deletes, the latter
// including a DeletedFinalStateUnknown tombstone. Adds carry no preemption --
// Kueue marks a pod that already exists -- but a resync that delivers a marked
// pod as an add is recorded as well.
func (w *PreemptionWatch) Handler() toolscache.ResourceEventHandlerFuncs {
	return toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.observe(obj) },
		UpdateFunc: func(_, obj interface{}) { w.observe(obj) },
		DeleteFunc: func(obj interface{}) {
			if tomb, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			w.observe(obj)
		},
	}
}

// observe buffers obj when it is a Kueue-managed pod marked for termination by
// a preemption; anything else is ignored. A pod already buffered is not added
// again.
func (w *PreemptionWatch) observe(obj interface{}) {
	p, ok := obj.(*corev1.Pod)
	if !ok || p == nil || p.Labels[kueue.ManagedLabel] != "true" {
		return
	}
	at, ok := preemptedPod(p)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.index[p.UID] {
		return
	}
	if len(w.items) >= w.capacity {
		delete(w.index, w.items[0].UID)
		w.items = w.items[1:]
		w.dropped++
	}
	labels := make(map[string]string, len(p.Labels))
	for k, v := range p.Labels {
		labels[k] = v
	}
	w.items = append(w.items, observedPreemption{
		Namespace: p.Namespace, Name: p.Name, UID: p.UID, Labels: labels,
		OwnerRefs: append([]metav1.OwnerReference(nil), p.OwnerReferences...), At: at,
	})
	w.index[p.UID] = true
}

// drain returns the buffered preemptions, oldest first, and how many were
// dropped past the bound since the last drain, and empties the buffer.
func (w *PreemptionWatch) drain() ([]observedPreemption, int) {
	if w == nil {
		return nil, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	items, dropped := w.items, w.dropped
	w.items, w.dropped = nil, 0
	w.index = map[types.UID]bool{}
	return items, dropped
}

// preemptionKey identifies one preemption for counting: a pod group -- a
// LeaderWorkerSet group or a plain pod group, which Kueue admits and preempts
// as one Workload named after the group -- by its group name, a single pod by
// its UID. The Workload read uses the same keys (workloadPreemptionKey), so a
// preemption seen both ways counts once.
func preemptionKey(ns string, uid types.UID, labels map[string]string) string {
	if g := labels[kueue.PodGroupNameLabel]; g != "" {
		return "group/" + ns + "/" + g
	}
	return "pod/" + string(uid)
}

// workloadPreemptionKey is preemptionKey for a preempted Workload: a
// single-pod Workload by its pod's UID, any other by its own name, which for a
// pod group is the group's name.
func workloadPreemptionKey(ns string, w kueue.PreemptedWorkload) string {
	var pods []types.UID
	for _, o := range w.OwnerRefs {
		if o.Kind == kindPod {
			pods = append(pods, o.UID)
		}
	}
	if len(pods) == 1 && len(w.OwnerRefs) == 1 {
		return "pod/" + string(pods[0])
	}
	if w.Name != "" {
		return "group/" + ns + "/" + w.Name
	}
	return "workload/" + string(w.UID)
}
