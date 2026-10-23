package saturation

import (
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
)

// Both start-time lines name the namespace. The estimate is keyed by it, and two
// namespaces running the same variant name logged lines that could not be told
// apart.
func TestReplicaStartLinesNameTheNamespace(t *testing.T) {
	const variant = "decode-v"
	const key = "team-a|model|" + variant
	replica := func(pod string, startSeconds float64) domain.ReplicaMetrics {
		rm := makeReplicaMetrics(pod, variant, 400_000, tracedKv, 10, 1000, 6000)
		rm.Ready = true
		rm.StartSeconds = startSeconds
		return rm
	}

	var lines []string
	logger := funcr.New(func(prefix, args string) { lines = append(lines, prefix+" "+args) }, funcr.Options{Verbosity: 10})
	a := NewSaturationAnalyzer(capacity.NewStore())
	a.noteReplicaStart(key, "team-a", variant, []domain.ReplicaMetrics{replica("d0", 80)}, logger)
	a.noteReplicaStart(key, "team-a", variant,
		[]domain.ReplicaMetrics{replica("d1", 80*maxStartOutlierFactor*2)}, logger)

	for _, msg := range []string{"replica-start-measured", "replica-start-outlier-rejected"} {
		found := false
		for _, l := range lines {
			if strings.Contains(l, `"msg"="`+msg+`"`) {
				found = true
				if !strings.Contains(l, `"namespace"="team-a"`) {
					t.Errorf("%s has no namespace: %s", msg, l)
				}
			}
		}
		if !found {
			t.Errorf("no %s line was logged; lines: %v", msg, lines)
		}
	}
}
