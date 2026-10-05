// Package fleet aggregates per-replica readings into the one figure a fleet is
// priced from.
package fleet

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// Mean is the request-rate-weighted mean of value over the replicas include
// selects: the figure the fleet is actually serving, rather than the mean of
// what its replicas happen to report.
//
// Weighting by request rate is what makes it robust to a fleet that is
// changing size. A fresh replica whose first completions are the short
// requests (they finish first) reports a short average at a low rate and
// barely moves it; a replica with no completions yet reports nothing and does
// not move it at all. With no rate reported anywhere it falls back to the
// plain mean, and it is zero when nothing reports the value at all.
//
// This exists as one function because it was previously three, and the three
// had drifted apart in ways no caller had chosen:
//
//   - saturation_v2.fleetAverage skipped a value of zero and, until 08d8ad6c,
//     admitted NaN -- `v <= 0` is false for NaN, so one bad replica turned the
//     whole fleet's average into NaN and every figure priced from it followed.
//   - saturation_v2.fleetPrefixHitRate hand-rolled the same body to treat zero
//     as a reading, and was the only one of the three that rejected NaN.
//   - throughput.averageShapeMetrics computed three means in one pass behind a
//     joint gate, and was never audited for NaN at all.
//
// NaN is rejected here, once, for every caller. A caller cannot opt back into
// it: there is no reading a NaN could represent.
//
// Zero, on the other hand, is a real disagreement between callers, so it is an
// option rather than a default. A token length of zero means "this replica has
// not completed anything" -- absent, and including it would drag the mean
// toward a figure nobody is serving. A prefix-cache hit rate of zero means
// "caching is off", which is a measurement, and skipping it would leave the
// mean to whichever replica happened to report something.
func Mean(
	replicas []domain.ReplicaMetrics,
	value func(domain.ReplicaMetrics) float64,
	include func(domain.ReplicaMetrics) bool,
	opts ...Option,
) float64 {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}

	var weighted, weights, plain float64
	var n int
	for _, rm := range replicas {
		v := value(rm)
		if !cfg.admits(v) || !include(rm) {
			continue
		}
		plain += v
		n++
		if rm.RequestRate > 0 {
			weighted += v * rm.RequestRate
			weights += rm.RequestRate
		}
	}
	if weights > 0 {
		return weighted / weights
	}
	if n > 0 {
		return plain / float64(n)
	}
	return 0
}

// Option narrows which readings Mean admits.
type Option func(*config)

// ZeroIsAReading admits a value of exactly zero.
//
// Off by default, because for the quantity this is usually asked about -- a
// token length -- zero means the replica has completed nothing, and averaging
// that in reports a shape the fleet is not serving. Pass it for a quantity
// whose zero is a measurement: a prefix-cache hit rate of zero is what a fleet
// with caching disabled reports on every replica.
func ZeroIsAReading() Option {
	return func(c *config) { c.zeroIsReading = true }
}

// Within rejects a reading outside [lo, hi].
//
// For a quantity with a definition range, where a value outside it is a
// scraping or arithmetic artefact rather than an unusual fleet: a hit rate is
// a fraction, so 1.4 is not a hit rate.
func Within(lo, hi float64) Option {
	return func(c *config) { c.lo, c.hi, c.bounded = lo, hi, true }
}

type config struct {
	zeroIsReading bool
	lo, hi        float64
	bounded       bool
}

// admits reports whether v is a reading at all, before include is consulted.
//
// The NaN test is explicit and first. Every comparison against NaN is false,
// so neither `v <= 0` nor a range test rejects it; it would pass straight into
// both accumulators and poison them.
func (c config) admits(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	if c.zeroIsReading {
		if v < 0 {
			return false
		}
	} else if v <= 0 {
		return false
	}
	if c.bounded && (v < c.lo || v > c.hi) {
		return false
	}
	return true
}
