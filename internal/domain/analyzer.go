package domain

import (
	"context"
	"time"
)

// SLOAnalyzerName is the name the SLO analyzer registers under and the name a
// scaling policy lists in its analyzers: section to run it. Reserved: an
// external analyzer of the same name is never run.
const SLOAnalyzerName = "slo"

// Analyzer is the common interface for all scaling analyzers.
// Each analyzer observes workload metrics and produces capacity signals
// (required_capacity, spare_capacity) that the engine combines into
// scaling decisions. Analyzers do NOT build scaling plans — the engine does.
//
// Saturation Analyzer V2 is the first implementation of this interface.
// Future analyzers (throughput, SLO) will implement the same interface.
type Analyzer interface {
	// Name returns the analyzer's identifier (e.g., "saturation", "throughput", "slo").
	Name() string

	// Analyze computes capacity signals for a model across all its variants.
	// Returns per-variant capacity breakdown and model-level scaling signals.
	Analyze(ctx context.Context, input AnalyzerInput) (*AnalyzerResult, error)
}

// The two Reason values that mean "this VariantCapacity carries no usable
// figure". They are named here, at the bottom of the dependency order, so an
// analyzer can emit them and the optimizer can test for them without either
// importing the other: the analyzers used to reach into the optimizer package
// for these two strings, the one upward import in the pipeline.
const (
	// ReasonNoData marks a variant for which the analyzer had no usable
	// measurement this cycle -- no metrics, no rows -- as opposed to a
	// measurement of zero.
	ReasonNoData = "no-data"
	// ReasonError marks a variant whose figure the analyzer could not produce
	// because something failed, as opposed to being absent.
	ReasonError = "error"
)

// AnalyzerConfig is the interface for analyzer-specific configuration.
// Each analyzer defines its own config type that implements this interface.
type AnalyzerConfig interface {
	// GetAnalyzerName returns the name of the analyzer this config is for.
	GetAnalyzerName() string
}

// AnalyzerInput is the common input provided to all analyzers. The engine
// builds one per model per cycle, complete before any analyzer runs, and
// every analyzer of the model receives the same one.
type AnalyzerInput struct {
	ModelID        string
	Namespace      string
	ReplicaMetrics []ReplicaMetrics
	VariantStates  []VariantReplicaState
	// VariantMetadata is discovery's identity and fleet state for each
	// variant of the model this cycle: role, accelerator, cost, GPUs per
	// replica, and the scale target's replica counts. Nil on paths that run
	// no discovery. No analyzer reads it yet: the engine joins the role and
	// clamps the replica count onto each result after Analyze
	// (steadystate.buildCapacities).
	VariantMetadata []VariantMetadata
	Config          AnalyzerConfig

	// SchedulerQueue holds model-level queue metrics from the llm-d inference
	// scheduler flow control layer. These represent requests queued upstream
	// of any pod and are not yet attributed to a specific variant or role.
	// Any analyzer with a demand model may convert this into per-analyzer
	// demand using its own unit (e.g., kv-tokens for saturation,
	// tokens/sec for a future throughput analyzer). Demand attribution to
	// roles or variants is each analyzer's choice.
	// Nil when flow control is disabled or metrics are unavailable.
	SchedulerQueue *SchedulerQueueMetrics

	// ArrivalRate is the model-level request arrival rate (requests/sec) from the
	// llm-d inference scheduler, summed across the whole model with no per-pod
	// labels to reconcile. Zero when the metric is unavailable (EPP absent or no
	// traffic yet). Any analyzer with a demand model may convert this into its
	// own unit (e.g. tokens/sec for the throughput analyzer).
	ArrivalRate float64
}

// SchedulerQueueMetrics holds model-level queue metrics from the llm-d
// inference scheduler flow control layer (inference_extension_flow_control_*).
// These are model-scoped, not per-pod, since the scheduler queues requests
// before routing them to a specific backend pod.
//
// They are scoped to one namespace as well as one model: the EPP exports no
// namespace itself (gateway-api-inference-extension #2309), and the collector
// selects by the namespace Prometheus stamps on the scraped series, so two
// namespaces serving the same model name each read their own router's queue.
type SchedulerQueueMetrics struct {
	// QueueSize is the number of requests currently queued in the
	// scheduler's flow control layer for this model.
	// Sourced from inference_extension_flow_control_queue_size.
	QueueSize int64

	// QueueBytes is the total bytes of request bodies currently queued
	// in the scheduler's flow control layer for this model.
	// Sourced from inference_extension_flow_control_queue_bytes.
	// Approximate token count: QueueBytes / BytesPerToken.
	QueueBytes int64
}

// RoleCapacity holds per-role capacity aggregation for P/D disaggregated models.
// Analyzers cannot populate this struct — it does not appear on AnalyzerResult.
// They emit per-role demand as AnalyzerResult.RoleDemand, and the engine's
// capacity-build step pairs that with the per-role supply it derives from the
// variant capacities, writing the result to the engine-owned
// NamedAnalyzerResult.RoleCapacities. Supply is assembled by buildRoleCapacities
// and RequiredCapacity/SpareCapacity by the universal threshold post-step, so a
// zero is a literal value here, never a sentinel.
type RoleCapacity struct {
	Role                   string
	TotalSupply            float64
	TotalDemand            float64
	TotalAnticipatedSupply float64
	RequiredCapacity       float64
	SpareCapacity          float64
}

// AnalyzerResult is the common output produced by all analyzers: the pure
// (D, P) signal and nothing else. Demand is the analyzer's — it owns demand
// attribution, at the model level (TotalDemand) and per role (RoleDemand).
// Per-replica capacity P is the analyzer's too, on each VariantCapacity.
//
// Everything derived from those two — model and per-role supply, utilization,
// and the RequiredCapacity/SpareCapacity scaling signals — belongs to the
// engine's capacity-build step and lives on pipeline.NamedAnalyzerResult, not
// here. An analyzer therefore cannot write a supply or a scaling signal that
// contradicts its own (D, P): the linearity invariant
// (supply = Σ_v replicas × per-replica P) holds by construction.
type AnalyzerResult struct {
	// AnalyzerName identifies which analyzer produced this result.
	AnalyzerName string

	ModelID    string
	Namespace  string
	AnalyzedAt time.Time

	// Per-variant capacity breakdown (in analyzer-specific units).
	VariantCapacities []VariantCapacity

	// TotalDemand is the model-level demand D (in analyzer-specific units).
	// Analyzer-supplied: the analyzer owns demand attribution.
	TotalDemand float64

	// RoleDemand is the analyzer's per-role demand D for P/D disaggregated models
	// (nil when non-disaggregated). It is the demand half of the contract — the
	// analyzer owns demand attribution (e.g. saturation charges waiting per role,
	// throughput charges arrival+queue per role); the capacity builder pairs it
	// with per-role supply to assemble NamedAnalyzerResult.RoleCapacities.
	RoleDemand map[string]float64

	// HeldRoleDemand is, for each role whose demand a hold changed this cycle,
	// its demand before the hold (nil when no hold changed one). A hold is the
	// analyzer withholding a decision -- a release on figures a shape change
	// made stale, or prefill sized on decode's backlog -- not a reading of
	// load: RoleDemand carries the held figure, which keeps the fleet where it
	// is. A consumer that would treat demand as what a role needs, rather than
	// as what to keep, reads the lower of the two.
	HeldRoleDemand map[string]float64

	// HotRankDemand is, for each role (RoleBoth on a model that is not
	// disaggregated) with a replica whose busiest data-parallel engine rank is
	// full, the demand the analyzer added to keep that replica's capacity
	// (nil when none was added). RoleDemand and TotalDemand carry it. It is
	// not a hold of the role, and a consumer must not read it as one: the
	// role's other replicas can still be released on their own spare, which a
	// role in HeldRoleDemand cannot.
	HotRankDemand map[string]float64

	// RoleStartSeconds is, for each role with a measured variant, how long a
	// new replica of its slowest measured variant took to become Ready, in
	// seconds (nil or absent when none is measured). The slowest, not the
	// fastest the throughput floor projects on: a consumer pacing work that
	// is not done until the replica serves must not count on the fastest.
	RoleStartSeconds map[string]float64

	// VariantBusiestQueueLoad is, for each decode or aggregated variant with
	// a Ready replica reading, the deepest waiting queue on any one of its
	// pods as a fraction of the queue at which a replica counts as saturated
	// (queueLengthThreshold). The fleet's load can read calm while one pod
	// holds a deep queue, and a consumer about to give a replica back reads
	// this as well as the fleet. Keyed by variant, not role, so a consumer
	// that groups variants by accelerator reads only its own. Prefill is
	// left out: its queue is decode's back-pressure or the router's choice,
	// which a replica kept does not relieve; and a P/D decode pod counts
	// only while its KV is full, since requests waiting for their remote KV
	// sit in the same count.
	VariantBusiestQueueLoad map[string]float64

	// PricingK is the KV utilization a decode replica's ITL line is evaluated
	// at to price its service rate: kvCacheThreshold x scaleUpThreshold,
	// clamped to the range a line is fitted over. Composed, so 0.68 with the
	// shipped defaults rather than the 0.85 k_sat suggests -- the reason it is
	// published (wva_pricing_k). 0 for an analyzer that does not price on a
	// line.
	PricingK float64
}

// LearnedStateBorrow is one figure a variant was priced from that a sibling
// variant learned.
type LearnedStateBorrow struct {
	// Quantity is what was borrowed: BorrowedK2 or BorrowedITLLine.
	Quantity string
	// Donor is the variant that measured it, and DonorNamespace its
	// namespace. A k2 is borrowed only within a namespace; an ITL line is
	// shared by engine configuration, whatever namespace the lender runs in,
	// so the variant name alone can be ambiguous -- or read as the variant
	// borrowing from itself.
	Donor          string
	DonorNamespace string
	// Relaxed is how far a k2 borrow has relaxed back to the variant's own
	// figure: 0 at full strength, 1 fully relaxed. Always 0 for a line,
	// which is used as it is.
	Relaxed float64
}

// The quantities a variant can borrow from a sibling.
const (
	BorrowedK2      = "k2"
	BorrowedITLLine = "itl-line"
)

// VariantCapacity is one variant's entry in an analyzer's (D, P) signal, in
// analyzer-specific units. For saturation: tokens. For throughput: tokens/sec.
// For SLO: latency-constrained capacity.
//
// It carries no variant identity beyond the name that keys it. Cost and
// accelerator are discovery's, and the optimizer reads them from
// VariantMetadata; an analyzer has no business restating them, and when it did,
// the values reached the optimizer only because the capacity builder overlaid
// the authoritative ones back on top.
//
// Role is the exception, and it stays because it is not identity here but the
// key the analyzer attributed demand by: the capacity builder pairs RoleDemand
// with the per-role supply grouped from these entries, so both halves must be
// keyed the same way. The builder still overlays it from discovery so the two
// cannot drift.
type VariantCapacity struct {
	VariantName string
	Role        string // "prefill", "decode", "both", "" (empty = non-disaggregated)

	// StuckReplicas is how many of this variant's Pods are not Ready and are not
	// starting either: terminal, backing off, or unschedulable on quota. They are
	// subtracted from PendingReplicas to get the replicas actually on their way,
	// because anticipated supply is what the engine withholds a scale-up for --
	// and a Pod that will never be Ready withholds it for ever.
	//
	// Only ever what the Pod listing could PROVE. An unreadable, stale or skipped
	// listing reports zero, which leaves anticipated supply exactly as it was.
	// PendingReplicas is deliberately everything the scale target owns that did
	// not report -- including replicas that turned Ready between the scrape and
	// this cycle -- and subtracting a count derived any other way would drop that
	// term and order a replica that already exists.
	StuckReplicas int

	// PendingAges is how long each starting replica of this variant has been
	// alive, in seconds -- one entry per Pod that exists and is not Ready. The
	// demand floor credits each with the part of its drain window it will be
	// Ready for, which PendingReplicas alone cannot say. Nil when the Pod
	// listing could not be read, which leaves the floor on a count-based
	// estimate of the same credit.
	PendingAges []float64

	// ReplicaCount and PendingReplicas are in SCALE-TARGET units (pods, or LWS
	// groups) — the same units as VariantMetadata.CurrentReplicas and as the
	// replica targets the optimizer produces. A pod running data parallelism
	// hosts several engine instances, but the collector merges their metrics
	// into one replica before an analyzer sees them, so there is one unit here
	// and no DP factor for a consumer to reconcile.
	//
	// ReplicaCount is analyzer-measured but not analyzer-final: like Role above,
	// the capacity-build step overlays it from discovery, capping it at
	// VariantMetadata.CurrentReplicas so supply cannot describe a larger fleet
	// than the scale target is committed to. The cap only ever lowers it. See
	// steadystate.clampReplicaCountToScaleTarget for why.
	ReplicaCount    int
	PendingReplicas int

	// ObservedReplicas is how many of this variant's own replicas REPORTED
	// METRICS this cycle and were attributed to it -- the live row count, never
	// scale-target status, and 0 when nothing reported or the analyzer does not
	// read per-replica rows. Unlike ReplicaCount it is not capped at the scale
	// target's count.
	//
	// It differs from the target's ready count in both directions, and both are
	// the point: above it, replicas are serving that the target does not own (a
	// conceded replica, a pod the ReplicaSet has not adopted); below it, a Pod's
	// metrics were missing from the cycle. Neither is recoverable downstream,
	// because ReplicaCount is either capped or filled in from status. Diagnostics
	// only: nothing sizes capacity from this, and it is published as
	// wva_analyzer_observed_replicas so "the analyzer saw the whole fleet" is
	// answerable without inferring it from demand, which is not proportional to
	// replicas.
	ObservedReplicas int

	// PerReplicaCapacity is the representative capacity per replica, in the same
	// scale-target units as ReplicaCount — so demand / PerReplicaCapacity yields
	// a replica target directly.
	// For saturation V2: median(effectiveCapacity) in tokens across ready replicas.
	//
	// Measured over the variant's OWN replicas only — never over a bridge. The
	// linearity invariant above is supply = replicas × P, and ReplicaCount counts
	// own replicas (the build step clamps it to the scale target, which a warm
	// pool Pod is not part of). A P blended over bridges would price that count at
	// a number none of those replicas delivers: the pool runs its engines at a
	// lower --gpu-memory-utilization than the workload, so less of the GPU is KV
	// cache and a bridge's memory-bound capacity is genuinely smaller (measured on
	// the shared GPU cluster, 2026-08-31: pool 0.90, workload 0.95). The error grows as the median
	// gets shorter — with one own replica and one bridge the median is their
	// average. See WarmPoolPerReplicaCapacity for the bridge's own reading.
	PerReplicaCapacity float64

	// Reason is a free-text string set by the analyzer to describe how the
	// variant's per-replica capacity was computed. Empty for analyzers that
	// do not set it. Saturation V2 uses "P0-store", "P1-obs", "P1-obs-unadmitted"
	// (a warm-pool bridge priced for the cycle at its own lower reading, not
	// recorded), "P2-hist", "P2-prior" (the variant's own figure from engine
	// arguments it no longer runs), "P2-borrow", "P3-k2", "P4-k1", "no-data",
	// "error". Throughput uses "T1-ols", "T2-pinned", "T2-default", "T2-failed".
	Reason string

	// Borrowed lists what this variant was priced from that a sibling
	// variant learned, this cycle: a k2 below its own capacity (P2-borrow) and
	// a shared ITL line. Diagnostics only -- published as
	// wva_learned_state_borrowed -- so that a variant priced from someone
	// else's measurement is visible without reading the cycle log. Nil when
	// it borrowed nothing.
	Borrowed []LearnedStateBorrow

	// K2Source is the k2 source (P1-obs, P2-hist, P2-borrow, P3-k2, P4-k1) of
	// the replica the variant's per-replica capacity was taken from -- the
	// lower-median replica, the one Reason names -- or k1-bound when that
	// replica's k2 is at or above its k1, so the KV cache prices it and the
	// k2 has no age worth reporting. K2Age is how long ago the window that figure came
	// from was last WRITTEN, when HasK2Age: the variant's own window for
	// P1-obs and P2-hist, the donor's for P2-borrow; P3-k2 and P4-k1 come
	// from no window. A read does not make a figure younger. K2Source is
	// empty when no replica reported this cycle, or the analyzer learns no
	// k2. Diagnostics only -- published as wva_learned_state_k2_source and
	// wva_learned_state_k2_age_seconds -- so that a fleet held up by a
	// figure measured a week ago is visible without reading the cycle log.
	K2Source string
	K2Age    time.Duration
	HasK2Age bool

	// WarmPoolReplicas is how many BRIDGES are serving this variant: warm pool
	// Pods lent to it while it is short, counted in the same scale-target units
	// as ReplicaCount but deliberately NOT part of it.
	//
	// Their load is in TotalDemand, because it is this variant's load. Their
	// capacity is not in supply, because the Pods are borrowed: counting them
	// would tell the optimizer the fleet is already big enough and suppress the
	// scale-up the bridge exists to cover, leaving the pool holding the Pod
	// because the replicas that would release it were never created.
	WarmPoolReplicas int

	// WarmPoolPerReplicaCapacity is P for a BRIDGE: the representative capacity of
	// one lent warm pool Pod, measured from the bridges themselves and in the same
	// units as PerReplicaCapacity.
	//
	// A separate reading rather than a reuse of PerReplicaCapacity because the two
	// are genuinely different numbers -- see PerReplicaCapacity. Zero when no
	// bridge is serving this variant.
	//
	// Analyzer-supplied: it is a measurement, so it is part of the (D, P) signal.
	WarmPoolPerReplicaCapacity float64

	// WarmPoolCapacity is what those bridges are worth, in the same units as
	// supply. Carried for the retained-pool switching decision -- where there are
	// no ordinary replicas coming and the pool IS the capacity, so choosing which
	// model holds the GPUs needs to compare what each is getting from the pool
	// against what each is asking for.
	//
	// BUILDER-OWNED, not analyzer-supplied: it is WarmPoolReplicas ×
	// WarmPoolPerReplicaCapacity, and a derived capacity belongs to the
	// capacity-build step for the same reason supply does. An analyzer that sets
	// it is overwritten. See steadystate.buildCapacities.
	WarmPoolCapacity float64

	// TotalDemand is the aggregate demand on this variant. It INCLUDES the
	// demand carried by any bridge above.
	TotalDemand float64

	// Utilization is TotalDemand / (ReplicaCount × PerReplicaCapacity), 0.0-1.0.
	Utilization float64

	// ServiceRate is the ITL line and per-replica service rate the analyzer
	// priced this variant on this cycle, published as wva_itl_line_* and
	// wva_service_rate_per_replica. Nil for an analyzer that does not price a
	// service rate. Diagnostic only: nothing downstream sizes from it.
	ServiceRate *ServiceRateSignal
}
