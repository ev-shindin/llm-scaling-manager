package collector

import (
	"context"
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
)

// extractSequenceCap reads the running-batch size and the preemption rate
// into the instances the model-scoped queries already found, and skips the
// rest, like the other supplementary series: a scrape-skewed row must not
// enter the fleet.
//
// The preemption rate is an agnostic query, which filterResultsToModel does
// not narrow, so its series are matched to the model here; one carrying no
// model_name is kept, as the instance key already ties it to a seen pod.
func (c *ReplicaMetricsCollector) extractSequenceCap(ctx context.Context, modelID, namespace string,
	results map[string]*source.MetricResult, podData map[string]*podMetricData) {
	each := func(query string, fn func(d *podMetricData, v float64, finite bool)) {
		result := results[query]
		if result == nil || result.HasError() {
			return
		}
		for _, value := range result.Values {
			if m, ok := value.Labels[seriesModelLabel]; ok && m != "" && m != modelID {
				continue
			}
			instanceKey, _, _ := c.buildInstanceKey(ctx, namespace, value.Labels)
			if instanceKey == "" || podData[instanceKey] == nil {
				continue
			}
			if !math.IsNaN(value.Value) && !math.IsInf(value.Value, 0) && value.Value >= 0 {
				// finite is false for a NaN or an infinity the source
				// turned into 0: the value is kept as before, but it is
				// not a reading.
				fn(podData[instanceKey], value.Value, value.Finite())
			}
		}
	}
	each(registration.QueryRunningRequests, func(d *podMetricData, v float64, finite bool) {
		d.runningRequests = int(math.Round(v))
		d.runningRequestsKnown = finite
	})
	each(registration.QueryPreemptionRate, func(d *podMetricData, v float64, finite bool) {
		d.preemptionRate = v
		d.preemptionRateOK = finite
	})
}
