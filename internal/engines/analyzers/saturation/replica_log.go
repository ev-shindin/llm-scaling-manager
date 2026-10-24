package saturation

import (
	"sort"
	"strconv"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// perReplicaLogLevel is the V() level of the lines written once per replica
// per cycle: replica-capacity-decision, k2-decision and derived-mu. At the
// shipped verbosity (logging.DEFAULT) they were most of the controller's log,
// about 1.9 KB per replica per cycle, and they grow with the fleet. One level
// above it they stay a --v=3 away for a benchmark or a diagnosis, and
// replica-capacity-summary says what each replica was priced at, at the
// default, in a few dozen bytes a replica.
const perReplicaLogLevel = logging.VERBOSE

// logReplicaCapacitySummary writes one replica-capacity-summary line per
// variant at the shipped verbosity: which replicas were priced this cycle and
// what each was priced at, as "<pod>=<capacity>/<bound>/<k2 source>", with
// "/bridge" on a warm-pool Pod lent to the variant. The bound is k1 when the
// KV cache binds and k2 when compute does, as replica-capacity-decision's
// boundBy says at perReplicaLogLevel.
func logReplicaCapacitySummary(logger logr.Logger, modelID, namespace string, caps []capacity.ReplicaCapacity) {
	if len(caps) == 0 {
		return
	}
	byVariant := make(map[string][]string)
	for _, rc := range caps {
		byVariant[rc.VariantName] = append(byVariant[rc.VariantName], replicaSummary(rc))
	}
	variants := make([]string, 0, len(byVariant))
	for v := range byVariant {
		variants = append(variants, v)
	}
	sort.Strings(variants)
	for _, v := range variants {
		replicas := byVariant[v]
		sort.Strings(replicas)
		logger.V(logging.DEFAULT).Info("replica-capacity-summary",
			"modelID", modelID, "namespace", namespace, "variant", v,
			"priced", len(replicas), "replicas", replicas)
	}
}

// replicaSummary is one replica's entry in replica-capacity-summary.
func replicaSummary(rc capacity.ReplicaCapacity) string {
	bound := "k1"
	if rc.ComputeBoundCapacity < rc.MemoryBoundCapacity {
		bound = "k2"
	}
	s := rc.PodName + "=" + strconv.FormatInt(rc.EffectiveCapacity, 10) + "/" + bound + "/" + rc.K2Priority.String()
	if rc.FromWarmPool {
		s += "/bridge"
	}
	return s
}
