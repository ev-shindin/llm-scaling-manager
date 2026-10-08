package allocation

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// NamedAnalyzerResult pairs an analyzer's name with its result, the engine-owned
// capacity aggregates derived from that result, and mutable working counters for
// the optimizer's allocation loop.
// It is the type of ModelScalingRequest.CompositeSignal and is
// only used inside the engine→optimizer contract; it is not a general-purpose
// interfaces type.
//
// The split of ownership is the point: Result carries the analyzer's pure
// (D, P) signal, and every field below it is written by the engine's
// capacity-build step from that signal. Nothing an analyzer returns can
// contradict the supply or the scaling signals the optimizer acts on.
//
// Remaining and Spare are initialised from RequiredCapacity and SpareCapacity
// by the engine (model scope) and decremented in place by applyAllocation as
// the optimizer allocates replicas.
// For disaggregated (P/D) models, the optimizer calls initRoleState
// to populate RoleSpare and RoleReleasable per role and initialize
// picker-local demand.
// The original Result values are never mutated.
type NamedAnalyzerResult struct {
	Name      string
	Result    *domain.AnalyzerResult
	Score     float64            // per-analyzer weight from AnalyzerScoreConfig; used for fair-share priority
	Remaining float64            // mutable remaining required capacity; P-scope for disaggregated, model-scope otherwise
	Spare     float64            // mutable remaining spare capacity; model-scope (non-disaggregated only)
	RoleSpare map[string]float64 // per-role mutable spare; set by initRoleState; nil for non-disaggregated
	// RoleReleasable is the per-role capacity that may actually be handed back,
	// measured against a target utilisation INSIDE the hysteresis band rather
	// than against its floor. RoleSpare answers "may this role release at all";
	// this answers "how many replicas", and they are different questions:
	// dividing RoleSpare by one replica requires the fleet to be a whole
	// replica below scaleDownBoundary, which at nine replicas means a
	// utilisation of 0.622 against a boundary of 0.70. Run T sat at 0.643 and
	// could not shed a replica it did not need.
	//
	// Mutable and decremented alongside RoleSpare as replicas are committed, so
	// a role with several variants cannot release the same capacity twice.
	RoleReleasable    map[string]float64
	ScaleUpThreshold  float64 // resolved scale-up threshold used to compute RC
	ScaleDownBoundary float64 // resolved scale-down boundary used to compute SC

	// Model-level supply aggregates — written by the engine's capacity-build
	// step. They are derived from Result.VariantCapacities so the linearity
	// invariant (supply = Σ_v replicas × per-replica P) holds by construction:
	//   TotalSupply            = Σ_v ReplicaCount × PerReplicaCapacity
	//   TotalAnticipatedSupply = Σ_v (ReplicaCount + PendingReplicas - StuckReplicas) × PerReplicaCapacity
	//   Utilization            = Result.TotalDemand / TotalSupply (0 when TotalSupply == 0)
	// TotalAnticipatedSupply counts pending replicas so they offset demand,
	// preventing double-scaling.
	TotalSupply            float64
	TotalAnticipatedSupply float64
	Utilization            float64

	// Scaling signals — written by the engine's capacity-build step; read by the
	// optimizer:
	//   RC = max(0, Result.TotalDemand/scaleUp − TotalAnticipatedSupply)
	//   SC = max(0, TotalSupply                − Result.TotalDemand/scaleDown)
	RequiredCapacity float64 // >0 means scale-up needed
	SpareCapacity    float64 // >0 means scale-down possible

	// RoleCapacities holds per-role capacity aggregation for P/D disaggregated
	// models. nil when no disaggregation is active (all variants are role
	// "both"). Assembled by the engine's capacity-build step from
	// Result.RoleDemand + per-variant supply.
	RoleCapacities map[string]domain.RoleCapacity

	// Live indicates the analyzer produced a non-error, informative result within the
	// staleness window. Set by the engine each cycle. Non-live analyzers are excluded
	// from the scale-down veto so a registered-but-uninformative analyzer (no metrics,
	// error state, never analyzed) cannot block scale-down. Recovery is automatic: a
	// fresh informative result makes it live again on the next cycle.
	Live bool
}

// ModelScalingRequest bundles the analyzer result with variant state for one model.
// The optimizer receives a slice of these — one per model — and produces decisions.
type ModelScalingRequest struct {
	ModelID         string
	Namespace       string
	CompositeSignal NamedAnalyzerResult // the one composed signal the engine hands the optimizer, already reduced to a single entry
	VariantStates   []domain.VariantReplicaState
	// Variants is the authoritative per-variant metadata (identity, cost,
	// accelerator, replica state) from the discovery step. When populated it is
	// the source of truth for variant metadata, superseding the copies the
	// saturation analyzer laundered onto its VariantCapacity output. May be nil
	// on non-primary paths (e.g. tests), in which case the optimizer falls back
	// to the analyzer-supplied metadata.
	Variants      []domain.VariantMetadata
	Priority      float64 // Model priority (default 1.0)
	Disaggregated bool    // true when model has prefill+decode variants

	// WeightClass and Weight are the model's utilization-share weight as its
	// scaling-policy entry states it (at most one is set). They are resolved
	// against the weight classes in force by config.UtilizationShare.Weight.
	WeightClass string
	Weight      config.ModelWeight
}

// ScalingOptimizer makes final scaling decisions for all models.
//
// Implementations:
//   - CostAwareOptimizer: processes each model independently, minimizes cost (unlimited mode)
//   - GreedyByScoreOptimizer: fair-shares GPUs across models (limited mode)
type ScalingOptimizer interface {
	// Name returns optimizer identifier for logging/metrics.
	Name() string

	// Optimize produces VariantDecisions from analyzer results and optional constraints.
	// constraints may be nil in unlimited mode.
	Optimize(ctx context.Context, requests []ModelScalingRequest, constraints []*ResourceConstraints) []domain.VariantDecision
}
