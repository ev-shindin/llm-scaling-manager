package kueue

import (
	"context"
	"fmt"
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

// KindWorkload is Kueue's Workload kind, read by the managed check.
const KindWorkload = "Workload"

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
// once per refresh interval per namespace, and keeps the answers.
//
// Pods are listed through the client it is given, which in the controller is
// the manager's cached client: the GPU-usage walk already keeps a cluster-wide
// pod informer, so that list costs no API call. Owners, Workloads and
// ResourceQuotas are read unstructured, which this project never caches, so they
// go to the API server directly and need only get/list (no watch): per
// namespace and interval one ResourceQuota list, at most one Workload list, and
// one get per distinct owner of a GPU pod no Workload below it matched.
type Eligibility struct {
	client   client.Client
	interval time.Duration
	now      func() time.Time
	gpuNames map[string]bool

	mu   sync.Mutex
	byNS map[string]NamespaceEligibility
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
	return &Eligibility{client: c, interval: opts.RefreshInterval, now: now, gpuNames: names, byNS: map[string]NamespaceEligibility{}}
}

// Evaluate returns the answer for each namespace, re-evaluating the ones whose
// answer is older than the refresh interval. An answer of ReasonStale (a read
// that may pass) is re-evaluated on the next call. A namespace not asked about
// for forgetAfter is forgotten, so a deleted namespace leaves nothing behind;
// one that drops out of the candidates only briefly keeps its last answer.
//
// e.mu is held across the API reads on purpose: both engines share one
// Eligibility, and the second caller in an interval must wait for the first
// one's answers rather than read the same namespaces again (single flight).
func (e *Eligibility) Evaluate(ctx context.Context, namespaces []string) map[string]NamespaceEligibility {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	out := make(map[string]NamespaceEligibility, len(namespaces))
	for _, ns := range namespaces {
		prev, seen := e.byNS[ns]
		if seen && e.interval > 0 && !prev.EvaluatedAt.IsZero() && now.Sub(prev.EvaluatedAt) < e.interval {
			prev.askedAt = now
			e.byNS[ns] = prev
			out[ns] = prev
			continue
		}
		got := e.evaluate(ctx, ns, prev, seen, now)
		if got.Reason != ReasonStale && got.failingSince.IsZero() {
			got.EvaluatedAt = now
		}
		got.askedAt = now
		e.byNS[ns] = got
		out[ns] = got
	}
	forget := max(forgetAfter, 10*e.interval)
	for ns, el := range e.byNS {
		if now.Sub(el.askedAt) > forget {
			delete(e.byNS, ns)
		}
	}
	return out
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
// The charge must also cover the GPUs: per charging owner, the reserved
// Workloads' GPUs in all must be at least what its running pods request. A
// tenant allowed to create Workloads could otherwise make a 1-GPU one owned by
// its own LeaderWorkerSet, have Kueue admit it, and pass the check for every
// pod of a 16-GPU-per-group set. For the pod integration the owner is the pod
// itself, so the check is exact per pod. A pod at the admission gate or being
// deleted holds no GPU of its own, so it adds nothing to its owner's figure.
//
// One Workload list per evaluation, taken only when the namespace has a GPU
// pod; an owner is read only when no Workload matched below it.
func (e *Eligibility) managed(ctx context.Context, ns string) (bool, bool) {
	var pods corev1.PodList
	if err := e.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return false, false
	}
	owners := map[types.UID]*unstructured.Unstructured{}
	var workloads map[types.UID]ownerCharge
	used := map[types.UID]int64{}
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
		if workloads == nil {
			var known bool
			if workloads, known = e.workloadsByOwner(ctx, ns); !known {
				return false, false
			}
		}
		owner, ok := e.chargingOwner(ctx, ns, pod, owners, workloads)
		if !ok {
			return false, true
		}
		if !PodGated(pod) && pod.DeletionTimestamp == nil {
			used[owner] += podGPUs(&pod.Spec, e.gpuNames)
		}
	}
	for owner, n := range used {
		if n > workloads[owner].gpus {
			return false, true // the reserved Workloads cover fewer GPUs than the pods hold
		}
	}
	return found, found
}

// TerminationTargetCondition is the pod condition Kueue sets on a pod it is
// about to delete, for example on a preemption.
const TerminationTargetCondition = "TerminationTarget"

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
// charged.
func (e *Eligibility) chargingOwner(ctx context.Context, ns string, pod *corev1.Pod,
	cache map[types.UID]*unstructured.Unstructured, workloads map[types.UID]ownerCharge) (types.UID, bool) {
	gated := PodGated(pod)
	charged := func(uid types.UID) bool {
		c, exists := workloads[uid]
		return c.reserved || (gated && exists)
	}
	if charged(pod.UID) {
		return pod.UID, true
	}
	ref := metav1.GetControllerOf(pod)
	for depth := 0; ref != nil && depth < ownerDepth; depth++ {
		if charged(ref.UID) {
			return ref.UID, true
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
			return "", false
		}
		ref = metav1.GetControllerOf(owner)
	}
	return "", false
}

// ownerCharge is what the Workloads owned by one object charge: whether one of
// them is reserved or admitted, and the GPUs the reserved ones request in all.
type ownerCharge struct {
	reserved bool
	gpus     int64
}

// workloadsByOwner maps the UID of every object a Workload in ns names as an
// owner to what its Workloads charge. known is false when the list failed for
// a reason that may pass; a Workload kind the cluster does not serve, or a
// Forbidden list, is known and empty, which leaves every GPU pod uncharged.
//
// Only a Workload Kueue itself would create counts: one that requests a GPU
// in some pod set, and only for owners of a kind a Kueue integration owns a
// Workload by (integratedOwner). A tenant allowed to create Workloads could
// otherwise make a tiny CPU-only one owned by its own Deployment or
// ReplicaSet, have Kueue admit it, and pass the check for GPU pods Kueue never
// charged; a GPU one too small for the pods is caught by managed.
func (e *Eligibility) workloadsByOwner(ctx context.Context, ns string) (map[types.UID]ownerCharge, bool) {
	out := map[types.UID]ownerCharge{}
	mapping, err := e.client.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: KindWorkload})
	if err != nil {
		return out, meta.IsNoMatchError(err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mapping.GroupVersionKind.GroupVersion().WithKind(KindWorkload + "List"))
	if err := e.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return out, apierrors.IsForbidden(err) || meta.IsNoMatchError(err)
	}
	for i := range list.Items {
		w := &list.Items[i]
		gpus := workloadGPUs(w, e.gpuNames)
		if gpus <= 0 {
			continue
		}
		reserved := workloadReserved(w)
		for _, ref := range w.GetOwnerReferences() {
			if !integratedOwner(ref) {
				continue
			}
			c := out[ref.UID]
			if reserved {
				c.reserved = true
				c.gpus += gpus
			}
			out[ref.UID] = c
		}
	}
	return out, true
}

// integratedOwners are the owner kinds Kueue's integrations set on the
// Workloads they create, by API group: the pod integration (a Deployment's
// pods too) owns it by the Pod, batch/v1 and JobSet by the Job or JobSet, and
// the LeaderWorkerSet and StatefulSet integrations by the set itself (checked
// against Kueue v0.20.0's reconcilers).
var integratedOwners = map[string]string{
	"Pod":             "",
	"Job":             "batch",
	"JobSet":          "jobset.x-k8s.io",
	"LeaderWorkerSet": "leaderworkerset.x-k8s.io",
	"StatefulSet":     "apps",
}

// integratedOwner reports whether ref names a kind, in its API group, a Kueue
// integration owns a Workload by.
func integratedOwner(ref metav1.OwnerReference) bool {
	group, ok := integratedOwners[ref.Kind]
	if !ok {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == group
}

// workloadGPUs is the GPUs a Workload requests in all: per pod set, its count
// times what one of its pods requests (podGPUs). A pod set whose template
// does not parse counts nothing.
//
// Pod-level resources (spec.resources, PodLevelResources) and GPUs claimed
// through DRA are not seen, here or in the pods: a Workload or pod that asks
// for its GPUs only that way counts none, so its pods read as not charged
// (fail safe), never as charged.
func workloadGPUs(w *unstructured.Unstructured, gpuNames map[string]bool) int64 {
	podSets, _, _ := unstructured.NestedSlice(w.Object, "spec", "podSets")
	var total int64
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
		if (cond["type"] == "QuotaReserved" || cond["type"] == "Admitted") && cond["status"] == "True" {
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

// PreemptedWorkloads lists the Workloads in ns that Kueue preempted. Read
// unstructured, so straight from the API server with get/list.
func PreemptedWorkloads(ctx context.Context, c client.Client, ns string) ([]PreemptedWorkload, error) {
	mapping, err := c.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: KindWorkload})
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mapping.GroupVersionKind.GroupVersion().WithKind(KindWorkload + "List"))
	if err := c.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	var out []PreemptedWorkload
	for _, w := range list.Items {
		conds, _, _ := unstructured.NestedSlice(w.Object, "status", "conditions")
		for _, raw := range conds {
			cond, _ := raw.(map[string]interface{})
			if cond["type"] != "Evicted" || cond["status"] != "True" || cond["reason"] != "Preempted" {
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
