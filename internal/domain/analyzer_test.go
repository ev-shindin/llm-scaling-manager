package domain

import "testing"

// Only the scheduler queue says the gate is on: a pool or a wait histogram is
// filled only while a recording is on, so neither counts.
func TestAnalyzerInput_FlowControlSeries(t *testing.T) {
	buckets := []LatencyBucket{{UpperBound: 1, Rate: 1}}
	for _, c := range []struct {
		name string
		in   AnalyzerInput
		want bool
	}{
		{"nothing", AnalyzerInput{}, false},
		{"router latency alone", AnalyzerInput{RouterLatency: &RouterLatency{}}, false},
		{"a pool alone", AnalyzerInput{FlowControlPools: []FlowControlPool{{}}}, false},
		{"a wait histogram alone", AnalyzerInput{RouterLatency: &RouterLatency{FlowControlWaitBuckets: buckets}}, false},
		{"an empty queue", AnalyzerInput{SchedulerQueue: &SchedulerQueueMetrics{}}, true},
	} {
		if got := c.in.FlowControlSeries(); got != c.want {
			t.Errorf("%s: FlowControlSeries() = %v, want %v", c.name, got, c.want)
		}
	}
}
