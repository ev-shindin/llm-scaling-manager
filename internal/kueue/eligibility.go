package kueue

import (
	"context"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

// ownerDepth is how far up a pod's controller chain the managed check looks for
// an object Kueue owns a Workload for. Four reaches a LeaderWorkerSet from a
// worker pod: worker StatefulSet, leader pod, leader StatefulSet, then the
// LeaderWorkerSet. A Job pod or a leader pod is matched within two.
const ownerDepth = 4

// NamespaceEligibility is the engine-side half of the B1 conditions for one
// namespace: the ones read from the namespace's own pods and ResourceQuotas
// rather than from Kueue's queues.
type NamespaceEligibility struct {
	// Reason is why borrowing must not be counted in the namespace
	// (ReasonNotKueueManaged, ReasonResourceQuotaUnreadable), ReasonStale when
	// a ResourceQuota read failed for a reason that may pass (the caller then
	// freezes this namespace's borrowing at its last figures), or empty. A
	// failure that lasts longer than unreadableAfter refresh intervals is no
	// longer stale but ReasonResourceQuotaUnreadable. The GPU bound condition
	// needs the queue figures and is the caller's.
	Reason Reason
	// GPUQuota is, per GPU resource name, the tightest hard limit any of the
	// namespace's ResourceQuotas sets on it (requests.<name> or <name>). Absent
	// when no ResourceQuota names it.
	GPUQuota map[string]int
	// EvaluatedAt is when the namespace was last evaluated.
	EvaluatedAt time.Time
	// managed is the last answer of the managed check, kept for a later
	// evaluation that finds no GPU pod.
	managed bool
	// askedAt is when the namespace was last asked about; Evaluate forgets
	// one not asked about for long.
	askedAt time.Time
	// dueAt is when the answer is next re-evaluated; zero asks again on the
	// next call.
	dueAt time.Time
	// failingSince is when the namespace's ResourceQuota reads began failing
	// for a reason that may pass; zero while they succeed.
	failingSince time.Time
}

// unreadableAfter is how many refresh intervals a ResourceQuota read may keep
// failing for a reason that may pass before the namespace is withheld as
// unreadable instead of frozen: a freeze is for a blip, and one tenant's
// failing reads must not hold that tenant's borrowing frozen indefinitely.
const unreadableAfter = 5

// Eligibility evaluates the engine-side B1 conditions per namespace, at most
// once per refresh interval per namespace, and keeps the answers. Its callers
// ask only about namespaces with managed variants (QuotaInventory), never
// about every namespace Kueue grants.
//
// Pods are listed through the client it is given, which in the controller is
// the manager's cached client: the GPU-usage walk already keeps a cluster-wide
// pod informer, so that list costs no API call. Workloads come from the
// Workload source (WorkloadsVia): in the controller an informer with an index
// on owner UID, so a pod's owners are looked up in memory; without one, one
// Workload list per namespace and evaluation. Owners and ResourceQuotas are
// read unstructured, straight from the API server, get/list only: per
// namespace and refresh interval one ResourceQuota list, and one get per
// distinct owner of a GPU pod no Workload below it matched.
type Eligibility struct {
	client   client.Client
	interval time.Duration
	now      func() time.Time
	gpuNames map[string]bool
	// workloads, when set, is the Workload source; nil reads through
	// WorkloadsVia(client) at the time of the read.
	workloads WorkloadSource

	mu   sync.Mutex
	byNS map[string]NamespaceEligibility
	// inflight holds, per namespace being evaluated, a channel closed when the
	// answer is stored, so a second caller waits for it instead of reading
	// the same namespace again.
	inflight map[string]chan struct{}
}

// NewEligibility returns an Eligibility over c. opts.Resources names the GPU
// resources a ResourceQuota is read for (empty: every vendor resource), and
// opts.RefreshInterval how long an answer is kept.
func NewEligibility(c client.Client, opts Options) *Eligibility {
	names := make(map[string]bool, len(opts.Resources))
	for _, r := range opts.Resources {
		names[r] = true
	}
	if len(names) == 0 {
		for _, v := range constants.VendorResources {
			names[v.ResourceName] = true
		}
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	return &Eligibility{client: c, interval: opts.RefreshInterval, now: now, gpuNames: names, workloads: opts.Workloads,
		byNS: map[string]NamespaceEligibility{}, inflight: map[string]chan struct{}{}}
}

// workloadSource is the source Workloads are read through.
func (e *Eligibility) workloadSource() WorkloadSource {
	if e.workloads != nil {
		return e.workloads
	}
	return WorkloadsVia(e.client)
}

// Evaluate returns the answer for each namespace, re-evaluating the ones whose
// answer is due. An answer of ReasonStale (a read that may pass) is
// re-evaluated on the next call. A namespace not asked about for forgetAfter
// is forgotten, so a deleted namespace leaves nothing behind; one that drops
// out of the candidates only briefly keeps its last answer.
//
// No lock is held across an API read: the due namespaces are claimed under
// e.mu, evaluated without it, and their answers merged under it again. Both
// engines share one Eligibility; a namespace another caller is evaluating is
// answered from its previous answer when there is one, and otherwise waited
// for (single flight), never read twice.
//
// Re-evaluations are spread over the refresh interval rather than all due
// together: a namespace's first answer is due again after a share of the
// interval fixed by its name (spreadDue), each later one a full interval
// after it was evaluated.
func (e *Eligibility) Evaluate(ctx context.Context, namespaces []string) map[string]NamespaceEligibility {
	type job struct {
		ns   string
		prev NamespaceEligibility
		seen bool
	}
	out := make(map[string]NamespaceEligibility, len(namespaces))
	var jobs []job
	waits := map[string]chan struct{}{}

	e.mu.Lock()
	now := e.now()
	for _, ns := range namespaces {
		prev, seen := e.byNS[ns]
		if seen && e.interval > 0 && !prev.dueAt.IsZero() && now.Before(prev.dueAt) {
			prev.askedAt = now
			e.byNS[ns] = prev
			out[ns] = prev
			continue
		}
		if ch, busy := e.inflight[ns]; busy {
			if seen {
				out[ns] = prev
			} else {
				waits[ns] = ch
			}
			continue
		}
		e.inflight[ns] = make(chan struct{})
		jobs = append(jobs, job{ns, prev, seen})
	}
	e.mu.Unlock()

	got := make([]NamespaceEligibility, len(jobs))
	for i, j := range jobs {
		got[i] = e.evaluate(ctx, j.ns, j.prev, j.seen, now)
	}

	e.mu.Lock()
	for i, j := range jobs {
		g := got[i]
		if g.Reason != ReasonStale && g.failingSince.IsZero() {
			g.EvaluatedAt = now
			g.dueAt = now.Add(e.interval)
			if !j.seen {
				g.dueAt = now.Add(spreadDue(j.ns, e.interval))
			}
		}
		g.askedAt = now
		e.byNS[j.ns] = g
		out[j.ns] = g
		close(e.inflight[j.ns])
		delete(e.inflight, j.ns)
	}
	forget := max(forgetAfter, 10*e.interval)
	for ns, el := range e.byNS {
		if _, busy := e.inflight[ns]; !busy && now.Sub(el.askedAt) > forget {
			delete(e.byNS, ns)
		}
	}
	e.mu.Unlock()

	for ns, ch := range waits {
		select {
		case <-ch:
			e.mu.Lock()
			out[ns] = e.byNS[ns]
			e.mu.Unlock()
		case <-ctx.Done():
			// No answer in time: stale, which withholds a namespace that
			// never borrowed and freezes one that did.
			out[ns] = NamespaceEligibility{Reason: ReasonStale}
		}
	}
	return out
}

// spreadDue is when a namespace's first answer is due again: between half the
// interval and the whole of it, by a hash of its name, so namespaces first
// evaluated together fall due at different cycles from then on.
func spreadDue(ns string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(ns))
	half := interval / 2
	// The hash as a fraction of 2^32 picks the point within the half.
	return half + time.Duration(float64(half)*float64(h.Sum32())/(1<<32))
}

// forgetAfter is the least time an answer is kept for a namespace no longer
// asked about; at least ten refresh intervals.
const forgetAfter = time.Hour

// evaluate runs both checks for one namespace.
func (e *Eligibility) evaluate(ctx context.Context, ns string, prev NamespaceEligibility, seen bool, now time.Time) NamespaceEligibility {
	got := NamespaceEligibility{}
	managed, known := e.managed(ctx, ns)
	switch {
	case known:
		got.managed = managed
	case seen:
		// No GPU pod to judge by, or the pods could not be listed: the namespace
		// keeps its previous answer. A namespace that scaled to zero must not
		// flip to "managed" for want of evidence against it.
		got.managed = prev.managed
	}
	quota, err := e.gpuQuota(ctx, ns)
	if err != nil && !apierrors.IsForbidden(err) {
		got.failingSince = now
		if seen && !prev.failingSince.IsZero() {
			got.failingSince = prev.failingSince
		}
	}
	failingTooLong := e.interval > 0 && !got.failingSince.IsZero() &&
		now.Sub(got.failingSince) >= unreadableAfter*e.interval
	switch {
	case !got.managed:
		got.Reason = ReasonNotKueueManaged
	case apierrors.IsForbidden(err):
		// Never "no ResourceQuota": a ResourceQuota that could not be read may
		// be the very bound borrowing relies on. Forbidden lasts until someone
		// grants the read, so the answer is kept for the interval.
		got.Reason = ReasonResourceQuotaUnreadable
	case failingTooLong:
		// Failing for unreadableAfter intervals is no blip: withheld, as for
		// Forbidden. Asked again on the next call, so it recovers as soon as
		// a read passes.
		got.Reason = ReasonResourceQuotaUnreadable
		quota = prev.GPUQuota
	case err != nil:
		// A read that may pass by the next cycle: stale, which freezes this
		// namespace's borrowing rather than dropping it to nominal, and is
		// not kept for the interval (Evaluate asks again next call). The last
		// figures read stand in meanwhile.
		got.Reason = ReasonStale
		quota = prev.GPUQuota
	}
	got.GPUQuota = quota
	return got
}

// managed reports whether every GPU pod in ns is charged to Kueue, and whether
// that was known: false when the namespace has no GPU pod, or its pods or its
// Workloads could not be listed for a reason that may pass.
//
// A pod counts only when Kueue has a Workload for it: a Workload owned by the
// pod itself (the pod integration), or by an object up its controller chain (a
// Job, a LeaderWorkerSet), that has quota reserved or is admitted. A pod behind
// Kueue's admission gate counts when such a Workload exists at all, reserved or
// not: Kueue creates the Workload before it lifts the gate, and a gated pod
// holds no GPU.
//
// Kueue's managed label, its admission gate and the queue-name label are not
// proof on their own. A tenant can write each of them into its own pod template,
// and in a namespace Kueue's webhook does not cover the pod then runs uncharged
// while borrowing is unlocked.
//
// The charge must also cover the GPUs, each Workload counted once: owners
// linked by a Workload that names them all form one charge group, and the
// reserved Workloads of a group must request at least what the running pods of
// all its owners request. A tenant allowed to create Workloads could otherwise
// make a 1-GPU one owned by its own LeaderWorkerSet, have Kueue admit it, and
// pass the check for every pod of a 16-GPU-per-group set; or make one 8-GPU
// Workload naming ten 8-GPU pods, or several sets, as owners and have it
// credited in full to each. A genuine pod group names every pod of the group
// and its pod sets cover them all; a LeaderWorkerSet's Workloads, one per
// group, are all owned by the set. For a single pod the group is the pod, so
// the check is exact per pod. A pod at the admission gate or being deleted
// holds no GPU of its own, so it adds nothing to its group's figure.
//
// Workloads are looked up by owner (workloadIndex), only when the namespace
// has a GPU pod; an owner is read only when no Workload matched below it.
func (e *Eligibility) managed(ctx context.Context, ns string) (bool, bool) {
	var pods corev1.PodList
	if err := e.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return false, false
	}
	owners := map[types.UID]*unstructured.Unstructured{}
	var idx *workloadIndex
	used := map[types.UID]int64{} // per charging owner
	found := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || resources.PodGPURequests(pod) <= 0 {
			continue
		}
		if StoppedByKueue(pod) {
			// Kueue is deleting it (a preemption) and has usually removed its
			// Workload already: judged as unmanaged, it withheld the whole
			// namespace for the pod's grace period, measured at ~30s live.
			continue
		}
		found = true
		if idx == nil {
			var known bool
			if idx, known = e.newWorkloadIndex(ctx, ns); !known {
				return false, false
			}
		}
		owner, ok, known := e.chargingOwner(ctx, ns, pod, owners, idx)
		if !known {
			return false, false
		}
		if !ok {
			return false, true
		}
		if !PodGated(pod) && pod.DeletionTimestamp == nil {
			used[owner] += podGPUs(&pod.Spec, e.gpuNames)
		}
	}
	byGroup := map[types.UID]int64{}
	for owner, n := range used {
		group, known := idx.group(owner)
		if !known {
			return false, false
		}
		byGroup[group] += n
	}
	for group, n := range byGroup {
		if n > idx.groupGPUs[group] {
			return false, true // the reserved Workloads cover fewer GPUs than the pods hold
		}
	}
	return found, found
}

// StoppedByKueue reports whether Kueue is deleting pod: it is terminating and
// carries Kueue's TerminationTarget condition. A terminating pod without that
// condition still counts, so a tenant cannot hide a GPU pod from the managed
// check by holding it in termination with a finalizer; pod conditions sit in
// the status subresource, which tenants do not write.
func StoppedByKueue(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp == nil {
		return false
	}
	return slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return string(c.Type) == TerminationTargetCondition && c.Status == corev1.ConditionTrue
	})
}

// PodGated reports whether a pod is held behind Kueue's admission gate.
func PodGated(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool {
		return g.Name == AdmissionGate
	})
}

// chargingOwner finds the Workload Kueue charges for pod: one owned by the pod
// or by an object up its controller chain (at most ownerDepth objects),
// reserved or admitted, or, for a pod behind the admission gate, any such
// Workload. It returns the UID of the object the Workload is owned by, and
// false when there is none. An owner that cannot be read ends the walk: not
// charged. known is false when a Workload lookup failed for a reason that may
// pass.
func (e *Eligibility) chargingOwner(ctx context.Context, ns string, pod *corev1.Pod,
	cache map[types.UID]*unstructured.Unstructured, idx *workloadIndex) (types.UID, bool, bool) {
	gated := PodGated(pod)
	charged := func(uid types.UID) (bool, bool) {
		ws, known := idx.owned(uid)
		if !known {
			return false, false
		}
		reserved := slices.ContainsFunc(ws, func(w *workloadCharge) bool { return w.reserved })
		return reserved || (gated && len(ws) > 0), true
	}
	if ok, known := charged(pod.UID); !known || ok {
		return pod.UID, ok, known
	}
	ref := metav1.GetControllerOf(pod)
	for depth := 0; ref != nil && depth < ownerDepth; depth++ {
		if ok, known := charged(ref.UID); !known || ok {
			return ref.UID, ok, known
		}
		owner, ok := cache[ref.UID]
		if !ok {
			owner = &unstructured.Unstructured{}
			owner.SetGroupVersionKind(schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind))
			if err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, owner); err != nil {
				owner = nil
			}
			cache[ref.UID] = owner
		}
		if owner == nil {
			return "", false, true
		}
		ref = metav1.GetControllerOf(owner)
	}
	return "", false, true
}

// workloadCharge is one Workload as the managed check counts it.
type workloadCharge struct {
	// name identifies it within the namespace: two lookups by different
	// owners return the same Workload, which counts once.
	name string
	// gpus is what it requests in all (workloadGPUs).
	gpus int64
	// reserved: Kueue reserved quota for it, or admitted it.
	reserved bool
	// owners are its owners of a kind a Kueue integration owns Workloads by.
	owners []types.UID
}

// workloadIndex answers, for one namespace and evaluation, which Workloads an
// owner has, and which owners they link into a charge group: the owners one or
// more Workloads name together. Each group's figure is the GPUs its reserved
// Workloads request in all, each Workload once.
//
// With an indexed source each owner is one in-memory lookup; otherwise the
// namespace is listed once and indexed here. Lookups are memoized.
//
// Only a Workload Kueue itself would create counts: one that requests a GPU
// in some pod set, and only for owners of a kind a Kueue integration owns a
// Workload by (integratedOwner). A tenant allowed to create Workloads could
// otherwise make a tiny CPU-only one owned by its own Deployment or
// ReplicaSet, have Kueue admit it, and pass the check for GPU pods Kueue never
// charged; a GPU one too small for the pods is caught by managed.
type workloadIndex struct {
	ctx      context.Context
	src      WorkloadSource
	ns       string
	gpuNames map[string]bool
	// whole is set when byOwner holds every Workload of the namespace.
	whole     bool
	byOwner   map[types.UID][]*workloadCharge
	looked    map[types.UID]bool
	charges   map[string]*workloadCharge
	groupOf   map[types.UID]types.UID
	groupGPUs map[types.UID]int64
}

// newWorkloadIndex prepares the Workload lookups of ns. known is false when
// the Workloads could not be read for a reason that may pass; a Workload kind
// the cluster does not serve, or a Forbidden read, is known and empty, which
// leaves every GPU pod uncharged.
func (e *Eligibility) newWorkloadIndex(ctx context.Context, ns string) (*workloadIndex, bool) {
	idx := &workloadIndex{ctx: ctx, src: e.workloadSource(), ns: ns, gpuNames: e.gpuNames,
		byOwner: map[types.UID][]*workloadCharge{}, looked: map[types.UID]bool{},
		charges: map[string]*workloadCharge{}, groupOf: map[types.UID]types.UID{}, groupGPUs: map[types.UID]int64{}}
	if idx.src.Indexed() {
		return idx, true
	}
	items, err := idx.src.Workloads(ctx, ns, "")
	idx.whole = true
	if err != nil {
		return idx, workloadsAbsent(err)
	}
	for i := range items {
		idx.add(&items[i])
	}
	return idx, true
}

// integratedOwner reports whether ref names a kind, in its API group, a Kueue
// integration owns a Workload by (integratedOwners).
func integratedOwner(ref metav1.OwnerReference) bool {
	group, ok := integratedOwners[ref.Kind]
	if !ok {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == group
}

// workloadsAbsent reports whether a failed Workload read settles the question
// as "none": Kueue does not serve the kind, or the read is Forbidden.
func workloadsAbsent(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsForbidden(err)
}

// add indexes one Workload under each of its integrated owners, once.
func (idx *workloadIndex) add(w *unstructured.Unstructured) {
	if _, dup := idx.charges[w.GetName()]; dup {
		return
	}
	gpus := workloadGPUs(w, idx.gpuNames)
	if gpus <= 0 {
		return
	}
	c := &workloadCharge{name: w.GetName(), gpus: gpus, reserved: workloadReserved(w)}
	for _, ref := range w.GetOwnerReferences() {
		if integratedOwner(ref) {
			c.owners = append(c.owners, ref.UID)
		}
	}
	if len(c.owners) == 0 {
		return
	}
	idx.charges[c.name] = c
	for _, o := range c.owners {
		idx.byOwner[o] = append(idx.byOwner[o], c)
	}
}

// owned returns the counted Workloads owned by uid. known is false when the
// lookup failed for a reason that may pass.
func (idx *workloadIndex) owned(uid types.UID) ([]*workloadCharge, bool) {
	if idx.whole || idx.looked[uid] {
		return idx.byOwner[uid], true
	}
	items, err := idx.src.Workloads(idx.ctx, idx.ns, uid)
	if err != nil {
		if !workloadsAbsent(err) {
			return nil, false
		}
		// Known and empty for every owner from now on.
		idx.whole = true
		return nil, true
	}
	idx.looked[uid] = true
	for i := range items {
		idx.add(&items[i])
	}
	return idx.byOwner[uid], true
}

// group returns the charge group of owner: the owners its Workloads link,
// transitively, named by the smallest UID among them. Its figure lands in
// groupGPUs. known is false when a lookup failed for a reason that may pass.
func (idx *workloadIndex) group(owner types.UID) (types.UID, bool) {
	if g, ok := idx.groupOf[owner]; ok {
		return g, true
	}
	members := []types.UID{owner}
	visited := map[types.UID]bool{owner: true}
	counted := map[string]bool{}
	var gpus int64
	for i := 0; i < len(members); i++ {
		ws, known := idx.owned(members[i])
		if !known {
			return "", false
		}
		for _, w := range ws {
			if counted[w.name] {
				continue
			}
			counted[w.name] = true
			if w.reserved {
				gpus += w.gpus
			}
			for _, o := range w.owners {
				if !visited[o] {
					visited[o] = true
					members = append(members, o)
				}
			}
		}
	}
	rep := slices.Min(members)
	for _, m := range members {
		idx.groupOf[m] = rep
	}
	idx.groupGPUs[rep] = gpus
	return rep, true
}

// workloadGPUs is the GPUs a Workload requests in all: per pod set, its count
// times what one of its pods requests (podGPUs). The count is the admitted
// one, status.admission.podSetAssignments[].count for the pod set of that
// name, when Kueue wrote it -- with partial admission (minCount) Kueue admits,
// and charges, fewer pods than the spec asks for -- else spec.podSets[].count,
// 1 when unset. A pod set whose template does not parse counts nothing.
//
// Pod-level resources (spec.resources, PodLevelResources) and GPUs claimed
// through DRA are not seen, here or in the pods: a Workload or pod that asks
// for its GPUs only that way counts none, so its pods read as not charged
// (fail safe), never as charged.
func workloadGPUs(w *unstructured.Unstructured, gpuNames map[string]bool) int64 {
	admitted := map[string]int64{}
	assignments, _, _ := unstructured.NestedSlice(w.Object, "status", "admission", "podSetAssignments")
	for _, raw := range assignments {
		a, _ := raw.(map[string]interface{})
		name, _, _ := unstructured.NestedString(a, "name")
		if count, found, err := unstructured.NestedInt64(a, "count"); found && err == nil {
			admitted[name] = count
		}
	}
	podSets, _, _ := unstructured.NestedSlice(w.Object, "spec", "podSets")
	var total int64
	for _, raw := range podSets {
		ps, _ := raw.(map[string]interface{})
		spec, _, _ := unstructured.NestedMap(ps, "template", "spec")
		var pod corev1.PodSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod); err != nil {
			continue
		}
		name, _, _ := unstructured.NestedString(ps, "name")
		count, ok := admitted[name]
		if !ok {
			var found bool
			var err error
			if count, found, err = unstructured.NestedInt64(ps, "count"); !found || err != nil {
				count = 1
			}
		}
		total += max(count, 0) * podGPUs(&pod, gpuNames)
	}
	return total
}

// podGPUs is the GPUs of the named resources one pod of spec requests, as the
// scheduler accounts them: regular containers add up, and init containers,
// which run one at a time before them, count only the largest. A container
// that sets only a limit on an extended resource requests that limit.
func podGPUs(spec *corev1.PodSpec, gpuNames map[string]bool) int64 {
	of := func(c *corev1.Container) int64 {
		var n int64
		for name := range gpuNames {
			rn := corev1.ResourceName(name)
			q, ok := c.Resources.Requests[rn]
			if !ok {
				q, ok = c.Resources.Limits[rn]
			}
			if ok {
				n += q.Value()
			}
		}
		return n
	}
	var regular, initMax int64
	for i := range spec.Containers {
		regular += of(&spec.Containers[i])
	}
	for i := range spec.InitContainers {
		initMax = max(initMax, of(&spec.InitContainers[i]))
	}
	return max(regular, initMax)
}

// workloadReserved reports whether Kueue has reserved quota for a Workload or
// admitted it: status.admission is set, or condition QuotaReserved or Admitted
// is True. Status is Kueue's to write, not the tenant's.
func workloadReserved(w *unstructured.Unstructured) bool {
	if adm, found, _ := unstructured.NestedMap(w.Object, "status", "admission"); found && adm != nil {
		return true
	}
	conds, _, _ := unstructured.NestedSlice(w.Object, "status", "conditions")
	for _, raw := range conds {
		cond, _ := raw.(map[string]interface{})
		if (cond["type"] == WorkloadQuotaReserved || cond["type"] == WorkloadAdmitted) && cond["status"] == string(corev1.ConditionTrue) {
			return true
		}
	}
	return false
}

// gpuQuota reads the namespace's ResourceQuotas, unstructured so the read goes
// to the API server and needs no watch, and returns per GPU resource the
// tightest hard limit. Any failure is returned: the caller reads it as
// unreadable, never as absent.
func (e *Eligibility) gpuQuota(ctx context.Context, ns string) (map[string]int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ResourceQuotaList"})
	if err := e.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("kueue: listing ResourceQuotas in %q: %w", ns, err)
	}
	var out map[string]int
	for _, rq := range list.Items {
		hard, _, _ := unstructured.NestedStringMap(rq.Object, "spec", "hard")
		for key, raw := range hard {
			name := strings.TrimPrefix(key, "requests.")
			if !e.gpuNames[name] {
				continue
			}
			q, err := resource.ParseQuantity(raw)
			if err != nil {
				continue
			}
			n := clampQuota(q.Value())
			if out == nil {
				out = map[string]int{}
			}
			if prev, ok := out[name]; !ok || n < prev {
				out[name] = n
			}
		}
	}
	return out, nil
}

// PreemptedWorkload is a Workload Kueue evicted to preempt it: condition
// Evicted=True with reason Preempted.
type PreemptedWorkload struct {
	UID types.UID
	// Name is the Workload's name; for a pod group, the group's name.
	Name string
	// Owners are the names of the objects the Workload is owned by: a pod for
	// the pod integration, a LeaderWorkerSet or Job otherwise.
	Owners []string
	// OwnerRefs are the same owners with their kind and UID, so a caller can
	// match a LeaderWorkerSet or a pod exactly rather than by name.
	OwnerRefs []metav1.OwnerReference
	At        time.Time
}

// PreemptedWorkloads lists the Workloads in ns that Kueue preempted, read
// through src: the Workload informer in the controller (WorkloadsVia), so a
// cycle's check costs no API call once it has synced. Any failure is returned;
// the caller withholds new borrowing on it.
func PreemptedWorkloads(ctx context.Context, src WorkloadSource, ns string) ([]PreemptedWorkload, error) {
	items, err := src.Workloads(ctx, ns, "")
	if err != nil {
		return nil, err
	}
	var out []PreemptedWorkload
	for _, w := range items {
		conds, _, _ := unstructured.NestedSlice(w.Object, "status", "conditions")
		for _, raw := range conds {
			cond, _ := raw.(map[string]interface{})
			if cond["type"] != WorkloadEvicted || cond["status"] != string(corev1.ConditionTrue) || cond["reason"] != WorkloadEvictedByPreemption {
				continue
			}
			at, _ := time.Parse(time.RFC3339, fmt.Sprint(cond["lastTransitionTime"]))
			pw := PreemptedWorkload{UID: w.GetUID(), Name: w.GetName(), At: at, OwnerRefs: w.GetOwnerReferences()}
			for _, ref := range pw.OwnerRefs {
				pw.Owners = append(pw.Owners, ref.Name)
			}
			out = append(out, pw)
		}
	}
	return out, nil
}
