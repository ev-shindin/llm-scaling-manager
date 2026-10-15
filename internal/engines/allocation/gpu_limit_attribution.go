package allocation

import (
	"math"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// gpuLimitTracker records the variants of ONE model whose scale-up the optimizer
// had to cut short because a GPU budget ran out. It is the bridge between the
// allocation loops — which are the only place that knows a target was clamped —
// and the decisions built afterwards, which carry the operator-facing signal.
//
// Only genuine scarcity is recorded. A variant held back by its own MaxReplicas
// ceiling is honouring user intent, not competing for GPUs, and marking it would
// raise a spurious ResourceConstrained warning; the loops therefore distinguish
// the two bounds before marking.
//
// Keyed by variant name, which is unique within a model. Trackers are per-model
// so names never collide across models or namespaces.
type gpuLimitTracker map[string]bool

// mark records that variantName wanted more replicas than the GPU budget allowed.
// Repeated marks are idempotent — a variant clamped in several fair-share
// iterations is still one constrained decision.
func (t gpuLimitTracker) mark(variantName string) {
	if t != nil {
		t[variantName] = true
	}
}

// applyGPULimitAttribution stamps the resource-constraint signal onto a model's
// decisions: WasLimited for every variant the tracker recorded, and LimitedBy
// naming the provider whose pool bound it. It also emits the
// decisions_limited_total metric, once per constrained decision.
//
// Decisions of every action carry the signal, not just scale-ups: a variant that
// wanted to grow and was denied every GPU ends up at ActionNoChange, and that is
// precisely the case an operator needs to see.
func applyGPULimitAttribution(
	decisions []domain.VariantDecision,
	limited gpuLimitTracker,
	constraints []*ResourceConstraints,
) {
	// Published on EVERY pass, including the one where nothing was limited.
	// An empty snapshot is a real answer -- it says this pass denied nobody --
	// and publishing only the non-empty ones would leave the last contended
	// reading standing forever, holding warm pools down long after the burst.
	contended := map[string]map[string]bool{}
	// Headroom rides along with contention, from the one place that sees every
	// constraint provider at once. They answer different questions and the warm
	// pool needs both: contention says a model replica is being denied RIGHT NOW,
	// which is a reason to yield ground already held; headroom says the allowance
	// is spent, which is a reason not to ask for more in the first place.
	defer func() {
		now := time.Now()
		decision.PublishGPUContention(contended, now)
		publishHeadroom(constraints, now)
	}()

	if len(limited) == 0 {
		return
	}
	emitter := metrics.NewMetricsEmitter()
	for i := range decisions {
		d := &decisions[i]
		if !limited[d.VariantName] {
			continue
		}
		d.WasLimited = true
		d.LimitedBy = bindingProvider(constraints, d.Namespace, d.AcceleratorName)
		emitter.RecordDecisionsLimitedTotalMetric(d.VariantName, d.Namespace, d.LimitedBy)

		// A warm pool of this accelerator type, in this namespace, should stop
		// competing for GPUs a model replica is being denied. See
		// decision.GPUContention.
		if d.AcceleratorName != "" {
			if contended[d.Namespace] == nil {
				contended[d.Namespace] = map[string]bool{}
			}
			contended[d.Namespace][d.AcceleratorName] = true
		}
	}
}

// namespaceHeadroom is how many GPUs of each accelerator each namespace may
// still take, across every provider, tightest bound winning -- the closed
// cluster-scope bound (clusterHeadroom) included, as GPUBudgets bounds a
// namespace's allowance by the cluster pools: a namespace may not be granted
// more than the cluster has free.
//
// Only namespaces some provider actually CAPS appear. A namespace nobody caps
// is absent rather than present-with-a-large-number, because "unconstrained" and
// "a lot left" are different answers and a caller has to be able to tell them
// apart -- a warm pool must grow freely in the first case. It is still bounded
// by the cluster figure, which is published beside this (Headroom.Cluster).
//
// Mirrors effectiveAvailable and GPUBudgets: a negative Limit is the unlimited
// sentinel and imposes no namespace bound, an accelerator a namespace's
// allowlist does not name is a hard deny, which is zero rather than absent, and
// a type the cluster bounds do not list is zero too however the namespace lists
// it (clusterBudgetFor) -- a physical pool of {H100: 8} leaves a namespace
// allowance of {A100: 4}, or of unlimited A100s, nothing to place.
func namespaceHeadroom(constraints []*ResourceConstraints) map[string]map[string]int {
	out := namespaceAllowance(constraints)
	cluster := clusterHeadroom(constraints)
	for _, perType := range out {
		for accType, free := range perType {
			cv, bounded := clusterBudgetFor(cluster, accType)
			if !bounded || cv == math.MaxInt {
				continue
			}
			if free < 0 {
				// Unlimited in the namespace: the cluster figure is the bound.
				perType[accType] = cv
			} else {
				perType[accType] = min(free, cv)
			}
		}
	}
	return out
}

// clusterHeadroom is the free GPUs per accelerator under every cluster bound
// (the physical inventory, a cluster-scope quota), merged as the optimizer
// merges them, or nil when none is in force. A namespace-scope quota bounds a
// namespace through that namespace's own allowance instead.
func clusterHeadroom(constraints []*ResourceConstraints) map[string]int {
	return mergeConstraints(constraints)
}

// publishHeadroom publishes the warm pool's headroom from one cycle's
// constraints: each capped namespace's allowance and the closed cluster-scope
// bound.
func publishHeadroom(constraints []*ResourceConstraints, now time.Time) {
	// The warm pool spends capacity within nominal only, never borrowed
	// capacity (B7): a pool Pod on borrowed GPUs is the first thing a reclaim
	// takes.
	constraints = WithinNominal(constraints)
	decision.PublishHeadroomBounds(namespaceHeadroom(constraints), clusterHeadroom(constraints),
		poolsVersionOf(constraints), now)
}

// namespaceAllowance is the free GPUs per (namespace, accelerator) under the
// namespace pools alone: mergeNamespaceConstraints, the optimizer's own merge,
// so the warm pool cannot read a type the optimizer refuses. An unlimited type
// is decision.HeadroomUnlimited -- skipping it would leave the type absent from
// a present namespace, which a reader takes as DENIED.
func namespaceAllowance(constraints []*ResourceConstraints) map[string]map[string]int {
	out := mergeNamespaceConstraints(constraints)
	for _, perType := range out {
		for accType, free := range perType {
			if free < 0 {
				perType[accType] = decision.HeadroomUnlimited
			}
		}
	}
	if out == nil {
		out = map[string]map[string]int{}
	}
	return out
}

// bindingProvider names the constraint provider whose pool most tightly bounds
// (namespace, accType) — the one to credit when a GPU budget clamps a scale-up.
//
// Cluster and namespace pools are weighed together, mirroring how
// effectiveAvailable combines them: the smallest availability wins. A provider
// that carries a closed allowlist for this namespace is judged on that allowlist
// alone (its per-type Pools are the aggregate *derived* from it, so counting
// both would weigh one provider twice), and a type the namespace does not list
// is a hard deny — availability 0, the tightest bound there is.
//
// The cluster pools follow mergeConstraints: a cluster bound that does not list
// the type denies it, availability 0, and is credited for it -- otherwise a
// cluster quota of {H100: 8} beside a physical pool of {A100: 4} would let A100
// be blamed on the physical limiter, which is the one that allowed it. A
// namespace-scope provider that does not hold the namespace bounds nothing
// here. Types are matched over the family/product tree (allowlistBound), as
// everywhere a workload's accelerator meets a pool key.
//
// Unlimited sentinels (Limit < 0) impose no bound and are skipped. Returns ""
// when no provider finitely bounds the type, which leaves LimitedBy empty rather
// than crediting a limiter that did not constrain anything.
func bindingProvider(constraints []*ResourceConstraints, namespace, accType string) string {
	name := ""
	tightest := math.MaxInt
	consider := func(avail int, provider string) {
		if avail < tightest {
			tightest, name = avail, provider
		}
	}
	for _, c := range constraints {
		if c == nil {
			continue
		}
		if perType, ok := c.NamespacePools[namespace]; ok {
			avail, unlimited, listed := allowlistBound(perType, accType)
			switch {
			case !listed:
				consider(0, c.ProviderName)
			case !unlimited:
				consider(avail, c.ProviderName)
			}
			continue
		}
		if !isClusterBound(c) {
			continue
		}
		if avail, bounds := clusterBound(c.Pools, accType); bounds && avail != math.MaxInt {
			consider(avail, c.ProviderName)
		}
	}
	return name
}

// PublishNamespaceHeadroom records what each namespace may still take, from a
// caller that built constraints outside an optimization pass.
//
// Headroom is normally published as a side effect of attribution, which runs
// only when there is something to optimize. A namespace whose ONLY WVA
// consumption is a warm pool has no variants and therefore no optimization pass
// -- so it never got an answer, and "no answer" used to mean unbounded to the
// pool, which is the one caller that reads it. Measured: a namespace with a
// one-GPU quota and no models grew its pool to three Pods.
//
// Exported rather than exposing namespaceHeadroom itself, so the mapping from
// constraints to headroom stays in one place and every publisher agrees about
// what an absent namespace means.
func PublishNamespaceHeadroom(constraints []*ResourceConstraints, now time.Time) {
	publishHeadroom(constraints, now)
}

// PublishUnboundedHeadroom records that NO limiter bounds any namespace, as of
// the given warm-pool figure version.
//
// This is a real answer and it has to be published: a warm pool holds until it
// has a headroom snapshot at least as new as its own last charge, and on a
// cluster with no limiter nothing else would ever publish one -- the pool would
// wait forever for an allowance nobody is keeping. An empty snapshot says
// "nothing bounds you", which is what the pool needs to hear to grow.
func PublishUnboundedHeadroom(poolsVersion uint64, now time.Time) {
	decision.PublishHeadroom(nil, poolsVersion, now)
}

// poolsVersionOf is the warm-pool figure version every constraint was computed
// against: the smallest across them, so a snapshot never claims to have charged
// a figure some constraint in it has not seen. Zero when no constraint is
// stamped, which no pool that has ever published can match -- an unstamped
// snapshot never lets a pool grow, and a publisher that forgets to stamp shows
// up as a pool that stops growing rather than one that overshoots.
func poolsVersionOf(constraints []*ResourceConstraints) uint64 {
	var v uint64
	first := true
	for _, c := range constraints {
		if c == nil {
			continue
		}
		if first || c.PoolsVersion < v {
			v = c.PoolsVersion
			first = false
		}
	}
	return v
}
