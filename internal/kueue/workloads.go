package kueue

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadOwnerUIDField is the field index the Workload informer keeps on each
// Workload's owner UIDs, so the managed check looks a pod's owners up instead
// of reading every Workload of the namespace.
const WorkloadOwnerUIDField = "kueue.metadata.ownerReferences.uid"

// WorkloadSource lists Kueue Workloads, for the managed check, the preemption
// check and the lender-waiting signal.
//
// A failure is returned as the API reports it: a Kueue that does not serve
// Workloads fails with a NoMatch error and a denied read with Forbidden, which
// the callers read as "known: none" (fail closed, never as "absent").
type WorkloadSource interface {
	// Workloads lists the Workloads in ns, "" for every namespace the source
	// can see; with owner set, only those owned by owner.
	Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error)
	// Indexed reports whether a lookup by owner is served from an index, so
	// one per owner costs no API call. When false, a caller that needs many
	// owners of one namespace lists the namespace once instead.
	Indexed() bool
}

// defaultWorkloads is the source every caller that is handed none uses: the
// Workload informer, once the controller has wired one (UseWorkloadSource).
var defaultWorkloads atomic.Pointer[WorkloadSource]

// UseWorkloadSource makes s the source of every Workload read that is not
// handed one explicitly: the managed check of every quota entry and the
// preemption check. The controller sets the informer-backed source once at
// startup; nil restores reading from the API server.
func UseWorkloadSource(s WorkloadSource) {
	if s == nil {
		defaultWorkloads.Store(nil)
		return
	}
	defaultWorkloads.Store(&s)
}

// WorkloadsVia returns the Workload source to read through: the one
// UseWorkloadSource set, or a direct API read through c.
func WorkloadsVia(c client.Client) WorkloadSource {
	if s := defaultWorkloads.Load(); s != nil {
		return *s
	}
	return APIWorkloads{Client: c}
}

// APIWorkloads reads Workloads straight from the API server: unstructured,
// which the project's clients never cache, so each call is one LIST.
type APIWorkloads struct {
	Client client.Client
}

// Workloads implements WorkloadSource.
func (a APIWorkloads) Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error) {
	list, err := newWorkloadList(a.Client)
	if err != nil {
		return nil, err
	}
	var opts []client.ListOption
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if err := a.Client.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return ownedBy(list.Items, owner), nil
}

// Indexed implements WorkloadSource: every call is a LIST.
func (APIWorkloads) Indexed() bool { return false }

// newWorkloadList is an empty Workload list at the version the cluster serves.
func newWorkloadList(c client.Client) (*unstructured.UnstructuredList, error) {
	gvk, err := workloadGVK(c)
	if err != nil {
		return nil, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(KindWorkload + "List"))
	return list, nil
}

// workloadGVK resolves the Workload kind at the version the cluster serves.
func workloadGVK(c client.Client) (schema.GroupVersionKind, error) {
	mapping, err := c.RESTMapper().RESTMapping(schema.GroupKind{Group: Group, Kind: KindWorkload})
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	return mapping.GroupVersionKind, nil
}

// ownedBy keeps the Workloads owned by owner; all of them when owner is empty.
func ownedBy(items []unstructured.Unstructured, owner types.UID) []unstructured.Unstructured {
	if owner == "" {
		return items
	}
	out := items[:0:0]
	for i := range items {
		for _, ref := range items[i].GetOwnerReferences() {
			if ref.UID == owner {
				out = append(out, items[i])
				break
			}
		}
	}
	return out
}

// workloadOwnerUIDs is the WorkloadOwnerUIDField index function.
func workloadOwnerUIDs(o client.Object) []string {
	refs := o.GetOwnerReferences()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref.UID))
	}
	return out
}

// InformerWorkloads reads Workloads from an informer of the manager's cache,
// started on first use, with an index on owner UID. The cache's namespaces are
// the informer's: a cluster-scoped install caches every namespace's Workloads,
// a namespace-scoped one only its own. Until the informer has synced -- and
// for good, if it never does (no watch permission, say) -- every read goes to
// the API server instead, so a Forbidden or a missing Kueue surfaces as the
// error it is rather than as an empty cache.
type InformerWorkloads struct {
	cache cache.Cache
	api   client.Client
	now   func() time.Time

	mu        sync.Mutex
	gvk       schema.GroupVersionKind
	informer  cache.Informer
	startedAt time.Time
	indexed   bool
	warned    bool
}

// NewInformerWorkloads returns a source over the manager's cache c; api
// resolves the Workload kind and serves the reads until the informer syncs.
func NewInformerWorkloads(c cache.Cache, api client.Client) *InformerWorkloads {
	return &InformerWorkloads{cache: c, api: api, now: time.Now}
}

// notSyncedWarnAfter is how long the informer may stay unsynced before that
// is logged: past it, every read is a LIST again.
const notSyncedWarnAfter = 2 * time.Minute

// synced starts the informer for the served Workload version if needed and
// reports whether it has synced, and whether the owner index is in place.
func (w *InformerWorkloads) synced(ctx context.Context) (schema.GroupVersionKind, bool, bool, error) {
	gvk, err := workloadGVK(w.api)
	if err != nil {
		return gvk, false, false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if w.informer == nil || w.gvk != gvk {
		inf, err := w.cache.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return gvk, false, false, nil // the API serves meanwhile
		}
		w.informer, w.gvk, w.startedAt, w.indexed, w.warned = inf, gvk, w.now(), false, false
	}
	if !w.informer.HasSynced() {
		if !w.warned && w.now().Sub(w.startedAt) > notSyncedWarnAfter {
			w.warned = true
			kueueLogger(ctx).Info("The Kueue Workload informer has not synced; Workloads are read from the API server on every check until it does. Does the controller have watch on workloads.kueue.x-k8s.io?",
				"waited", notSyncedWarnAfter.String())
		}
		return gvk, false, false, nil
	}
	if !w.indexed {
		// Synced, so IndexField returns at once; an index added after the
		// informer started is built from its store.
		if err := w.cache.IndexField(ctx, obj, WorkloadOwnerUIDField, workloadOwnerUIDs); err == nil {
			w.indexed = true
		}
	}
	return gvk, true, w.indexed, nil
}

// Workloads implements WorkloadSource.
func (w *InformerWorkloads) Workloads(ctx context.Context, ns string, owner types.UID) ([]unstructured.Unstructured, error) {
	gvk, synced, indexed, err := w.synced(ctx)
	if err != nil {
		return nil, err
	}
	if !synced {
		return APIWorkloads{Client: w.api}.Workloads(ctx, ns, owner)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(KindWorkload + "List"))
	var opts []client.ListOption
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if owner != "" && indexed {
		opts = append(opts, client.MatchingFields{WorkloadOwnerUIDField: string(owner)})
	}
	if err := w.cache.List(ctx, list, opts...); err != nil {
		return nil, err
	}
	return ownedBy(list.Items, owner), nil
}

// Indexed implements WorkloadSource: true once the informer has synced and
// carries the owner index.
func (w *InformerWorkloads) Indexed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.informer != nil && w.indexed && w.informer.HasSynced()
}
