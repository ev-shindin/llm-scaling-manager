package registration

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// The router's flow-control layer per InferencePool, recorded for the TTFT
// model and read by nothing yet (docs/proposals/analyzer-learner.md, 2.9;
// slo-capacity-planner-ttft.md, "The router queue"). The EPP publishes the
// series with its flowControl feature gate on, and with it off they are
// normally absent. Not always: wait buckets at rate 0 were recorded within a
// run's first 3 to 4 minutes on runs goldens.yaml describes as off, cause not
// known.
//
// The saturation gauge and the outcome counter carry the pool
// (inference_pool) and no model. The pool is joined to a model by the
// flow-control queue gauge, which carries both (QueryFlowControlPoolModels),
// and only where that gauge names one model for the pool: the guard the
// arrival rate takes on the same gauge (arrivalModelLabel). The wait
// histogram carries the model (model_name, target_model_name) as well as the
// pool, so it is read per model, with the router latency.
//
// All of them are issued only while a learner recording is on (collector
// SetRecordTTFTInputs).
//
// Only the current llm_d_epp_* names: the deprecated inference_extension_*
// alias carries no inference_pool label, so it cannot be split by pool.
const (
	// QueryFlowControlPoolModels names, per pool, the models its flow-control
	// queue gauge carries: one series per (inference_pool, model_name,
	// target_model_name), value ignored.
	QueryFlowControlPoolModels = "flow_control_pool_models"
	// QueryFlowControlWaitBuckets is the flow-control queue wait histogram
	// per model, as per-second bucket rates, of the requests dispatched
	// (outcome="Dispatched"): only a dispatched request's wait is part of a
	// TTFT. Summed over fairness_id, priority and pool.
	QueryFlowControlWaitBuckets = "flow_control_wait_buckets"
	// QueryFlowControlPoolSaturation is the pool's saturation score per stage
	// (prefill, decode, and effective, the larger, which gates dispatch),
	// averaged over the window, the highest over the router's replicas.
	QueryFlowControlPoolSaturation = "flow_control_pool_saturation"
	// QueryFlowControlOutcomes is the rate of requests leaving the queue per
	// pool and outcome.
	QueryFlowControlOutcomes = "flow_control_outcomes"

	// FlowControlWindow is the window of the three queries above: the router
	// latency's (SLORouterWindow), so the wait and the TTFT it is part of
	// cover the same interval.
	FlowControlWindow        = SLORouterWindow
	FlowControlWindowSeconds = SLORouterWindowSeconds
)

const (
	flowControlPoolModels = `max by (inference_pool, model_name, target_model_name) (` +
		`llm_d_epp_flow_control_queue_size{namespace="{{.namespace}}",inference_pool!=""})`
	// Rate inside the sum, as the router latency queries take it, so a
	// counter reset on one EPP replica cannot read as a burst.
	flowControlWaitBuckets = `sum by (model_name, target_model_name, le) (` +
		`rate(llm_d_epp_flow_control_request_queue_duration_seconds_bucket{namespace="{{.namespace}}",outcome="Dispatched"}[` + FlowControlWindow + `]))`
	// A max over the router's replicas: each scores the same endpoints, so
	// they agree up to scrape timing, and a sum would add their scores.
	flowControlPoolSaturation = `max by (inference_pool, stage) (` +
		`avg_over_time(llm_d_epp_flow_control_pool_saturation{namespace="{{.namespace}}"}[` + FlowControlWindow + `]))`
	flowControlOutcomes = `sum by (inference_pool, outcome) (` +
		`rate(llm_d_epp_flow_control_requests_total{namespace="{{.namespace}}"}[` + FlowControlWindow + `]))`
)

// RegisterFlowControlPoolQueries registers the flow-control queries. The
// engine collects them every cycle for every model while a learner recording
// is on, and not otherwise.
func RegisterFlowControlPoolQueries(sourceRegistry *source.SourceRegistry) {
	metricsSource := sourceRegistry.Get("prometheus")
	if metricsSource == nil {
		ctrl.Log.V(logging.DEBUG).Info("Prometheus source not registered, skipping flow-control pool query registration")
		return
	}
	registry := metricsSource.QueryList()
	for _, q := range []struct{ name, template, description string }{
		{QueryFlowControlPoolModels, flowControlPoolModels,
			"Models each InferencePool's flow-control queue serves, to join the per-pool series to a model"},
		{QueryFlowControlWaitBuckets, flowControlWaitBuckets,
			"Router (EPP) flow-control queue wait histogram bucket rates per model, dispatched requests"},
		{QueryFlowControlPoolSaturation, flowControlPoolSaturation,
			"Router (EPP) flow-control saturation per InferencePool and stage, averaged over the window"},
		{QueryFlowControlOutcomes, flowControlOutcomes,
			"Router (EPP) flow-control requests leaving the queue per second, per InferencePool and outcome"},
	} {
		registry.MustRegister(source.QueryTemplate{
			Name:        q.name,
			Type:        source.QueryTypePromQL,
			Template:    q.template,
			Params:      []string{source.ParamNamespace},
			Description: q.description,
		})
	}
}
