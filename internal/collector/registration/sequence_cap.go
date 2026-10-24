package registration

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// The queries that say whether a replica's batch is at its sequence cap and
// whether it preempted, so the ITL fit can take a capped replica's reading
// above its KV band (itl.Tracker; issue #287).
const (
	// QueryRunningRequests is the query name for the number of sequences in
	// the running batch per pod, summed over its engine ranks: the figure
	// compared with max_num_seqs. Instantaneous, like QueryKvUsageInstant,
	// whose k it is read beside. A max per rank comes first, so copies of a
	// rank's gauge count once (see QueryKvUsageInstant).
	// SGLang takes the max, as its queue query does: a duplicate series must
	// not double the count, and a count read low only keeps a reading out.
	// Source: vllm:num_requests_running, sglang:num_running_reqs (gauges)
	QueryRunningRequests = "running_requests"

	// QueryPreemptionRate is the query name for the sequences preempted per
	// second per pod over ITLWindow, summed over its engine ranks: the window
	// the ITL mean is taken over, so a zero says no token in that mean came
	// from a preempting engine.
	//
	// vLLM only, and so registered once under its bare name: SGLang publishes
	// no preemption counter, and on an SGLang fleet the query returns no
	// series, which leaves PreemptionRateKnown unset.
	// Source: vllm:num_preemptions_total (counter)
	QueryPreemptionRate = "preemption_rate"
)

// registerSequenceCapQueries registers QueryRunningRequests for both engines
// and QueryPreemptionRate for vLLM.
func registerSequenceCapQueries(registry *source.QueryList) {
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryRunningRequests,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (vllm:num_requests_running{namespace="{{.namespace}}"}))`,
		Params:      []string{source.ParamNamespace},
		Description: "Sequences in the running batch per pod, compared with max_num_seqs",
	})
	registerForEngine(registry, inferenceengine.EngineSGLang, source.QueryTemplate{
		Name:        QueryRunningRequests,
		Type:        source.QueryTypePromQL,
		Template:    `max by (model_name, instance, pod) (sglang:num_running_reqs{namespace="{{.namespace}}"})`,
		Params:      []string{source.ParamNamespace},
		Description: "Sequences in the running batch per pod, compared with max_running_requests (SGLang)",
	})
	registry.MustRegister(source.QueryTemplate{
		Name:        QueryPreemptionRate,
		Type:        source.QueryTypePromQL,
		Template:    `sum by (model_name, instance, pod) (max by (model_name, instance, pod, engine) (rate(vllm:num_preemptions_total{namespace="{{.namespace}}"}[` + ITLWindow + `])))`,
		Params:      []string{source.ParamNamespace},
		Description: "Sequences preempted per second per pod over the ITL window",
	})
}
