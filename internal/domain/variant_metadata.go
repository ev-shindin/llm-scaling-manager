package domain

// DefaultVariantCost is the per-replica cost assumed when a variant declares no
// cost (the llm-d.ai/variant-cost annotation is absent or unparseable). It lives
// here, next to the variant metadata it belongs to, so every consumer
// (discovery, collector, optimizer) shares one definition.
const DefaultVariantCost = 10.0

// VariantMetadata is the authoritative per-variant identity and replica/GPU
// state for one optimization cycle. It carries everything the optimizer and the
// analyzers need to know about a variant that is *not* a measured signal:
// identity (VariantName, ModelID, Namespace, Role, AcceleratorName), economics
// (Cost), and current fleet state (replica counts, GPUsPerReplica, bounds).
//
// It is produced by the discovery step (internal/engines/discovery), the single
// source of this metadata — replacing the previous scheme in which the
// saturation analyzer laundered identity onto its per-variant capacity output
// and cost/accelerator were stamped onto every pod's ReplicaMetrics.
type VariantMetadata struct {
	VariantName string
	ModelID     string
	Namespace   string
	// Role is the P/D disaggregation role: "prefill", "decode", or "both".
	Role string
	// Cost is the per-replica cost used for cost-aware variant selection.
	Cost float64
	// StuckReplicas is how many of this variant's Pods are not Ready and not
	// starting either. See domain.VariantCapacity.StuckReplicas.
	StuckReplicas int
	// PendingAges is how long each STARTING replica has been alive, in seconds:
	// one entry per Pod that exists and is not Ready. The demand floor credits
	// each with the part of its drain window it will be Ready for, which a count
	// alone cannot say. Nil when the listing could not be read.
	PendingAges []float64
	// AcceleratorName is the GPU product the variant runs on, resolved from the
	// scale target's pod-template nodeSelector/nodeAffinity (identical for every
	// pod of the variant — a variant-level fact, not a per-pod one).
	AcceleratorName string
	// Engine is the inference engine the variant runs ("vllm", "sglang"),
	// detected from the scale target's pod template. It selects the per-engine
	// query body for engine-specific analyzers (mirrors the collector's
	// registerForEngine).
	Engine          string
	GPUsPerReplica  int
	CurrentReplicas int
	// DesiredReplicas is the last optimizer decision recorded on the VA status,
	// 0 if none yet.
	DesiredReplicas int
	ReadyReplicas   int
	PendingReplicas int
	// HeldReplicas counts the replicas holding GPUs: scheduled and not
	// finished, TERMINATING ones included -- a pod being deleted keeps its GPUs
	// through its drain and grace period, which CurrentReplicas
	// (status.replicas) stops counting the moment deletion starts. HeldKnown is
	// false when the pods could not be listed; consumers then fall back to
	// CurrentReplicas.
	HeldReplicas int
	HeldKnown    bool
	// FilledReplicas counts the replicas every pod of which holds GPUs: the
	// same as HeldReplicas for a Deployment; for a LeaderWorkerSet, the groups
	// whose every pod is scheduled. A receiver's fill is judged by it -- a
	// group whose leader alone is bound serves nothing -- where a donor's
	// release is judged by HeldReplicas, the opposite safe direction.
	FilledReplicas int
	// PodGPUs is the GPUs each pod of one replica requests, leader first: one
	// entry for a Deployment, the group size for a LeaderWorkerSet. Nil when
	// no pod requests a GPU explicitly, where GPUsPerReplica's default of 1
	// says nothing about shape.
	PodGPUs []int
	// MinReplicas/MaxReplicas are the scaling bounds; nil means unset.
	MinReplicas *int
	MaxReplicas *int
}

// ToReplicaState projects the metadata onto the VariantReplicaState the analyzers
// consume today. Identity/economics fields that VariantReplicaState has no home
// for (ModelID, Namespace, Cost, AcceleratorName, ReadyReplicas) are dropped
// here; consumers that need them read the VariantMetadata directly.
//
// This projection keeps BuildVariantStates behavior-identical while discovery
// becomes the single producer — the transitional step before analyzers and the
// optimizer switch to reading VariantMetadata.
func (m VariantMetadata) ToReplicaState() VariantReplicaState {
	return VariantReplicaState{
		VariantName:     m.VariantName,
		CurrentReplicas: m.CurrentReplicas,
		DesiredReplicas: m.DesiredReplicas,
		PendingReplicas: m.PendingReplicas,
		PendingAges:     m.PendingAges,
		StuckReplicas:   m.StuckReplicas,
		HeldReplicas:    m.HeldReplicas,
		HeldKnown:       m.HeldKnown,
		FilledReplicas:  m.FilledReplicas,
		PodGPUs:         m.PodGPUs,
		GPUsPerReplica:  m.GPUsPerReplica,
		Role:            m.Role,
		AcceleratorName: m.AcceleratorName,
		Engine:          m.Engine,
		MinReplicas:     m.MinReplicas,
		MaxReplicas:     m.MaxReplicas,
	}
}
