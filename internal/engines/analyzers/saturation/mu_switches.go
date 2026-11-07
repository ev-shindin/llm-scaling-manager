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

// SwitchMuWeighsFleet is the replay switch for one slow pod in a sample
// (mu_slow_pod.go; analyzer-learner.md, 2.6). On, a reading under half of what
// a Ready replica served counts by its weight, and a sample whose readers sit
// under the tolerance of what the replicas it did not read serve counts those
// too, so it is the fleet's and not the part that queued. Off, as before, that
// reading was refused, and the sample was the readers' mean alone.
const SwitchMuWeighsFleet = "mu-sample-weighs-the-fleet"

// SwitchMuYieldCappedAtMeasurement is the replay switch for the sibling
// yield's cap (yieldCapped; analyzer-learner.md, 2.6 and 3.4). On, a yield
// never prices mu above the measurement it replaces; since the yield opens
// only under the tolerance of a higher figure, it no longer moves the price.
// Off, as before, a measurement under SiblingMuTolerance of its full
// siblings' lower median priced at that median, which left the slow pod out
// and sat over the fleet's mean.
const SwitchMuYieldCappedAtMeasurement = "mu-yield-capped-at-measurement"

// SwitchMuUnreadAtTarget is the replay switch for the rate an unread replica
// under the KV line counts at (servedPod.credit; analyzer-learner.md, 2.6).
// On, it counts at what it serves over the scale-up target: its rate is what
// it was offered, not its limit, and the floor charges the target on mu
// again. Off, as SwitchMuWeighsFleet alone, it counted at what it serves.
const SwitchMuUnreadAtTarget = "mu-unread-replica-at-target"

// SwitchMuMinorityFigureExpires is the replay switch for a measured mu no
// line can replace, written by a minority of the fleet
// (minorityFigureExpired; analyzer-learner.md, 3.4). On, a figure under the
// fleet's served bound gives way to that bound after MuChoice.LowHoldFor with
// no saturated reading in agreement with it. Off, as before, with no line the
// measurement stood for good.
const SwitchMuMinorityFigureExpires = "mu-minority-figure-expires"
