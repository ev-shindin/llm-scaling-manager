package decision

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// A scale-from-zero wake may claim GPUs a utilization-share transfer is still
// releasing for another receiver, when the wake is worse off by the measure that
// decided the transfer (docs/proposals/utilization-share-optimizer.md, section
// 6.3). The claim redirects the transfer: the donor's release proceeds, and at
// release nobody is raised -- the wake's own pod is already waiting for the
// hole -- while the original receiver is planned again. Claims go through this
// one store, behind one mutex, so the steady-state engine and the 10 Hz
// scale-from-zero loop cannot both act on one transfer.

// ShareClaimable is a transfer a wake may claim: still releasing, for a
// receiver whose score at its current holdings is ReceiverZ.
type ShareClaimable struct {
	ID        string
	ReceiverZ float64
	// DonorPodGPUs is the GPUs of each pod the donor releases, leader first;
	// nil when unknown, and DonorGPUs, the replica's total, is compared.
	DonorPodGPUs []int
	DonorGPUs    int
}

// ShareClaim is a claim the steady-state engine has yet to apply.
type ShareClaim struct {
	Scope, Accelerator, ID string
	// Wake names the woken variant (namespace/variant), for the log.
	Wake string
	// Model is the woken model (namespace/model), for the log. The claims
	// one wake made together -- a decode and its prefill -- share it.
	Model string
}

// Claim outcomes, the outcome label of wva_utilization_share_claims_total.
const (
	ShareClaimRedirected    = "redirected"
	ShareClaimRefusedScore  = "refused-score"
	ShareClaimRefusedFit    = "refused-fit"
	ShareClaimNoneReleasing = "none-releasing"
)

// ShareClaimMaxAge is how old the claimable list may be. Older, it belongs to an
// optimizer that has stopped publishing, and nothing is claimable.
const ShareClaimMaxAge = 2 * time.Minute

// ShareClaimModelHold is how long a model that claimed a transfer may not claim
// another. A woken model stays inactive until KEDA has acted on its wake, and
// the wake loop runs at 10 Hz: without a hold, one parked model would claim
// every releasing transfer in reach before its first replica existed.
const ShareClaimModelHold = 2 * time.Minute

// ShareClaimRefusedHeld is the outcome for a model still within its hold.
const ShareClaimRefusedHeld = "refused-held"

// ShareClaimStore holds the claimable transfers of every active group, keyed by
// ShareGroupKey, and the claims made against them.
type ShareClaimStore struct {
	mu        sync.Mutex
	claimable map[string][]ShareClaimable
	claims    []ShareClaim
	at        time.Time
	// lastClaim is when each model last claimed (namespace/model).
	lastClaim map[string]time.Time
}

// ShareGroupKey is a group's key: its scope ("" for the cluster group, else a
// namespace) and accelerator.
func ShareGroupKey(scope, accelerator string) string { return scope + "|" + accelerator }

// Publish replaces the claimable transfers. Claims already made and not yet
// taken are kept: the engine applies them on its next pass.
func (s *ShareClaimStore) Publish(claimable map[string][]ShareClaimable, now time.Time) {
	c := make(map[string][]ShareClaimable, len(claimable))
	for k, v := range claimable {
		c[k] = slices.Clone(v)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A transfer already claimed and not yet taken stays out: the engine still
	// lists it until it applies the claim, and a second wake must not claim
	// the same hole.
	for k, entries := range c {
		c[k] = slices.DeleteFunc(entries, func(e ShareClaimable) bool {
			return slices.ContainsFunc(s.claims, func(cl ShareClaim) bool { return cl.ID == e.ID })
		})
	}
	s.claimable, s.at = c, now
}

// ShareWake is one replica a wake needs funded: its pods' GPUs (nil when
// unknown, then GPUs is compared), its total, and its variant
// (namespace/variant).
type ShareWake struct {
	Pods    []int
	GPUs    int
	Variant string
}

// ClaimSet tries to redirect one releasing transfer to each replica of a
// wake -- a decode with the prefill it must wake alongside -- all or none:
// half a P/D pair woken serves nothing. scope is the wake's own group -- its
// namespace when the namespace has its own quota, "" for the cluster group --
// and z its score there. Only that group is tried: a model is planned in one
// group, and the GPUs of another are not its to take. A transfer qualifies for
// a replica when the wake scores lower than its receiver and its donor pods
// cover the replica's. Replicas are matched largest first, each preferring the
// transfer of the best-off receiver (highest score), backtracking when a later
// replica would be left without one. model (namespace/model)
// is held for ShareClaimModelHold after a claim. It returns the claims and the
// outcome; only ShareClaimRedirected carries claims.
func (s *ShareClaimStore) ClaimSet(scope, accelerator string, wakes []ShareWake,
	z float64, model string, now time.Time) ([]ShareClaim, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(wakes) == 0 || s.claimable == nil || now.Sub(s.at) > ShareClaimMaxAge {
		return nil, ShareClaimNoneReleasing
	}
	if at, ok := s.lastClaim[model]; ok && now.Sub(at) < ShareClaimModelHold {
		return nil, ShareClaimRefusedHeld
	}
	key := ShareGroupKey(scope, accelerator)
	entries := s.claimable[key]
	if len(entries) == 0 {
		return nil, ShareClaimNoneReleasing
	}
	// Ties go to the receiver, which was promised first.
	if !slices.ContainsFunc(entries, func(e ShareClaimable) bool { return z < e.ReceiverZ }) {
		return nil, ShareClaimRefusedScore
	}
	order := slices.Clone(wakes)
	slices.SortStableFunc(order, func(a, b ShareWake) int { return cmp.Compare(b.GPUs, a.GPUs) })
	pref := make([]int, len(entries))
	for i := range pref {
		pref[i] = i
	}
	slices.SortStableFunc(pref, func(a, b int) int {
		return cmp.Or(cmp.Compare(entries[b].ReceiverZ, entries[a].ReceiverZ), cmp.Compare(entries[a].ID, entries[b].ID))
	})
	used := make([]bool, len(entries))
	pick := make([]int, len(order))
	var assign func(i int) bool
	assign = func(i int) bool {
		if i == len(order) {
			return true
		}
		w := order[i]
		for _, j := range pref {
			e := entries[j]
			if used[j] || !(z < e.ReceiverZ) || !PodsCover(e.DonorPodGPUs, e.DonorGPUs, w.Pods, w.GPUs) {
				continue
			}
			used[j], pick[i] = true, j
			if assign(i + 1) {
				return true
			}
			used[j] = false
		}
		return false
	}
	if !assign(0) {
		return nil, ShareClaimRefusedFit
	}
	claims := make([]ShareClaim, 0, len(order))
	for i, w := range order {
		claims = append(claims, ShareClaim{Scope: scope, Accelerator: accelerator, ID: entries[pick[i]].ID,
			Wake: w.Variant, Model: model})
	}
	s.claimable[key] = slices.DeleteFunc(slices.Clone(entries), func(e ShareClaimable) bool {
		return slices.ContainsFunc(claims, func(c ShareClaim) bool { return c.ID == e.ID })
	})
	s.claims = append(s.claims, claims...)
	if s.lastClaim == nil {
		s.lastClaim = map[string]time.Time{}
	}
	s.lastClaim[model] = now
	return claims, ShareClaimRedirected
}

// Take returns and forgets the claims made against one group.
func (s *ShareClaimStore) Take(scope, accelerator string) []ShareClaim {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ShareClaim
	s.claims = slices.DeleteFunc(s.claims, func(c ShareClaim) bool {
		if c.Scope == scope && c.Accelerator == accelerator {
			out = append(out, c)
			return true
		}
		return false
	})
	return out
}

// PodsCover reports whether a donor replica's pods can host a receiver
// replica's (section 6.5): every receiver pod needs a donor pod of its own at
// least its size, since nothing shows that several smaller donor pods share a
// node. With either shape unknown, the replicas' totals are compared.
func PodsCover(donorPods []int, donorGPUs int, receiverPods []int, receiverGPUs int) bool {
	if len(donorPods) == 0 || len(receiverPods) == 0 {
		return max(donorGPUs, 1) >= max(receiverGPUs, 1)
	}
	if len(receiverPods) > len(donorPods) {
		return false
	}
	d := slices.Sorted(slices.Values(donorPods))
	r := slices.Sorted(slices.Values(receiverPods))
	slices.Reverse(d)
	slices.Reverse(r)
	for i := range r {
		if d[i] < r[i] {
			return false
		}
	}
	return true
}

// DefaultShareClaims is the process-wide store.
var DefaultShareClaims = &ShareClaimStore{}
