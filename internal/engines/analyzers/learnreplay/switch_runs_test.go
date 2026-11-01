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
// the outputs it names; one that deletes a seam deletes its row.

// replaySwitches is each saturation replay switch, and the golden outputs
// (fixture/file) it is named to move.
var replaySwitches = map[string][]string{
	saturation.SwitchK2ReadsSettle: nil,
	saturation.SwitchK2WaitsAfterRestart: {
		"mu-paths/learned-state.csv", "mu-paths/store-records.csv", "mu-paths/variant-capacities.csv",
	},
}

// switchBase is the sha256 of each output a switch names, as the base
// commit's goldens hold it (25617e954, learner PR 1).
var switchBase = map[string]string{
	"mu-paths/learned-state.csv":      "04745ee34d9d20cdbbc46956144b6fdec7bd53cb0a0b3d557e88af30fd90161e",
	"mu-paths/store-records.csv":      "9db7ac4cb7887fbeade8ef0ab5d1a04762fea4b8b6bce127c6494fb82a83e8a3",
	"mu-paths/variant-capacities.csv": "b763381d5eb0ee0a59e3ac9006365e0acb2f873c8a2014c0dde3666b25af5850",
}

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
	moved := func(from, to map[string][]byte) []string {
		var out []string
		for f, b := range to {
			d, err := compareOutput(f, from[f], b)
			require.NoError(t, err)
			if d != nil {
				out = append(out, f)
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
		got := moved(off, run(set(name)))
		for _, f := range got {
			require.Contains(t, replaySwitches[name], f, "%s alone moved %s, which it does not name", name, f)
		}
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
}
