package learnreplay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
)

// The switch runs of the golden rule (analyzer-learner.md 2.5), over every
// committed golden, for this branch's switches: all off, each alone, all on.
//
//   - All on is the committed goldens (the golden check holds that too).
//   - All off is the base commit's goldens: an output no switch names is the
//     committed file, and an output a switch names is the base's, by digest
//     (switchBase).
//   - Each switch alone moves, from all off, only the outputs it names.
//
// A PR that adds a switch adds it to replaySwitches, and the base digests of
// the outputs it names. An earlier PR's switch whose seam is still in the
// code moves to earlierSeams, and is replayed off alone from all on; the PR
// that deletes a seam deletes its row. Every name SetSwitchesForReplay
// accepts has a row in one of the two.

// replaySwitches is each saturation replay switch, and the golden outputs
// (fixture/file) it is named to move.
//
// #415's band on the throughput (mu) key, its window's freeze while a swap
// is pending, and the clamp on its writes past the margin. Learner PR 2's
// and PR 3's switches, #396's bands and #399's batch cap are not here: "all
// off" is this PR's switches, and an earlier PR's stay at their constructor
// default, on, as the base goldens were written (analyzer-learner.md, 2.5).
var replaySwitches = map[string][]string{
	saturation.SwitchMuKeyBucketHeld: nil,
	saturation.SwitchMuSwapFreeze:    nil,
	saturation.SwitchMuMarginClamp:   nil,
}

// earlierSeams is each switch an earlier learner PR added whose seam the
// code still has, and the golden outputs that PR named it to move. Each
// stays on, at its constructor default, in this PR's runs; off alone, from
// all on, it may move only what it named. A seam's row goes with the seam,
// in the PR, or cleanup PR, that deletes it with zero golden diff.
var earlierSeams = map[string][]string{
	saturation.SwitchK2ReadsSettle: nil,
	saturation.SwitchK2WaitsAfterRestart: {
		"mu-paths/learned-state.csv", "mu-paths/store-records.csv", "mu-paths/variant-capacities.csv",
	},
	saturation.SwitchMuRepeatIsUse:       nil,
	saturation.SwitchMuUnknownAgeSafe:    nil,
	saturation.SwitchMuLatchRestoresSafe: nil,
	saturation.SwitchIdleBand:            nil,
	saturation.SwitchK2KeyBucketHeld:     nil,
	saturation.SwitchBatchCapDeclared:    nil,
	saturation.SwitchBatchCapLearned:     nil,
}

// heldBy is, for a switch that names no output, the unit tests that hold it:
// no committed golden moves under it, so the runs below see nothing of it,
// and a switch whose off path were dead would pass them. Each test named
// must exist (by its func declaration, in the package directory given).
var heldBy = map[string][]heldTest{
	saturation.SwitchIdleBand: {
		{"../saturation", "TestIdleBand_ARunStartsUnderTheLineAndEndsAtTheTop"},
		{"../saturation", "TestIdleBand_TheSwitchOffHoldsEveryReadingToTheLine"},
	},
	saturation.SwitchK2KeyBucketHeld: {
		{"../saturation", "TestK2KeyBucket_TheSmoothedLengthLeavesPastTheMargin"},
		{"../saturation", "TestK2KeyBucket_ASwapMovesItTheSameWayBothWays"},
		{"../saturation", "TestK2KeyBucket_OneReadingNeverMovesIt"},
		{"../saturation", "TestK2KeyBucket_APendingSwapHoldsTheBucket"},
		{"../saturation", "TestK2KeyBucket_AMovedKeyIsPricedFromTheBucketItLeft"},
		{"../saturation", "TestK2KeyBucket_TheSwitchPicksTheLength"},
	},
	saturation.SwitchMuKeyBucketHeld: {
		{"../saturation", "TestMuKeyBucket_TheSwitchPicksTheLength"},
		{"../saturation", "TestMuKeyBucket_TheKeysShareABucket"},
		{"../saturation", "TestMuKeyBucket_AMovedKeyIsCarriedLowerOnly"},
		{"../saturation", "TestMuKeyBucket_TheCarryPricesThroughAnalyze"},
	},
	saturation.SwitchMuSwapFreeze: {
		{"../saturation", "TestMuKeyBucket_APendingSwapFreezesTheWindow"},
		{"../saturation", "TestMuKeyBucket_APendingSwapLeavesPrefillWriting"},
	},
	saturation.SwitchMuMarginClamp: {
		{"../saturation", "TestMuKeyBucket_AMarginMoveNeverRaisesTheWindow"},
		{"../saturation", "TestMuKeyBucket_TheClampWritesNothingToAnEmptyWindow"},
	},
	saturation.SwitchK2ReadsSettle: {
		{"../saturation", "TestSettleView_TheTrackerSwitchMovesTheAbsenceRule"},
		{"../saturation", "TestSettleView_TheTrackerSwitchKeepsTheSettleThroughAZeroReadyTime"},
	},
	saturation.SwitchMuRepeatIsUse: {
		{"../saturation", "TestMuRepeat_IsUseNotAWrite"},
		{"../saturation", "TestMuRepeat_AFrozenSeriesLetsTheEpisodeEnd"},
		{"../saturation", "TestMuRepeat_OverDerivedDoesNotKeepTheMeasurementFresh"},
	},
	saturation.SwitchMuUnknownAgeSafe: {
		{"../saturation", "TestMuUnknownAge_UnderDerivedKeepsTheLowHold"},
		{"../saturation", "TestMuUnknownAge_NoTimeAtAllIsHeld"},
	},
	saturation.SwitchBatchCapDeclared: {
		{"../saturation", "TestBatchCapSwitch_DeclaredOffPricesTheDefault"},
	},
	saturation.SwitchBatchCapLearned: {
		{"../saturation", "TestBatchCapSwitch_LearnedOffLearnsNothing"},
		{"../saturation", "TestBatchCap_SwitchesOffReproduceTheIssue"},
	},
	saturation.SwitchMuLatchRestoresSafe: {
		{"../saturation", "TestMuLatch_ARestoredMeasurementOverDerivedComesBackLatched"},
		{"../saturation", "TestMuLatch_TheRestoredLatchIsKnownAfterItsFirstChoice"},
		{"../saturation", "TestMuLatch_AWindowRestoredAfterARefusedReadingComesBackLatched"},
		{"../saturation", "TestMuLatch_AWindowBroughtBackFromTheStashComesBackLatched"},
		{"../saturation", "TestMuLatch_AWindowRestoredAsAPriorAfterARefusedReadingComesBackLatched"},
	},
}

// Every switch the saturation hook accepts has a row: this PR's, or an
// earlier PR's seam still in the code. A seam with no row is a switch no
// run drives.
func TestGoldens_EverySwitchHasARow(t *testing.T) {
	for _, name := range saturation.ReplaySwitchNames() {
		_, mine := replaySwitches[name]
		_, earlier := earlierSeams[name]
		require.True(t, mine || earlier, "%s is accepted by SetSwitchesForReplay and is in neither replaySwitches nor earlierSeams", name)
	}
	for name := range earlierSeams {
		require.Contains(t, saturation.ReplaySwitchNames(), name, "%s's seam is gone: delete its row", name)
	}
}

// heldTest is a test function, and the package directory that declares it.
type heldTest struct{ dir, name string }

// A switch that names no golden output names the unit tests that hold it,
// and they exist.
func TestGoldens_ASwitchNoGoldenMovesIsHeldByItsTests(t *testing.T) {
	for name, outs := range mergedRows() {
		if len(outs) > 0 {
			continue
		}
		tests := heldBy[name]
		require.NotEmpty(t, tests, "%s moves no golden: name the unit tests that hold it in heldBy", name)
		for _, h := range tests {
			files, err := filepath.Glob(filepath.Join(h.dir, "*_test.go"))
			require.NoError(t, err)
			found := false
			for _, f := range files {
				b, err := os.ReadFile(f)
				require.NoError(t, err)
				if strings.Contains(string(b), "func "+h.name+"(t *testing.T)") {
					found = true
					break
				}
			}
			require.True(t, found, "%s is held by %s, which %s does not declare", name, h.name, h.dir)
		}
	}
}

// mergedRows is replaySwitches and earlierSeams together.
func mergedRows() map[string][]string {
	out := maps.Clone(replaySwitches)
	maps.Copy(out, earlierSeams)
	return out
}

// switchBase is the sha256 of each output a switch names, as the base
// commit's goldens hold it (2ea8f0aa1). No switch of this PR names one.
var switchBase = map[string]string{}

func TestGoldens_TheSwitchRuns(t *testing.T) {
	m, err := ReadManifest(goldenDir)
	require.NoError(t, err)
	named := map[string]bool{}
	for _, outs := range replaySwitches {
		for _, o := range outs {
			named[o] = true
			require.Contains(t, switchBase, o, "an output a switch names needs its base digest")
		}
	}
	// run replays every fixture with the switches set, and returns its
	// outputs by fixture/file.
	run := func(sw map[string]bool) map[string][]byte {
		out := map[string][]byte{}
		for _, g := range m.Goldens {
			gz, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(g.Recording)))
			require.NoError(t, err)
			rec, err := gunzip(gz)
			require.NoError(t, err)
			rep, err := Replay(context.Background(), rec, Options{Mode: Normal,
				Switches: map[string]map[string]bool{PackageSaturation: sw}})
			require.NoError(t, err)
			require.Empty(t, rep.Failed(), "%s: the replay failed", g.Name)
			outs, err := Outputs(rep.Recording)
			require.NoError(t, err)
			for _, f := range g.Outputs {
				out[f] = outs[path.Base(f)]
			}
		}
		return out
	}
	set := func(on ...string) map[string]bool {
		sw := map[string]bool{}
		for name := range replaySwitches {
			sw[name] = slices.Contains(on, name)
		}
		return sw
	}
	// moved is the outputs that differ between two runs, sorted, each
	// logged with the replay's one-line summary of how.
	moved := func(name string, from, to map[string][]byte) []string {
		var out []string
		for f, b := range to {
			d, err := compareOutput(f, from[f], b)
			require.NoError(t, err)
			if d != nil {
				out = append(out, f)
				t.Logf("%s: %s", name, d)
			}
		}
		slices.Sort(out)
		return out
	}

	off := run(set())
	for f, b := range off {
		if named[f] {
			sum := sha256.Sum256(b)
			require.Equal(t, switchBase[f], hex.EncodeToString(sum[:]),
				"all off: %s is not the base commit's golden", f)
			continue
		}
		committed, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(f)))
		require.NoError(t, err)
		d, err := compareOutput(f, committed, b)
		require.NoError(t, err)
		require.Nil(t, d, "all off: %s moved, and no switch names it", f)
	}
	for _, name := range slices.Sorted(maps.Keys(replaySwitches)) {
		got := moved(name, off, run(set(name)))
		// Exactly what it names: a named output that does not move is an off
		// path that went dead.
		require.ElementsMatch(t, replaySwitches[name], got, "%s alone must move exactly the outputs it names", name)
		t.Logf("%s alone moves %d outputs: %v", name, len(got), got)
	}
	on := run(set(slices.Collect(maps.Keys(replaySwitches))...))
	for f, b := range on {
		committed, err := os.ReadFile(filepath.Join(goldenDir, filepath.FromSlash(f)))
		require.NoError(t, err)
		d, err := compareOutput(f, committed, b)
		require.NoError(t, err)
		require.Nil(t, d, "all on: %s is not the committed golden", f)
	}
	for _, name := range slices.Sorted(maps.Keys(earlierSeams)) {
		sw := set(slices.Collect(maps.Keys(replaySwitches))...)
		sw[name] = false
		got := moved(name, on, run(sw))
		require.ElementsMatch(t, earlierSeams[name], got, "%s off alone must move exactly the outputs its PR named", name)
		t.Logf("%s off alone moves %d outputs: %v", name, len(got), got)
	}
}
