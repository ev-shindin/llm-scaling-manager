package domain

// Service-rate signal sources: which path answered a variant's per-replica
// service rate. A closed set, published as the source label of
// wva_service_rate_per_replica, so a dashboard can tell a rate priced for the
// shape arriving now from one remembered from a saturated cycle.
const (
	// ServiceRateSourceDerived is a rate derived from the variant's OWN
	// ITL(k) line at the pricing k.
	ServiceRateSourceDerived = "derived"
	// ServiceRateSourceBorrowedLine is a rate derived from a line a sibling
	// with the same engine configuration fitted (enableLearnedStateReuse).
	// The arithmetic is the same as derived, the authority is not: the
	// throughput floor may hold a fleet on it and may not grow one -- the
	// floor's heldWhy=borrowed-line.
	ServiceRateSourceBorrowedLine = "borrowed-line"
	// ServiceRateSourceMeasured is a rate read from the variant's own
	// saturated-throughput window for the output-length bucket the fleet is
	// serving.
	ServiceRateSourceMeasured = "measured"
	// ServiceRateSourceNeighbour is a rate read from a NEIGHBOURING bucket's
	// window, because the bucket the fleet is serving has no reading of its
	// own yet. Wrong in a known direction; the throughput floor may hold a
	// fleet on it and may not grow one.
	//
	// Also the variant's OWN bucket's window while the fleet's output length
	// is in a swap the key has not followed yet (up to the 2-minute dwell of
	// the saturation analyzer's bucket band): the window holds the shape
	// being left, so it takes no readings and is held, not ordered on.
	//
	// A measured window too thin to order on (fewer readings than
	// floor.MinThroughputSamplesToOrder) is a third hold-only case, and is
	// not a source of its own: it is measured, and the floor's heldWhy says
	// single-sample.
	ServiceRateSourceNeighbour = "neighbour"
	// ServiceRateSourceLowerBound is a prefill rate the variant was seen to
	// sustain with its queue saturated while its own window could not record
	// it, usually because decode was saturated and metering it. The true
	// rate is at least this. The throughput floor may grow prefill on it, up
	// to a growth cap, and an own measured window replaces it.
	ServiceRateSourceLowerBound = "lower-bound"
)

// ServiceRateSignal is what the saturation analyzer priced one variant's
// service rate on in one cycle: the ITL(k) line and the completion rate one
// replica sustains at saturation.
//
// One struct, filled where the cycle already holds the values, so that what
// the analyzer decided on and what it publishes cannot drift apart -- the
// engine reads this and nothing else to set the wva_itl_line_* and
// wva_service_rate_per_replica series (docs/proposals/signals-as-metrics.md,
// "Making it not rot"). Diagnostic only: nothing sizes capacity from it.
type ServiceRateSignal struct {
	// HasLine is set when the variant has an ITL(k) = A*k + B line this
	// cycle, its own or a borrowed one, that is positive at the pricing k --
	// the line a derived rate would come from -- and has replicas reporting.
	// LineSlope is A and LineIntercept is B, in seconds; ITLAtPricingK is the
	// line evaluated at the pricing k (AnalyzerResult.PricingK). All three are
	// zero when HasLine is false.
	HasLine       bool
	LineSlope     float64
	LineIntercept float64
	ITLAtPricingK float64

	// ITLWindowed is set for a variant that keeps an ITL window -- decode
	// only, because ITL is the gap between generated tokens and a prefill
	// replica generates one -- and has replicas reporting: a variant parked at
	// zero keeps its window for a while and publishes none of it. LineSamples
	// is how many (k, ITL) readings the variant's OWN window holds, published
	// even when it is zero and there is no line: it is the number that
	// explains an absent one.
	ITLWindowed bool
	LineSamples int

	// Role is the variant's canonical role (prefill, decode, both).
	Role string

	// Rate is the per-replica saturated completion rate, requests/s: the
	// median over the variant's own replicas (never a bridge) that have one.
	// RateSource is one of the ServiceRateSource* constants, or "" when no
	// own replica has a rate -- in which case Rate is 0 and nothing is
	// published for it.
	Rate       float64
	RateSource string
}
