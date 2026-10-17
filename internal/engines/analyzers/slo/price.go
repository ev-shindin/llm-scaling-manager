package slo

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
)

// The fill a TPOT target is priced at is clamped to the range the ITL line is
// fitted over -- the window accepts readings only inside it -- which is the
// same clamp the saturation analyzer's pricingK applies. The line says nothing
// useful outside the range it was fitted on, and above DefaultMaxObservableK
// the engine preempts rather than slowing down.
//
// An earlier design bounded the fill by the highest fill the fleet had been
// observed at, plus a step. That bound has a low fixed point: the fleet runs
// N / scaleUp replicas and the token rate is concave in k, so the realised
// fill sits below the priced one, each window's observed maximum pulls the
// bound down, and the fill converges near 0.05*s/(1-s) -- 0.28 at a scale-up
// threshold of 0.85. See docs/proposals/slo-tiers.md, "Bounds".
var (
	minFill = itl.DefaultMinObservableK
	maxFill = itl.DefaultMaxObservableK
)

// pricing is the outcome of inverting a line at a mean-TPOT target.
type pricing struct {
	// fill is the KV fill, as a fraction of physical KV, at which the line's
	// mean ITL equals the target, clamped to [minFill, maxFill].
	fill float64
	// target is that fill before the clamp, for the log: above maxFill the
	// target is looser than the clamp lets the variant run.
	target float64
	// feasible is false when the line's ITL at minFill already reaches the
	// target (equal counts): no fill in range meets it on this variant.
	feasible bool
	// ok is false when an input was missing or non-finite. Neither of the
	// other two fields means anything then.
	ok bool
}

// priceTPOT inverts ITL(k) = A*k + B at a mean TPOT target, in seconds:
// k = (target - B) / A.
//
// TPOT is a MEAN, and the line is fitted on the mean inter-token latency
// (vllm:inter_token_latency_seconds, sum over count), so no tail model stands
// between the two.
func priceTPOT(line itl.Model, targetSeconds float64) pricing {
	if !finite(line.A) || !finite(line.B) || !finite(targetSeconds) {
		return pricing{}
	}
	if !(line.A > 0) || !(targetSeconds > 0) {
		return pricing{}
	}
	k := (targetSeconds - line.B) / line.A
	if targetSeconds <= line.ITLAt(minFill) {
		return pricing{target: k, ok: true}
	}
	if !finite(k) {
		return pricing{}
	}
	return pricing{fill: min(max(k, minFill), maxFill), target: k, feasible: true, ok: true}
}

// replicaTokenRate is the generation tokens per second one replica sustains at
// fill k: the sequences resident there, capped at maxNumSeqs, each advancing
// one token every ITL(k) seconds. Zero when any input is unusable.
//
// maxNumSeqs is the REPLICA's cap: the engine's max_num_seqs times the
// data-parallel ranks the replica runs, since capacityTokens is the whole
// replica's KV (priceVariant scales it).
//
// itl.TokenRate does not apply the cap -- it prices a cache, not an engine --
// and short requests imply more resident sequences than the engine admits, so
// the cap is applied here before the division, as the saturation analyzer's
// deriveMu applies it.
func replicaTokenRate(line itl.Model, k, capacityTokens, kvPerRequest float64, maxNumSeqs int64) float64 {
	seqs := itl.Sequences(k, capacityTokens, kvPerRequest)
	if !(seqs > 0) {
		return 0
	}
	if maxNumSeqs > 0 {
		seqs = min(seqs, float64(maxNumSeqs))
	}
	itlSec := line.ITLAt(k)
	if !(itlSec > 0) {
		return 0
	}
	rate := seqs / itlSec
	if !finite(rate) {
		return 0
	}
	return rate
}

// reachableFill is the fill a replica can actually reach at k: k itself, unless
// the engine's max_num_seqs admits fewer sequences than k implies. Then the
// cache never fills past maxNumSeqs*KVreq/C, and pricing the ITL of k would
// charge the latency of a load the engine cannot carry. Never below minFill,
// where the line was not fitted: the ITL there is the higher, safer reading.
func reachableFill(k, capacityTokens, kvPerRequest float64, maxNumSeqs int64) float64 {
	if maxNumSeqs <= 0 || !(capacityTokens > 0) || !(kvPerRequest > 0) {
		return k
	}
	kCap := float64(maxNumSeqs) * kvPerRequest / capacityTokens
	if kCap >= k {
		return k
	}
	return max(kCap, minFill)
}

// realisedFill is the fill a replica actually runs at once the fleet carries
// scale-up headroom: N_slo / scaleUp replicas share the load, so each sustains
// scaleUp times the token rate it was priced at. The rate is not linear in k,
// so the fill that delivers it is found by bisection rather than by scaling k.
// Returns k itself when the headroom is unusable, and NaN when the rate is.
func realisedFill(line itl.Model, k, scaleUp, capacityTokens, kvPerRequest float64, maxNumSeqs int64) float64 {
	priced := replicaTokenRate(line, k, capacityTokens, kvPerRequest, maxNumSeqs)
	if !(priced > 0) {
		return math.NaN()
	}
	if !(scaleUp > 0) || scaleUp >= 1 {
		return k
	}
	want := scaleUp * priced
	lo, hi := 0.0, k
	for range 60 {
		mid := (lo + hi) / 2
		if replicaTokenRate(line, mid, capacityTokens, kvPerRequest, maxNumSeqs) < want {
			lo = mid
		} else {
			hi = mid
		}
	}
	// Below the fitted range the line says nothing; the ITL at its bottom is
	// the higher, safer reading.
	return max(hi, minFill)
}

func finite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}
