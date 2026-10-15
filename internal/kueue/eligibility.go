package kueue

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/resources"
)

// KindWorkload is Kueue's Workload kind, read by the managed check.
const KindWorkload = "Workload"

// ownerDepth is how far up a pod's controller chain the managed check looks
// for a queue-name label: the pod's own controller (a Job, a ReplicaSet, a
// StatefulSet) and that one's controller (a Deployment, a LeaderWorkerSet).
const ownerDepth = 2

// NamespaceEligibility is the engine-side half of the B1 conditions for one
// namespace: the ones read from the namespace's own pods and ResourceQuotas
// rather than from Kueue's queues.
type NamespaceEligibility struct {
	// Reason is why borrowing must not be counted in the namespace
	// (ReasonNotKueueManaged, ReasonResourceQuotaUnreadable), or empty. The GPU
	// bound condition needs the queue figures and is the caller's.
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
}

// Eligibility evaluates the engine-side B1 conditions per namespace, at most
// once per refresh interval per namespace, and keeps the answers.
//
// Pods are listed through the client it is given, which in the controller is
// the manager's cached client: the GPU-usage walk already keeps a cluster-wide
// pod informer, so that list costs no API call. Owners, Workloads and
// ResourceQuotas are read unstructured, which this project never caches, so they
// go to the API server directly and need only get/list (no watch): per
// namespace and interval one ResourceQuota list, at most one Workload list, and
// one get per distinct owner of an unlabelled GPU pod.
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
// answer is older than the refresh interval.
func (e *Eligibility) Evaluate(ctx context.Context, namespaces []string) map[string]NamespaceEligibility {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	out := make(map[string]NamespaceEligibility, len(namespaces))
	for _, ns := range namespaces {
		prev, seen := e.byNS[ns]
		if seen && e.interval > 0 && now.Sub(prev.EvaluatedAt) < e.interval {
			out[ns] = prev
			continue
		}
		got := e.evaluate(ctx, ns, prev, seen)
		got.EvaluatedAt = now
		e.byNS[ns] = got
		out[ns] = got
	}
	return out
}

// evaluate runs both checks for one namespace.
func (e *Eligibility) evaluate(ctx context.Context, ns string, prev NamespaceEligibility, seen bool) NamespaceEligibility {
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
	switch {
	case !got.managed:
		got.Reason = ReasonNotKueueManaged
	case err != nil:
		// Never "no ResourceQuota": a ResourceQuota that could not be read may
		// be the very bound borrowing relies on.
		got.Reason = ReasonResourceQuotaUnreadable
	}
	got.GPUQuota = quota
	return got
}

// managed reports whether every GPU pod in ns is managed by Kueue, and whether
// that was known: false when the namespace has no GPU pod or its pods could
// not be listed.
//
// A pod is managed when it carries Kueue's managed label or its admission
// gate, which record what Kueue did, not what was asked of it. A pod of a
// Kueue-managed Job or other integration carries neither (Kueue suspends its
// owner instead); it counts when its controller, or that one's controller,
// carries the queue-name label and a Workload in the namespace is owned by that
// object. A queue-name label alone is not proof: Kueue may have that integration
// off, or not select the namespace.
func (e *Eligibility) managed(ctx context.Context, ns string) (bool, bool) {
	var pods corev1.PodList
	if err := e.client.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return false, false
	}
	owners := map[types.UID]*unstructured.Unstructured{}
	var workloadOwners map[types.UID]bool
	found := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || resources.PodGPURequests(pod) <= 0 {
			continue
		}
		found = true
		if PodManaged(pod) {
			continue
		}
		if workloadOwners == nil {
			workloadOwners = e.workloadOwners(ctx, ns)
		}
		if !e.ownerManaged(ctx, ns, pod, owners, workloadOwners) {
			return false, true
		}
	}
	return found, found
}

// PodManaged reports whether a pod carries Kueue's managed label or its
// admission gate.
func PodManaged(pod *corev1.Pod) bool {
	if pod.Labels[ManagedLabel] == "true" {
		return true
	}
	return PodGated(pod)
}

// PodGated reports whether a pod is held behind Kueue's admission gate.
func PodGated(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool {
		return g.Name == AdmissionGate
	})
}

// ownerManaged walks pod's controller chain up to ownerDepth objects for one
// carrying the queue-name label, and reports whether a Workload is owned by it.
// An owner that cannot be read ends the walk: not managed.
func (e *Eligibility) ownerManaged(ctx context.Context, ns string, pod *corev1.Pod,
	cache map[types.UID]*unstructured.Unstructured, workloadOwners map[types.UID]bool) bool {
	ref := metav1.GetControllerOf(pod)
	for depth := 0; ref != nil && depth < ownerDepth; depth++ {
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
			return false
		}
		if owner.GetLabels()[QueueNameLabel] != "" {
			return workloadOwners[ref.UID]
		}
		ref = metav1.GetControllerOf(owner)
	}
	return false
}

// workloadOwners is the UIDs of every object a Workload in ns names as an
// owner. Empty when the Workloads cannot be listed, which leaves an unlabelled
// pod unmanaged.
func (e *Eligibility) workloadOwners(ctx context.Context, ns string) map[types.UID]bool {
	out := map[types.UID]bool{}
	mapping, err := e.client.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: KindWorkload})
	if err != nil {
		return out
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(mapping.GroupVersionKind.GroupVersion().WithKind(KindWorkload + "List"))
	if err := e.client.List(ctx, list, client.InNamespace(ns)); err != nil {
		return out
	}
	for _, w := range list.Items {
		for _, ref := range w.GetOwnerReferences() {
			out[ref.UID] = true
		}
	}
	return out
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
	// Owners are the names of the objects the Workload is owned by: a pod for
	// the pod integration, a LeaderWorkerSet or Job otherwise.
	Owners []string
	At     time.Time
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
			pw := PreemptedWorkload{UID: w.GetUID(), At: at}
			for _, ref := range w.GetOwnerReferences() {
				pw.Owners = append(pw.Owners, ref.Name)
			}
			out = append(out, pw)
		}
	}
	return out, nil
}
