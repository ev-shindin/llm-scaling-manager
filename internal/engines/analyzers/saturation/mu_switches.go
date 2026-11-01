package saturation

// Mu's replay switches (docs/proposals/analyzer-learner.md, 2.5 and section 7,
// PR 3): one per named change, each on from the constructor, so production
// runs the change, and turned off only by a replay (SetSwitchesForReplay).
// None reads a policy key, and no policy key turns one off.

// SwitchMuRepeatIsUse is the replay switch for an exact repeat of mu's last
// reading (analyzer-learner.md, 3.4). A saturated rate read past the sample
// spacing at exactly the value last read is the same scrape pair read again,
// or a frozen series: evidence that the window is in use, not that the
// bucket saturated again. On, such a repeat only touches the window's use
// clock: it neither keeps the window's episode alive (its write clock, which
// EpisodeGap ages and learner reports publish as the last admitted time)
// nor keeps the measurement fresh against the derived figure (muChoice's
// agreement clock). Off, as before, it was taken as a write and as an
// agreeing reading. The prefill bound (row 7) writes through the same fold
// and keeps the old rule either way: it moves in its own PR.
const SwitchMuRepeatIsUse = "mu-exact-repeat-is-use"

// SwitchMuUnknownAgeSafe is the replay switch for a measured mu whose age is
// unknown (analyzer-learner.md, 3.4). A window with no sample time and no
// agreeing reading on record -- restored from a document that carried no
// sample time -- was taken as stale as can be, and a measurement under the
// derived figure skipped its MeasuredLowHoldFor hold and gave way to the
// higher derived figure at once: the side that under-orders. On, the
// unknown age resolves to the safe side on each side: above the derived
// figure it is still stale (the measurement yields to the lower figure), and
// under it the hold counts from the window's own last write, which is never
// older than its newest sample, so the lower figure prices for at least as
// long as a known sample time would let it.
const SwitchMuUnknownAgeSafe = "mu-unknown-age-resolves-safe"

// SwitchMuLatchRestoresSafe is the replay switch for mu's latch after a
// controller restart (analyzer-learner.md, 3.5 and the mu choice). The
// latch (muChoice.yielded) is not persisted, so a window restored, or brought
// back, with no choice on record came back unlatched: a stale measurement
// between MeasuredDerivedReturnRatio and MeasuredDerivedMaxRatio above the
// derived figure priced from the higher, measured figure, the side that
// under-orders. On, a latch with no evidence behind it restores to the safe
// side: latched while the measurement sits above the derived figure (it
// yields to the lower one past the return ratio), unlatched while it sits
// under it (it keeps pricing the lower one).
const SwitchMuLatchRestoresSafe = "mu-latch-restores-safe-side"
