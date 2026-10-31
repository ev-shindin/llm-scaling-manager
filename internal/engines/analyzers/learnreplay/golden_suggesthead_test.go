package learnreplay

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// A run that built slo and never analyzed it can still be cut to a head:
// the head need not hold an slo Analyze the whole recording does not have.
// On the parent commit suggestHead asked for one, so it suggested no head
// at all, and a recording of that size could not be added.
func TestSuggestHead_ABuiltAnalyzerThatNeverAnalyzedNeedsNoAnalyze(t *testing.T) {
	rec := probeRecording(t, "ns", "mdl", []string{"v"}, nil, nil)
	withSLO := bytes.Replace(rec, []byte(`"SLO":null`), []byte(`"SLO":{"StateTimeout":3600000000000,"SlopeHoldFor":3600000000000,"Judging":false}`), 1)
	require.NotEqual(t, rec, withSLO, "precondition: the start builds slo")
	require.NotContains(t, string(withSLO), `"Kind":"slo.Analyze"`, "precondition: and it never analyzed")
	require.Positive(t, suggestHead(rec, len(rec), 2*len(rec)), "precondition: the same recording without slo has a head")
	// A budget of twice the recording: every line fits, so only the Analyze
	// rule can make the suggestion zero.
	require.Positive(t, suggestHead(withSLO, len(withSLO), 2*len(withSLO)))
}
