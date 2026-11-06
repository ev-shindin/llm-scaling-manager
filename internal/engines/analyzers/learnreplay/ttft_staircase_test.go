package learnreplay

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The what-if TTFT staircase's recordings, one golden per phase
// (docs/proposals/slo-capacity-planner-ttft.md, "As built (M2)"). The router's
// and the driver's figures live outside the repository; what these tests
// hold is what the recordings themselves say: S2's gate, and the router's
// flow-control series.

// staircaseFixtures is the staircase's goldens, in the order they ran.
var staircaseFixtures = []string{"ttft-staircase-a-off", "ttft-staircase-a-on", "ttft-staircase-b-off",
	"ttft-staircase-b-on", "ttft-staircase-c-off", "ttft-staircase-c-on"}

// staircaseCycles is what one phase's replay says of S2's gate
// (slo-tiers.md, "What this leaves for S2's gate"), by cycle: slo analyzed
// on every cycle of every phase, so each count is of cycles with slo listed.
type staircaseCycles struct {
	// cycles is the slo analyzer's TTFT readings, one a cycle.
	cycles int
	// vp and a are the cycles with V_P, and with a, from P1.
	vp, a int
	// decodeLine is the cycles with a decode line that prices.
	decodeLine int
	// vpAndDecode is the cycles with V_P and a decode line at once, and
	// allAtOnce those with a too: what S2's gate asks of one cycle.
	vpAndDecode, allAtOnce int
	// priced is the cycles S1 answered with a number.
	priced int
}

// staircaseSeries is what one phase's recording holds of the router's
// flow-control series, by cycle.
type staircaseSeries struct {
	// gaugeEmpty and gaugeQueued are the cycles the queue gauge read 0, and
	// above 0; gaugeEmptyIdle those of gaugeEmpty with no arrivals.
	gaugeEmpty, gaugeQueued, gaugeEmptyIdle int
	// noGauge is the cycles with no gauge at all, and gaugeless those of them
	// after the first cycle that held one: a gauge that went missing.
	noGauge, gaugeless int
	// waitBuckets is the cycles holding the wait histogram.
	waitBuckets int
}

var ttftStaircase = []struct {
	fixture string
	on      bool
	gate    staircaseCycles
	series  staircaseSeries
}{
	{"ttft-staircase-a-off", false,
		staircaseCycles{cycles: 128},
		staircaseSeries{noGauge: 128}},
	{"ttft-staircase-a-on", true,
		staircaseCycles{cycles: 130},
		staircaseSeries{gaugeEmpty: 126, gaugeEmptyIdle: 3, noGauge: 4, waitBuckets: 125}},
	{"ttft-staircase-b-off", false,
		staircaseCycles{cycles: 289, vp: 29},
		staircaseSeries{noGauge: 289, waitBuckets: 15}},
	{"ttft-staircase-b-on", true,
		staircaseCycles{cycles: 289, decodeLine: 34},
		staircaseSeries{gaugeEmpty: 281, gaugeQueued: 4, gaugeEmptyIdle: 3, noGauge: 4, waitBuckets: 284}},
	{"ttft-staircase-c-off", false,
		staircaseCycles{cycles: 286, vp: 30},
		staircaseSeries{noGauge: 286, waitBuckets: 14}},
	{"ttft-staircase-c-on", true,
		staircaseCycles{cycles: 292, vp: 36},
		staircaseSeries{gaugeEmpty: 245, gaugeQueued: 43, gaugeEmptyIdle: 3, noGauge: 4, waitBuckets: 288}},
}

// logField reads one field of a funcr JSON line. Not a JSON decode: the
// slo-ttft-variant line holds NaN, which is no JSON number.
func logField(t *testing.T, line, field string) string {
	t.Helper()
	m := regexp.MustCompile(`[{,]"` + field + `":"?([^",}]*)`).FindStringSubmatch(line)
	require.NotNil(t, m, "no %s in %s", field, line)
	return m[1]
}

// loggedPositive reports whether a logged number is above zero.
func loggedPositive(t *testing.T, s string) bool {
	t.Helper()
	v, err := strconv.ParseFloat(s, 64)
	require.NoError(t, err)
	return v > 0
}

// staircaseRecording is a golden's recording, uncompressed.
func staircaseRecording(t *testing.T, fixture string) []byte {
	t.Helper()
	gz, err := os.ReadFile(filepath.Join(goldenDir, fixture, RecordingName))
	require.NoError(t, err)
	raw, err := gunzip(gz)
	require.NoError(t, err)
	return raw
}

// readGate replays a recording and counts S2's gate from the slo analyzer's
// own line for each cycle.
func readGate(t *testing.T, raw []byte) staircaseCycles {
	t.Helper()
	var lines []string
	logger := funcr.NewJSON(func(obj string) {
		if strings.Contains(obj, `"msg":"slo-ttft-variant"`) {
			lines = append(lines, obj)
		}
	}, funcr.Options{Verbosity: 10})
	_, err := Replay(logr.NewContext(context.Background(), logger), raw, Options{Mode: Normal})
	require.NoError(t, err)
	var got staircaseCycles
	for _, l := range lines {
		vp, a := loggedPositive(t, logField(t, l, "vp")), loggedPositive(t, logField(t, l, "a"))
		decode := logField(t, l, "decodeVariant") != "none"
		got.cycles++
		count := func(n *int, yes bool) {
			if yes {
				*n++
			}
		}
		count(&got.vp, vp)
		count(&got.a, a)
		count(&got.decodeLine, decode)
		count(&got.vpAndDecode, vp && decode)
		count(&got.allAtOnce, vp && a && decode)
		count(&got.priced, logField(t, l, "reason") == constants.SLOTTFTReasonPriced)
	}
	return got
}

// No phase holds S2's gate on one cycle: V_P, a, slo and a decode line that
// prices. So S1 prices no cycle of the staircase, and the staircase's
// validation rests on the warm-up's state and the frozen prediction, not on
// these replays. The counts are frozen: a change that lets a phase price one
// moves them, which is the moment to read the phase as S2's evidence.
func TestGoldens_TTFTStaircaseHoldsNoS2GateCycle(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	for _, c := range ttftStaircase {
		got := readGate(t, staircaseRecording(t, c.fixture))
		t.Logf("%s: %+v", c.fixture, got)
		assert.Positive(t, got.cycles, c.fixture)
		assert.Equal(t, c.gate, got, c.fixture)
	}
}

// readSeries counts what a recording's inputs hold of the router's
// flow-control series.
func readSeries(t *testing.T, raw []byte) staircaseSeries {
	t.Helper()
	rec, err := learnrec.Read(raw)
	require.NoError(t, err)
	var got staircaseSeries
	seen := false
	for _, ev := range rec.Events {
		if ev.Kind != saturation.KindAnalyze {
			continue
		}
		var d saturation.AnalyzeEvent
		require.NoError(t, ev.DecodeData(&d))
		in, err := decodeInput(d.Input)
		require.NoError(t, err)
		switch q := in.SchedulerQueue; {
		case q == nil:
			got.noGauge++
			if seen {
				got.gaugeless++
			}
		case q.QueueSize > 0:
			seen = true
			got.gaugeQueued++
		default:
			seen = true
			got.gaugeEmpty++
			if in.ArrivalRate == 0 {
				got.gaugeEmptyIdle++
			}
		}
		if in.RouterLatency != nil && len(in.RouterLatency.FlowControlWaitBuckets) > 0 {
			got.waitBuckets++
		}
	}
	return got
}

// An on gate publishes the queue gauge at 0 while the queue is empty, once a
// request has passed; an off gate never publishes it. The first is what
// slo-tiers.md's "declared enabled with no series" assumed, and no recording
// before these could say: the three on phases are the first with the gate on.
//
// Each on phase's first 4 cycles hold no gauge: no request had reached the
// new router yet. So a router that has served nothing reads as no series, and
// that much of the mismatch stays. The wait histogram is no signal of the
// gate: the two off phases that follow an on phase hold it for their first
// minutes, while the on phase's counters are still inside the 5 minute rate
// window.
func TestGoldens_TTFTStaircaseFlowControlSeries(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	for _, c := range ttftStaircase {
		got := readSeries(t, staircaseRecording(t, c.fixture))
		t.Logf("%s: %+v", c.fixture, got)
		assert.Equal(t, c.series, got, c.fixture)
		if c.on {
			assert.Equal(t, 4, got.noGauge, "%s: the cycles before the first request", c.fixture)
			assert.Zero(t, got.gaugeless, "%s: the gauge went missing after it first appeared", c.fixture)
			assert.Positive(t, got.gaugeEmpty, "%s: an on gate with an empty queue published no gauge", c.fixture)
			assert.Positive(t, got.gaugeEmptyIdle, "%s: no idle cycle read the gauge", c.fixture)
		} else {
			assert.Equal(t, got.noGauge, got.gaugeEmpty+got.gaugeQueued+got.noGauge, "%s: an off gate published the gauge", c.fixture)
		}
	}
}

// staircaseSwitchMoves is what each switch of replaySwitches, alone from all
// off, moves in the staircase goldens; staircaseSeamMoves what each earlier
// seam, off alone from all on, moves in them. Measured at the commit that
// added them, with the recordings made at 59772fce2. Every other switch and
// seam moves none of them.
//
//   - slo.SwitchTTFTPrice is S1: slo is listed, so each cycle has a reading.
//   - saturation.SwitchStartBlendsFirstSamples moves RoleStartSeconds (121.5
//     to 111.7 on the first row of b-off).
//   - saturation.SwitchK2FirstReadingWaits moves b-off, c-off and c-on, and
//     saturation.SwitchKVPerRequestOutputDispersion all six.
var staircaseSwitchMoves = map[string][]string{
	saturation.SwitchStartBlendsFirstSamples: {
		"ttft-staircase-a-off/composite-results.csv", "ttft-staircase-a-off/results.csv",
		"ttft-staircase-a-on/composite-results.csv", "ttft-staircase-a-on/results.csv",
		"ttft-staircase-b-off/composite-results.csv", "ttft-staircase-b-off/results.csv",
		"ttft-staircase-b-on/composite-results.csv", "ttft-staircase-b-on/results.csv",
		"ttft-staircase-c-off/composite-results.csv", "ttft-staircase-c-off/results.csv",
		"ttft-staircase-c-on/composite-results.csv", "ttft-staircase-c-on/results.csv",
	},
	slo.SwitchTTFTPrice: {
		"ttft-staircase-a-off/slo-published.csv",
		"ttft-staircase-a-on/slo-published.csv",
		"ttft-staircase-b-off/slo-published.csv",
		"ttft-staircase-b-on/slo-published.csv",
		"ttft-staircase-c-off/slo-published.csv",
		"ttft-staircase-c-on/slo-published.csv",
	},
}

var staircaseSeamMoves = map[string][]string{
	saturation.SwitchK2FirstReadingWaits: {
		"ttft-staircase-b-off/variant-capacities.csv",
		"ttft-staircase-c-off/composite-results.csv", "ttft-staircase-c-off/composite-roles.csv",
		"ttft-staircase-c-off/composite-variants.csv", "ttft-staircase-c-off/variant-capacities.csv",
		"ttft-staircase-c-off/warm-pool-pressure.csv",
		"ttft-staircase-c-on/variant-capacities.csv",
	},
	saturation.SwitchKVPerRequestOutputDispersion: {
		"ttft-staircase-a-off/composite-results.csv", "ttft-staircase-a-off/composite-roles.csv",
		"ttft-staircase-a-off/composite-variants.csv", "ttft-staircase-a-off/store-records.csv",
		"ttft-staircase-a-off/variant-capacities.csv", "ttft-staircase-a-off/warm-pool-pressure.csv",
		"ttft-staircase-a-on/composite-results.csv", "ttft-staircase-a-on/composite-roles.csv",
		"ttft-staircase-a-on/composite-variants.csv", "ttft-staircase-a-on/store-records.csv",
		"ttft-staircase-a-on/variant-capacities.csv", "ttft-staircase-a-on/warm-pool-pressure.csv",
		"ttft-staircase-b-off/composite-results.csv", "ttft-staircase-b-off/composite-roles.csv",
		"ttft-staircase-b-off/composite-variants.csv", "ttft-staircase-b-off/results.csv",
		"ttft-staircase-b-off/store-records.csv", "ttft-staircase-b-off/variant-capacities.csv",
		"ttft-staircase-b-on/composite-results.csv", "ttft-staircase-b-on/composite-roles.csv",
		"ttft-staircase-b-on/results.csv", "ttft-staircase-b-on/variant-capacities.csv",
		"ttft-staircase-c-off/composite-results.csv", "ttft-staircase-c-off/composite-roles.csv",
		"ttft-staircase-c-off/results.csv", "ttft-staircase-c-off/variant-capacities.csv",
		"ttft-staircase-c-on/composite-results.csv", "ttft-staircase-c-on/composite-roles.csv",
		"ttft-staircase-c-on/results.csv", "ttft-staircase-c-on/variant-capacities.csv",
	},
}

// staircaseBase is the sha256 of each output replaySwitches moves, replayed
// with all of its switches off: the staircase goldens have no base commit.
var staircaseBase = map[string]string{
	"ttft-staircase-a-off/composite-results.csv": "b9370e54b3dcb83c0ddef99adc82e14977f7f09c102355685151a7756b860cbf",
	"ttft-staircase-a-off/results.csv":           "40a230b5f61cbf849262b771347d2b582d205f8c63a2bd13c3c40975f5965c51",
	"ttft-staircase-a-off/slo-published.csv":     "d08ec336ae845d257b51e51a09d824d4dd9ff69e648ea665a26aac271776445f",
	"ttft-staircase-a-on/composite-results.csv":  "cfe0f38884aff0addb094aa7e15209399bbc7b6426d279aee6832b52d04e5a44",
	"ttft-staircase-a-on/results.csv":            "81ba84b1ce75653e63b5ca0b651668077fc0246493d1a54c41d72f42eef8f266",
	"ttft-staircase-a-on/slo-published.csv":      "bee777d4880e914f40fbb67bc6df85825ff0b78c9a2a88dea183e44196f3cb34",
	"ttft-staircase-b-off/composite-results.csv": "1efe56bdcae56cac7f7811e61f6718a8c9ffd6f3af95fc0a55419e7c08ec29cc",
	"ttft-staircase-b-off/results.csv":           "c698695070f8e3882a48f8713221c8d3eb9d9cc949c71670f01b7e0387a16175",
	"ttft-staircase-b-off/slo-published.csv":     "b80436315ca91b6be8c58f0f0ada03448c2e16f4258759af1df11cab10ac2beb",
	"ttft-staircase-b-on/composite-results.csv":  "5cd25e0d0b1dc60385ccadfd7898f4528cbafbbb0cbd71da8854bd287936cd89",
	"ttft-staircase-b-on/results.csv":            "6d9d32f9a37f382e0693cd42b2993bf3ad6c82e180243094a767c98689ccb373",
	"ttft-staircase-b-on/slo-published.csv":      "28d8b2aac88fbb33babed2502651640158ccfb65e0ce22e43e6fc942373a1bfa",
	"ttft-staircase-c-off/composite-results.csv": "cf0baec6450ef374588b293b0829ee2652ce9cb2400e16f05aff8abb960a6f79",
	"ttft-staircase-c-off/results.csv":           "6672e1ff6b4ebf7757adb86927926bfd3ba752da12144828637a54309b9c85d2",
	"ttft-staircase-c-off/slo-published.csv":     "8766d3086ca1fe705fc7bafd53d6c5ec58c9107f111acf12a59391f2032440d2",
	"ttft-staircase-c-on/composite-results.csv":  "9f0616e5e8552357ac815f33d8f59ece8465db642faa08ef4b2b970a3505e5e2",
	"ttft-staircase-c-on/results.csv":            "c1e27a80d68e5e70ba94988e7a6c72a7e3ebdb2181709f133360e5efdbda60da",
	"ttft-staircase-c-on/slo-published.csv":      "ad46354cbb452fec5116aadac1cefd387638136226fcf495c25e5bd73c999a6f",
}
