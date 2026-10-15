package steadystate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
)

// borrowingState is a state holding one running borrow and one forget mark,
// the two entries whose clocks move every cycle.
func borrowingState(now time.Time) saturation.LearnedState {
	return saturation.LearnedState{
		K2BorrowedSince: map[string]time.Time{"ns|v|m|H100|1|both|b0|q5": now.Add(-time.Hour)},
		K2BorrowState: map[string]saturation.BorrowStateSnapshot{
			"ns|v|m|H100|1|both|b0|q5": {Tick: now, Seen: now, DonorK2: 4000, Args: "fp"},
		},
		Forgotten: map[string]saturation.ForgetMarkSnapshot{"ns/w": {Token: "t1", Seen: now}},
	}
}

// A running borrow ticks every serving cycle and a forget mark is stamped
// every cycle its variant exists. Neither is something learned, so neither may
// change the digest: when they did, the document was rewritten every minute
// for as long as any borrow or forget existed.
func TestABorrowsTickAndAForgetMarksSeenDoNotChangeTheDigest(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	before := learnedStateDigest(borrowingState(now))
	require.NotEmpty(t, before)

	later := borrowingState(now)
	st := later.K2BorrowState["ns|v|m|H100|1|both|b0|q5"]
	st.Tick, st.Seen = now.Add(time.Minute), now.Add(time.Minute)
	later.K2BorrowState["ns|v|m|H100|1|both|b0|q5"] = st
	later.Forgotten["ns/w"] = saturation.ForgetMarkSnapshot{Token: "t1", Seen: now.Add(time.Minute)}
	assert.Equal(t, before, learnedStateDigest(later), "a cycle's clocks changed the digest")

	paused := borrowingState(now)
	paused.K2BorrowedSince["ns|v|m|H100|1|both|b0|q5"] = now.Add(-30 * time.Minute)
	assert.NotEqual(t, before, learnedStateDigest(paused), "a pause moves the start, which is learned")

	reforgot := borrowingState(now)
	reforgot.Forgotten["ns/w"] = saturation.ForgetMarkSnapshot{Token: "t2", Seen: now}
	assert.NotEqual(t, before, learnedStateDigest(reforgot), "a new token is learned")

	// The digest works on a copy: the caller's clocks are untouched.
	s := borrowingState(now)
	learnedStateDigest(s)
	assert.Equal(t, now, s.K2BorrowState["ns|v|m|H100|1|both|b0|q5"].Tick)
	assert.Equal(t, now, s.Forgotten["ns/w"].Seen)
}

// A spent borrow's start is the oldest time in the document, so ordered by
// its start it was the first entry the size bound dropped, and it went
// without its state. It is ordered by when its variant was last seen, and
// goes with its state.
func TestTheSizeBoundOrdersABorrowClockByItsVariantAndDropsItWhole(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	const key = "ns|v|m|H100|1|both|b0|q5"
	s := saturation.LearnedState{
		K2BorrowedSince: map[string]time.Time{key: now.Add(-30 * 24 * time.Hour)},
		K2BorrowState: map[string]saturation.BorrowStateSnapshot{
			key: {Tick: now, Seen: now, DonorK2: 4000, Args: "fp"},
		},
		VariantSeenAt: map[string]time.Time{"ns|m|old|H100|1": now.Add(-time.Hour)},
		Forgotten:     map[string]saturation.ForgetMarkSnapshot{"ns/w": {Token: "t1", Seen: now.Add(-2 * time.Hour)}},
	}
	entries := learnedStateEntries(s)
	require.Len(t, entries, 3)
	assert.Equal(t, now.Add(-2*time.Hour), entries[0].at, "the forget mark is an entry, by when it was last seen")
	assert.Equal(t, now.Add(-time.Hour), entries[1].at)
	assert.Equal(t, now, entries[2].at, "a spent clock went first, on its start")

	entries[2].drop()
	assert.NotContains(t, s.K2BorrowedSince, key)
	assert.NotContains(t, s.K2BorrowState, key, "the clock's state was left behind without its clock")
}
