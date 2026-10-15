package allocation

import (
	"slices"
	"sync"
)

// BorrowInFlight is borrowed growth the greedy optimizer ordered that Kueue
// has not reserved yet, kept by the steady-state engine across cycles
// (docs/proposals/kueue-borrowing-and-limiter-composition.md, B4). The Kueue
// reader cannot see it: until the pods exist and are admitted, the cohort's
// free pool still counts those GPUs, and a second cycle -- or another
// namespace -- would borrow them again.
//
// An entry stays until the variant's pods exist (its replica count reaches the
// target it was ordered at; from then on a pod still behind Kueue's admission
// gate is charged by the gated-pod deduction, and an admitted one by Kueue's
// reservation) or its desired count goes back below that target. The release
// steps of the proposal (B5, B6) extend this record; it is keyed by namespace,
// variant and chunk so a release can find what a variant borrowed and where.
type BorrowInFlight struct {
	mu      sync.Mutex
	entries map[inFlightKey]BorrowDraw
}

type inFlightKey struct {
	namespace, variant, cohort, flavor, resource string
}

func keyOfDraw(d BorrowDraw) inFlightKey {
	return inFlightKey{d.Namespace, d.Variant, d.Cohort, d.Flavor, d.Resource}
}

// VariantObservation is one variant's replica counts as a cycle sees them.
type VariantObservation struct {
	// Current is the replicas that exist (pods created, gated ones included).
	Current int
	// Desired is the target the variant is held at now; 0 when unknown.
	Desired int
}

// NewBorrowInFlight returns an empty record.
func NewBorrowInFlight() *BorrowInFlight {
	return &BorrowInFlight{entries: map[inFlightKey]BorrowDraw{}}
}

// Record adds a pass's draws. A variant that borrows again from the same chunk
// before its earlier draw resolved holds both, at the newer target.
func (b *BorrowInFlight) Record(draws []BorrowDraw) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range draws {
		k := keyOfDraw(d)
		if prev, ok := b.entries[k]; ok {
			d.GPUs += prev.GPUs
			d.Replicas += prev.Replicas
			d.Target = max(d.Target, prev.Target)
		}
		b.entries[k] = d
	}
}

// Observe drops the entries that resolved: the variant's pods exist up to the
// target the draw was ordered at, its desired count went back below that
// target, or it is no longer observed at all. observed is keyed by
// "namespace/variant".
func (b *BorrowInFlight) Observe(observed map[string]VariantObservation) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, d := range b.entries {
		o, ok := observed[k.namespace+"/"+k.variant]
		if !ok || o.Current >= d.Target || (o.Desired > 0 && o.Desired < d.Target) {
			delete(b.entries, k)
			continue
		}
		// Replicas that now exist are no longer in flight here.
		if missing := d.Target - o.Current; missing < d.Replicas {
			per := ceilDiv(d.GPUs, d.Replicas)
			d.GPUs = min(d.GPUs, missing*per)
			d.Replicas = missing
			b.entries[k] = d
		}
	}
}

// Entries returns the draws still in flight, in a stable order.
func (b *BorrowInFlight) Entries() []BorrowDraw {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BorrowDraw, 0, len(b.entries))
	for _, d := range b.entries {
		out = append(out, d)
	}
	slices.SortFunc(out, func(x, y BorrowDraw) int {
		kx, ky := keyOfDraw(x), keyOfDraw(y)
		return compareInFlightKeys(kx, ky)
	})
	return out
}

func compareInFlightKeys(a, b inFlightKey) int {
	for _, p := range [][2]string{{a.namespace, b.namespace}, {a.variant, b.variant}, {a.cohort, b.cohort}, {a.flavor, b.flavor}, {a.resource, b.resource}} {
		switch {
		case p[0] < p[1]:
			return -1
		case p[0] > p[1]:
			return 1
		}
	}
	return 0
}

// DeductUnreserved returns copies of constraints whose shared pools no longer
// count what the scaling manager asked for and Kueue has not reserved: the
// draws in flight, from the chunk each was drawn from, and gated -- GPUs of
// managed pods behind Kueue's admission gate, per namespace and accelerator
// ("" when the pod names none) -- from the chunks of the namespace's cohorts
// that serve the accelerator, first chunk first. Every figure clamps at 0.
// Constraints without borrowing are returned as they are.
func DeductUnreserved(constraints []*ResourceConstraints, inFlight []BorrowDraw, gated map[string]map[string]int) []*ResourceConstraints {
	if !HasBorrowing(constraints) || (len(inFlight) == 0 && len(gated) == 0) {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || c.NamespaceBorrow == nil || len(c.SharedPools) == 0 {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.SharedPools = slices.Clone(c.SharedPools)
		for _, d := range inFlight {
			for i := range cc.SharedPools {
				p := &cc.SharedPools[i]
				if p.Cohort == d.Cohort && p.Flavor == d.Flavor && p.Resource == d.Resource {
					p.Free = max(0, p.Free-d.GPUs)
				}
			}
		}
		for ns, byAcc := range gated {
			for acc, n := range byAcc {
				for i := range cc.SharedPools {
					p := &cc.SharedPools[i]
					if n <= 0 {
						break
					}
					if !slices.Contains(c.NamespaceCohorts[ns], p.Cohort) || (acc != "" && !p.serves(acc)) {
						continue
					}
					take := min(p.Free, n)
					p.Free -= take
					n -= take
				}
			}
		}
		out = append(out, &cc)
	}
	return out
}

// BlockCohorts returns copies of constraints whose shared pools in the given
// cohorts read 0 free: nothing new is borrowed there this cycle (a lender is
// waiting, or waited within the cooldown). Without such a cohort, the input.
func BlockCohorts(constraints []*ResourceConstraints, cohorts map[string]bool) []*ResourceConstraints {
	if len(cohorts) == 0 || !HasBorrowing(constraints) {
		return constraints
	}
	out := make([]*ResourceConstraints, 0, len(constraints))
	for _, c := range constraints {
		if c == nil || len(c.SharedPools) == 0 {
			out = append(out, c)
			continue
		}
		cc := *c
		cc.SharedPools = slices.Clone(c.SharedPools)
		for i := range cc.SharedPools {
			if cohorts[cc.SharedPools[i].Cohort] {
				cc.SharedPools[i].Free = 0
			}
		}
		out = append(out, &cc)
	}
	return out
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return a
	}
	return (a + b - 1) / b
}
