package steadystate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
)

// lowerBoundDigestState is a state holding one running lower bound and one forget
// mark, the two entries whose clocks move every cycle.
func lowerBoundDigestState(now time.Time) saturation.LearnedState {
	return saturation.LearnedState{
		LowerBounds: map[string]saturation.LowerBoundSnapshot{
			"ns|v|m|H100|1|both|b0|q5": {
				Armed: true, Since: now.Add(-time.Hour), Tick: now, Seen: now,
				Donor: "w", DonorK2: 4000, Args: "fp",
			},
		},
		Forgotten: map[string]saturation.ForgetMarkSnapshot{"ns/w": {Token: "t1", Seen: now}},
	}
}

// A running bound ticks every serving cycle and a forget mark is stamped
// every cycle its variant exists. Neither is something learned, so neither may
// change the digest: when they did, the document was rewritten every minute
// for as long as any borrow or forget existed.
func TestABorrowsTickAndAForgetMarksSeenDoNotChangeTheDigest(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	const key = "ns|v|m|H100|1|both|b0|q5"
	before := learnedStateDigest(lowerBoundDigestState(now))
	require.NotEmpty(t, before)

	later := lowerBoundDigestState(now)
	lb := later.LowerBounds[key]
	lb.Tick, lb.Seen = now.Add(time.Minute), now.Add(time.Minute)
	later.LowerBounds[key] = lb
	later.Forgotten["ns/w"] = saturation.ForgetMarkSnapshot{Token: "t1", Seen: now.Add(time.Minute)}
	assert.Equal(t, before, learnedStateDigest(later), "a cycle's clocks changed the digest")

	paused := lowerBoundDigestState(now)
	lb = paused.LowerBounds[key]
	lb.Since = now.Add(-30 * time.Minute)
	paused.LowerBounds[key] = lb
	assert.NotEqual(t, before, learnedStateDigest(paused), "a pause moves the start, which is learned")

	reforgot := lowerBoundDigestState(now)
	reforgot.Forgotten["ns/w"] = saturation.ForgetMarkSnapshot{Token: "t2", Seen: now}
	assert.NotEqual(t, before, learnedStateDigest(reforgot), "a new token is learned")

	// The digest works on a copy: the caller's clocks are untouched.
	s := lowerBoundDigestState(now)
	learnedStateDigest(s)
	assert.Equal(t, now, s.LowerBounds[key].Tick)
	assert.Equal(t, now, s.Forgotten["ns/w"].Seen)
}

// A spent bound's start is the oldest time in the document, so ordered by its
// start it was the first entry the size bound dropped. It goes before every
// other entry, ordered among the spent ones by its last serving cycle, and
// goes whole, with the rollback copy of its start.
func TestTheSizeBoundOrdersABorrowClockByItsVariantAndDropsItWhole(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	const key = "ns|v|m|H100|1|both|b0|q5"
	s := saturation.LearnedState{
		LowerBounds: map[string]saturation.LowerBoundSnapshot{
			key: {Armed: true, Since: now.Add(-30 * 24 * time.Hour), Tick: now, Seen: now, DonorK2: 4000, Args: "fp"},
		},
		K2BorrowedSince: map[string]time.Time{key: now.Add(-30 * 24 * time.Hour)},
		VariantSeenAt:   map[string]time.Time{"ns|m|old|H100|1": now.Add(-time.Hour)},
		Forgotten:       map[string]saturation.ForgetMarkSnapshot{"ns/w": {Token: "t1", Seen: now.Add(-2 * time.Hour)}},
	}
	entries := learnedStateEntries(s)
	require.Len(t, entries, 3, "a bound and its rollback start are one entry")
	// A spent bound goes before everything else: it only keeps a borrow from
	// re-arming without new evidence. Ordered by when its variant was last
	// seen -- every cycle -- it outlived everything else in the document.
	assert.True(t, entries[0].spare, "the spent bound is the first to go")
	assert.Equal(t, now, entries[0].at, "ordered among the spent ones by its last serving cycle")
	assert.Equal(t, now.Add(-2*time.Hour), entries[1].at, "the forget mark is an entry, by when it was last seen")
	assert.Equal(t, now.Add(-time.Hour), entries[2].at)

	entries[0].drop()
	assert.NotContains(t, s.LowerBounds, key)
	assert.NotContains(t, s.K2BorrowedSince, key, "the rollback start was left behind without its bound")
}
