package steadystate

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
)

// A role the swing rule plans on its mean need is named in the log when the
// rule takes it and when it lets it go, with both needs, and is not repeated
// while it holds (issue #269: a swap back sat at 2:2 for eleven minutes and
// no log line said why).
func TestUtilizationShareReportsSwingingRoles(t *testing.T) {
	var lines []string
	logger := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
	st := &utilizationShareState{}
	tm := allocation.ShareTimings{SwingWindow: 90 * time.Minute}
	const busy, idle = "ns-a/m/decode", "ns-b/m/decode"
	both := allocation.SharePlan{Swinging: []string{busy, idle}, SwingNeeds: map[string]allocation.ShareSwingNeed{
		busy: {Mean: 1.43, Current: 2.73}, idle: {Mean: 0.72, Current: 0.66}}}

	count := func(sub string) int {
		n := 0
		for _, l := range lines {
			if strings.Contains(l, sub) {
				n++
			}
		}
		return n
	}

	st.reportSwinging(logger, "|NVIDIA-H200", both, tm)
	if got := count("planned on its mean need"); got != 2 {
		t.Fatalf("entering the swing rule logged %d lines naming it, want 2: %q", got, lines)
	}
	if count(`"flappingWindow"="1h30m0s"`) != 2 {
		t.Errorf("the swing lines lack the flapping window: %q", lines)
	}
	for _, want := range []string{`"role"="ns-a/m/decode"`, `"meanNeedGPUs"="1.43"`, `"currentNeedGPUs"="2.73"`} {
		if count(want) != 1 {
			t.Errorf("the swing line lacks %s: %q", want, lines)
		}
	}

	lines = nil
	st.reportSwinging(logger, "|NVIDIA-H200", both, tm)
	if len(lines) != 0 {
		t.Fatalf("a swing that holds was logged again: %q", lines)
	}

	lines = nil
	st.reportSwinging(logger, "|NVIDIA-H200", allocation.SharePlan{Swinging: []string{idle},
		SwingNeeds: map[string]allocation.ShareSwingNeed{idle: {Mean: 0.72, Current: 0.66}}}, tm)
	if len(lines) != 1 || count("planned on its current need again") != 1 || count(busy) != 1 {
		t.Fatalf("the busy role leaving the swing rule logged %q, want one line naming it", lines)
	}

	lines = nil
	st.reportSwinging(logger, "|NVIDIA-H200", allocation.SharePlan{}, tm)
	if len(lines) != 1 || count(idle) != 1 {
		t.Fatalf("the last role leaving the swing rule logged %q, want one line naming it", lines)
	}
	if len(st.swinging) != 0 {
		t.Fatalf("a group with no swinging role kept state %v", st.swinging)
	}
}
