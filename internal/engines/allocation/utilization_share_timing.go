package allocation

import "time"

// Defaults the cluster applies when a ScaledObject or pod template leaves a
// value unset, and the fixed parts of a release and a fill (§8.4).
const (
	hpaDefaultScaleDownWindow  = 300 * time.Second
	kedaDefaultPolling         = 30 * time.Second
	k8sDefaultTerminationGrace = 30 * time.Second
	hpaSyncPeriod              = 15 * time.Second
	// shareModelLoadEstimate stands in for pod start and model load until a
	// group has measured releases of its own.
	shareModelLoadEstimate = 5 * time.Minute
	// shareSchedulingAllowance is how long scheduling a receiver may take
	// before its fill times out; a gang-scheduled LWS group gets twice this.
	shareSchedulingAllowance = time.Minute
)

// ShareTimingInputs are the cluster facts a group's timings derive from: the
// slowest of its donors' configurations. A nil value is unset, and the
// default the cluster applies is used.
type ShareTimingInputs struct {
	Cycle            time.Duration
	ScaleDownWindow  *time.Duration
	PollingInterval  *time.Duration
	TerminationGrace *time.Duration
	// LWS is true when any role of the group is a LeaderWorkerSet, whose
	// groups are gang-scheduled.
	LWS bool
}

// ShareTimingSource says where each derived value came from, for the log line
// and the effective-timing gauge (§8.4): measured, scaledobject, pod, default.
type ShareTimingSource map[string]string

// DeriveShareTimings computes a group's timings from cluster facts and, once
// it has them, the group's own measured releases (§8.4). Nothing here is
// configuration: the values are facts the cluster states, or are measured, and
// the multipliers are those §6.7's simulation validated.
//
//   - the release bound is the scale-down window, the HPA sync, the KEDA poll
//     and the donor pods' termination grace -- an upper bound, so the release
//     timeout built on it does not abort a release that is merely slow;
//   - the release time is the measured p90 once three releases have
//     completed, else the bound; the hold and the swing window build on it,
//     so a long drain grace that is rarely used does not stretch them.
func DeriveShareTimings(in ShareTimingInputs, l *ShareLedger) (ShareTimings, ShareTimingSource) {
	src := ShareTimingSource{}
	pick := func(name string, v *time.Duration, def time.Duration, from string) time.Duration {
		if v != nil && *v >= 0 {
			src[name] = from
			return *v
		}
		src[name] = "default"
		return def
	}
	window := pick("window", in.ScaleDownWindow, hpaDefaultScaleDownWindow, "scaledobject")
	polling := pick("polling", in.PollingInterval, kedaDefaultPolling, "scaledobject")
	grace := pick("grace", in.TerminationGrace, k8sDefaultTerminationGrace, "pod")

	bound := window + hpaSyncPeriod + polling + grace
	release := bound
	src["release"] = "bound"
	if l != nil {
		if m, ok := l.MeasuredRelease(); ok {
			release = m
			src["release"] = "measured"
		}
	}
	scheduling := shareSchedulingAllowance
	if in.LWS {
		scheduling *= 2
	}
	latency := release + shareModelLoadEstimate + 3*in.Cycle
	return ShareTimings{
		Window:         window,
		ReleaseTimeout: bound*3/2 + 2*in.Cycle,
		FillTimeout:    polling + hpaSyncPeriod + 2*in.Cycle + scheduling,
		ReversalHold:   2 * release,
		SwingWindow:    8 * latency,
	}, src
}
