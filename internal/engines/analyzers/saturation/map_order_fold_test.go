package saturation

import (
	"math"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// A float sum depends on the order it is taken in: 1e16 + 1 + 1 is 1e16 (each
// 1 is below half an ulp of 1e16 and is lost), while 1 + 1 + 1e16 is 1e16 + 2.
// A fold over a map in Go's random order therefore gives a different last bit
// from one run to the next -- which a replay of a recorded cycle reads as a
// mismatch. These folds must give one answer however the map was built.
var _ = Describe("float folds over a map", func() {
	// foldRuns is enough runs that a fold taken in map order sees more than
	// one order: three keys have six, and each run builds a fresh map.
	const foldRuns = 1000
	keys := []string{"a", "b", "c"}
	figures := map[string]float64{"a": 1e16, "b": 1, "c": 1}

	// build makes a fresh map of figures, inserting the keys rotated by i.
	build := func(i int) map[string]float64 {
		out := make(map[string]float64, len(keys))
		for j := range keys {
			k := keys[(i+j)%len(keys)]
			out[k] = figures[k]
		}
		return out
	}

	It("meanOf gives one bit-identical mean however the map was built", func() {
		want := math.Float64bits(meanOf(build(0)))
		for i := range foldRuns {
			Expect(math.Float64bits(meanOf(build(i)))).To(Equal(want),
				"run %d: the mean of the same readings moved with the map's order", i)
		}
	})

	It("holdFleetFloor moves the total by one bit-identical figure", func() {
		vcs := make([]domain.VariantCapacity, 0, len(keys))
		for _, k := range keys {
			vcs = append(vcs, domain.VariantCapacity{
				VariantName: "v-" + k, Role: k, ReplicaCount: 1, PerReplicaCapacity: figures[k],
			})
		}
		// Every role at zero demand, floored to its whole supply (scaleDown
		// 1), so moved is the sum of the three supplies.
		zero := func(i int) map[string]float64 {
			out := build(i)
			for k := range out {
				out[k] = 0
			}
			return out
		}
		first, _ := holdFleetFloor(zero(0), vcs, 1)
		Expect(first).To(BeNumerically(">=", 1e16), "the premise: every role is raised")
		want := math.Float64bits(first)
		for i := range foldRuns {
			moved, _ := holdFleetFloor(zero(i), vcs, 1)
			Expect(math.Float64bits(moved)).To(Equal(want),
				"run %d: the total moved with the map's order", i)
		}
	})
})
