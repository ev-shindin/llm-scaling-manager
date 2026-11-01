// Package learn is the learners' shared vocabulary (docs/proposals/
// analyzer-learner.md, 6.3): the Report a learner fills each cycle, the
// sources and reasons it fills it with, and the plain list of learners the
// forget-learned-state annotation reaches. It holds types only: a learner
// fills the Report, the analyzer logs it, and nothing here logs, registers
// a metric, or drives a learner.
//
// It imports only the standard library, so domain and internal/metrics can
// use its types without a cycle.
package learn

import (
	"maps"
	"slices"
	"time"
)

// K2 is the k2 learner: KV at saturation, a variant's own window (the
// saturation analyzer's row 3) and the lower bound it borrows from a
// sibling (row 4).
const K2 = "k2"

// Learners is every learner the forget-learned-state annotation reaches, by
// name, in the order Forget reaches them. A plain list, not a registry: each
// analyzer forgets a name on it with code of its own, and a test holds every
// name to that code. A learner joins the list in the PR that moves it onto
// the shared rules.
func Learners() []string {
	return []string{K2}
}

// By is what forgot a key.
type By string

const (
	// ByOperator is the forget-learned-state annotation.
	ByOperator By = "operator"
	// ByEvicted is the learner's own sweep: the key went unused past its
	// horizon.
	ByEvicted By = "evicted"
)

// Forgotten is one key a learner forgot, and what forgot it.
type Forgotten struct {
	Key string
	By  By
}

// Source is where a learner's figure came from, in the learner's own
// vocabulary (k2's P1-obs, P2-hist and the rest). A restore is not a source.
type Source string

// Reason is an admission outcome: why a reading was admitted, or refused.
// Each learner names its own.
type Reason string

// Report is one learner's account of one variant for one cycle.
type Report struct {
	Learner   string
	Namespace string
	ModelID   string
	Variant   string
	Role      string
	// PricingKey is the key that priced the variant this cycle, "" when no
	// key did.
	PricingKey string
	Source     Source
	// Counts is the cycle's admission outcomes by reason.
	Counts map[Reason]int64
	// Samples is how many readings the pricing key's window holds, and
	// LastAdmitted when the newest of them was admitted, zero when none.
	Samples      int
	LastAdmitted time.Time
	// Writes is how many readings the cycle admitted.
	Writes int64
	// Forgotten is the keys of the variant the learner forgot since its last
	// report, and what forgot each.
	Forgotten []Forgotten
}

// Clone is a deep copy of r.
func (r Report) Clone() Report {
	r.Counts = maps.Clone(r.Counts)
	r.Forgotten = slices.Clone(r.Forgotten)
	return r
}
