package allocation

import (
	"cmp"
	"container/heap"
	"math"
	"slices"
)

// ShareRole is one role's input to the utilization-share allocation: a role of
// a P/D model, or the single synthetic role of an aggregated one. All GPU
// quantities are whole GPUs except Need, which is continuous.
//
// See docs/proposals/utilization-share-optimizer.md, sections 4 and 5.
type ShareRole struct {
	// Key identifies the role (namespace, model, role) and breaks ties, so the
	// allocation is deterministic for identical input.
	Key string
	// Weight is the model's weight, already clamped into the class range.
	Weight float64
	// Need is N_r: the GPUs that put the role exactly at its scale-up threshold.
	Need float64
	// Floor is F_r, the GPUs its minReplicaCount holds.
	Floor int
	// Ceiling is C_r, the GPUs its maxReplicaCount allows. Zero is unbounded.
	Ceiling int
	// ReplicaGPUs is g_r, one replica of the role.
	ReplicaGPUs int
}

func (r ShareRole) ceiling() float64 {
	if r.Ceiling <= 0 {
		return math.Inf(1)
	}
	return float64(r.Ceiling)
}

// Claim is the role's need raised to its floor and capped at its ceiling:
// N̄_r = min(max(N_r, F_r), C_r).
func (r ShareRole) Claim() float64 {
	return math.Min(math.Max(r.Need, float64(r.Floor)), r.ceiling())
}

// ShareScore is z, the one score that orders both regimes (proposal §5.4):
// headroom divided by weight when the role holds at least its need, shortfall
// multiplied by weight when it holds less. A heavier role's headroom counts
// for less and its shortfall for more. A role with no need has no score.
func ShareScore(held, need, weight float64) float64 {
	if need <= 0 {
		return math.Inf(1)
	}
	x := held/need - 1
	if x >= 0 {
		return x / weight
	}
	return x * weight
}

// ShareSpare is S = B − Σ N̄_r over the roles: positive when the budget covers
// every claim, negative when it is short (proposal §5.1).
func ShareSpare(roles []ShareRole, budget float64) float64 {
	sum := 0.0
	for _, r := range roles {
		sum += r.Claim()
	}
	return budget - sum
}

// WholeReplicaCoverage reports whether whole replicas allow every role its
// claim: Σ g_r · ⌈N̄_r / g_r⌉ ≤ budget. Only then is "every role gets at least
// its claim" guaranteed; otherwise a role may end up one replica below it
// (proposal §5.1).
func WholeReplicaCoverage(roles []ShareRole, budget int) bool {
	sum := 0
	for _, r := range roles {
		g := max(r.ReplicaGPUs, 1)
		sum += g * int(math.Ceil(r.Claim()/float64(g)-1e-9))
	}
	return sum <= budget
}

// ContinuousShareTargets returns Ĝ_r, the continuous allocation the tolerance
// band is judged against (proposal §5.1, §6.1): every role at its claim, the
// spare shared as headroom proportional to weight, or a shortfall shared by
// inverse weight, water-filled over floors and ceilings. Roles with no need
// hold their floor and take no share.
func ContinuousShareTargets(roles []ShareRole, budget float64) map[string]float64 {
	out := make(map[string]float64, len(roles))
	active := make([]ShareRole, 0, len(roles))
	pool := budget
	for _, r := range roles {
		if r.Need <= 0 {
			out[r.Key] = float64(r.Floor)
			pool -= float64(r.Floor)
			continue
		}
		active = append(active, r)
	}
	// Each pass solves for the common factor over the unpinned roles, then pins
	// every role the solution would take past its floor or ceiling. A pinned
	// role's GPUs leave the pool; the rest are solved again. Each pass pins at
	// least one role or finishes, so it ends within len(roles) passes.
	for len(active) > 0 {
		sumN, sumWN, sumNoverW := 0.0, 0.0, 0.0
		for _, r := range active {
			sumN += r.Need
			sumWN += r.Weight * r.Need
			sumNoverW += r.Need / r.Weight
		}
		spare := pool - sumN
		target := func(r ShareRole) float64 {
			if spare >= 0 {
				return r.Need * (1 + spare/sumWN*r.Weight)
			}
			return r.Need * (1 + spare/sumNoverW/r.Weight)
		}
		var keep []ShareRole
		pinned := false
		for _, r := range active {
			g := target(r)
			switch {
			case g < float64(r.Floor):
				out[r.Key] = float64(r.Floor)
				pool -= float64(r.Floor)
				pinned = true
			case g > r.ceiling():
				out[r.Key] = r.ceiling()
				pool -= r.ceiling()
				pinned = true
			default:
				keep = append(keep, r)
			}
		}
		if !pinned {
			for _, r := range keep {
				out[r.Key] = math.Max(0, target(r))
			}
			break
		}
		active = keep
	}
	return out
}

// IntegerShareTargets returns G*_r, the whole-replica allocation (proposal
// §5.4). Every role starts at its floor; then, while some role can take one
// more replica, the next replica goes to the candidate with the lowest score z
// -- ties to the higher weight, then to the key. A candidate has demand, a next
// replica that fits the remaining budget, and room below its ceiling. Any
// leftover smaller than every candidate's replica stays unallocated.
func IntegerShareTargets(roles []ShareRole, budget int) map[string]int {
	out := make(map[string]int, len(roles))
	left := budget
	h := &shareHeap{}
	for _, r := range roles {
		out[r.Key] = r.Floor
		left -= r.Floor
	}
	for _, r := range roles {
		if r.Need > 0 {
			heap.Push(h, shareEntry{role: r, held: out[r.Key]})
		}
	}
	// A popped role whose replica does not fit is dropped for good: the
	// remaining budget only shrinks, so it never fits later.
	for h.Len() > 0 {
		e := heap.Pop(h).(shareEntry)
		g := max(e.role.ReplicaGPUs, 1)
		if g > left || float64(e.held+g) > e.role.ceiling() {
			continue
		}
		e.held += g
		left -= g
		out[e.role.Key] = e.held
		heap.Push(h, e)
	}
	return out
}

type shareEntry struct {
	role ShareRole
	held int
}

func (e shareEntry) z() float64 { return ShareScore(float64(e.held), e.role.Need, e.role.Weight) }

// shareHeap is a min-heap on (z, -weight, key).
type shareHeap []shareEntry

func (h shareHeap) Len() int { return len(h) }
func (h shareHeap) Less(i, j int) bool {
	return cmp.Or(
		cmp.Compare(h[i].z(), h[j].z()),
		cmp.Compare(h[j].role.Weight, h[i].role.Weight),
		cmp.Compare(h[i].role.Key, h[j].role.Key),
	) < 0
}
func (h shareHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *shareHeap) Push(x any)   { *h = append(*h, x.(shareEntry)) }
func (h *shareHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

// ShareIdleFloor is the utilization distance within which a role not past its
// threshold counts as in band whatever its GPU distance (proposal §6.1, §8.4):
// it stops idle models from churning on large relative misses of no
// consequence.
const ShareIdleFloor = 0.05

// ShareInBand reports whether a role holding committed GPUs is inside the band
// around its continuous target (proposal §6.1 step 3). The band is judged in
// GPUs, never narrower than half a replica; a role not past its scale-up
// threshold is also in band when its utilization is within ShareIdleFloor of
// the target's. threshold is the role's scale-up threshold k_r, so its
// utilization is need·k / GPUs.
func ShareInBand(committed, target float64, r ShareRole, tolerance, threshold float64) bool {
	if math.Abs(committed-target) <= shareTol(target, r, tolerance) {
		return true
	}
	if committed <= 0 || target <= 0 {
		return false
	}
	u := r.Need * threshold / committed
	ut := r.Need * threshold / target
	return u <= threshold && math.Abs(u-ut) <= ShareIdleFloor
}

// ShareRoleVerdict is the per-role result of one evaluation.
type ShareRoleVerdict struct {
	Key string
	// Committed is Ḡ_r, the GPUs the role holds or is committed to.
	Committed int
	// Continuous is Ĝ_r and Integer is G*_r.
	Continuous float64
	Integer    int
	// Headroom is x_r = Committed / Need − 1: the spike the role absorbs before
	// it must scale. NaN when the role has no need.
	Headroom float64
	// InBand and Actionable are §6.1 steps 3 and 4: a role is actionable when it
	// is out of band AND off its integer target, so a move could fix it.
	InBand     bool
	Actionable bool
}

// ShareEvaluation is one group's evaluation.
type ShareEvaluation struct {
	// Spare is S = B_net − Σ claims; negative when the group is short.
	Spare float64
	// WholeReplicaCoverage reports whether whole replicas allow every claim.
	WholeReplicaCoverage bool
	// Roles are sorted by key.
	Roles []ShareRoleVerdict
	// ReplicasToMove is how many replicas the integer target would move: the
	// sum of every role's shortfall below G*, in replicas.
	ReplicasToMove int
}

// EvaluateShare evaluates one group: targets, the band and the actionable
// roles, for the committed allocation given (proposal §6.1 steps 2-4). It
// plans nothing. thresholds maps each role key to its scale-up threshold.
func EvaluateShare(roles []ShareRole, committed map[string]int, thresholds map[string]float64,
	budget int, tolerance float64) ShareEvaluation {
	cont := ContinuousShareTargets(roles, float64(budget))
	integ := IntegerShareTargets(roles, budget)
	ev := ShareEvaluation{
		Spare:                ShareSpare(roles, float64(budget)),
		WholeReplicaCoverage: WholeReplicaCoverage(roles, budget),
	}
	for _, r := range roles {
		c := committed[r.Key]
		v := ShareRoleVerdict{
			Key:        r.Key,
			Committed:  c,
			Continuous: cont[r.Key],
			Integer:    integ[r.Key],
			Headroom:   math.NaN(),
		}
		if r.Need > 0 {
			v.Headroom = float64(c)/r.Need - 1
		}
		v.InBand = ShareInBand(float64(c), v.Continuous, r, tolerance, thresholds[r.Key])
		v.Actionable = !v.InBand && c != v.Integer
		if short := v.Integer - c; short > 0 {
			ev.ReplicasToMove += short / max(r.ReplicaGPUs, 1)
		}
		ev.Roles = append(ev.Roles, v)
	}
	slices.SortFunc(ev.Roles, func(a, b ShareRoleVerdict) int { return cmp.Compare(a.Key, b.Key) })
	return ev
}
