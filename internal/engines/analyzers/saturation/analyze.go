package saturation

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/itl"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/shape"
)

// One cycle, end to end. What each stage computes is in the file named after
// it; what each stage of THIS file computes is in its own name.
//
// See docs/proposals/analyzer-structure.md. The stages were already here as
// 155 consecutive lines; naming them changed no arithmetic and no ordering.

// Analyze computes capacity signals for a model across all its variants.
//
// The stages run in this order and the order matters: the shape has to be
// observed before mu can be priced at it, replicas have to be priced before
// the fleet can be said to have measured itself, and the floor and the holds
// come last because they only ever raise a demand the stages above computed.
func (a *SaturationAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	satConfig, ok := input.Config.(*config.ScalingPolicy)
	if !ok {
		return nil, fmt.Errorf("expected *ScalingPolicy, got %T", input.Config)
	}

	c := a.newCycle(ctx, input, satConfig)
	c.observeFleetShape()
	c.resolvePricing()
	c.fitLines()

	caps, err := c.priceReplicas(ctx)
	if err != nil {
		return nil, err
	}
	c.settleShape(caps)

	d := c.priceDemand(caps)
	total := c.applyFloorAndHolds(caps, d)

	return &domain.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: d.variants,
		TotalDemand:       total,
		RoleDemand:        d.roleDemand,
	}, nil
}

// cycle is the state of one Analyze call, carried between its stages.
//
// A struct rather than a dozen parameters: the stages genuinely share this
// much, and threading it positionally is how an argument ends up in the wrong
// slot. Every field is written by exactly one stage and read by later ones,
// which is the property that makes the order above checkable.
type cycle struct {
	a      *SaturationAnalyzer
	input  domain.AnalyzerInput
	cfg    *config.ScalingPolicy
	logger logr.Logger

	// newCycle: variant-level identity, and whether decode is saturated.
	gpusByVariant   map[string]int
	rolesByVariant  map[string]string
	accelByVariant  map[string]string
	decodeSaturated bool

	// observeFleetShape: the one shape the fleet is serving, and the tracker's
	// view of whether it just changed.
	fleetInput   float64
	fleetOutput  float64
	fleetHitRate float64
	stableInput  float64
	stableOutput float64
	shapeChanged bool

	// resolvePricing: the shape mu is priced at, which is not always the one
	// above.
	muDivisor     float64
	muInput       float64
	muShortWindow bool

	// fitLines: one ITL(k) line per decode variant.
	itlModels map[string]itl.Model
}

// demandParts is what priceDemand produces and applyFloorAndHolds consumes.
// queueByRole is carried separately from roleDemand because the holds treat
// the scheduler queue's share differently from demand a pod already holds.
type demandParts struct {
	variants    []domain.VariantCapacity
	roleDemand  map[string]float64
	queueByRole map[string]float64
	total       float64
}

// newCycle builds the variant lookups and settles the one question that must
// be answered before any replica is priced.
func (a *SaturationAnalyzer) newCycle(
	ctx context.Context, input domain.AnalyzerInput, cfg *config.ScalingPolicy,
) *cycle {
	c := &cycle{
		a:      a,
		input:  input,
		cfg:    cfg,
		logger: ctrl.LoggerFrom(ctx),
	}

	// Build GPU count and P/D role lookups from variant states. The role decides
	// how a waiting request is charged against a replica's KV capacity, so it must
	// be known before per-replica demand is computed.
	// Accelerator joins these two: it is variant-level identity from discovery, not
	// a per-instance measurement, so it is read from the variant state rather than
	// repeated on every ReplicaMetrics record.
	c.gpusByVariant = make(map[string]int, len(input.VariantStates))
	c.rolesByVariant = make(map[string]string, len(input.VariantStates))
	c.accelByVariant = make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		c.gpusByVariant[vs.VariantName] = vs.GPUsPerReplica
		c.rolesByVariant[vs.VariantName] = vs.Role
		c.accelByVariant[vs.VariantName] = vs.AcceleratorName
	}

	// Whether the decode role is saturated this cycle decides what a
	// saturated PREFILL replica is evidence of (computeK2): a prefill request
	// completes only when decode admits it, so while decode is full and
	// queued, prefill's queue and its held KV are decode's backlog seen from
	// upstream, and neither its occupancy nor its completion rate is a
	// reading of prefill. Decided once, over every decode row, before any
	// replica is priced -- the order the rows arrive in must not matter --
	// and remembered for the collector's row window, because prefill's row
	// is a one-minute max that can outlive decode's
	// (rememberDecodeSaturation).
	c.decodeSaturated = a.rememberDecodeSaturation(input.Namespace, input.ModelID,
		roleSaturated(input.ReplicaMetrics, c.rolesByVariant, domain.RoleDecode,
			cfg.QueueLengthThreshold, cfg.KvCacheThreshold))

	return c
}

// observeFleetShape reduces the replicas' disagreeing readings to one shape,
// and asks the tracker whether that shape just changed.
func (c *cycle) observeFleetShape() {
	// The output length the fleet is serving this cycle, once, for every
	// replica's throughput key (computeReplicaCapacity says why the key is
	// the fleet's shape and not the replica's).
	c.fleetOutput = fleetOutputLength(c.input.ReplicaMetrics, c.rolesByVariant)
	// The prefill side's hit rate, once for the role, for the same reason:
	// it discounts the prompt length that buckets prefill's throughput key,
	// and a per-replica figure would split one window between replicas.
	c.fleetHitRate = fleetPrefixHitRate(c.input.ReplicaMetrics, c.rolesByVariant)
	// The other axis, and the event. The prompt length arriving reads the
	// switch within a scrape of it, where the output half waits for a
	// completion; a change on either says the learned figures describe a
	// workload that is no longer running (shape_change.go).
	c.fleetInput = servedPromptLength(c.input.ReplicaMetrics)
	arriving, arrivingOK := arrivingPromptLength(c.input.SchedulerQueue)
	holdFor, _ := c.cfg.ShapeChangeHold(ShapeChangeHoldMax)
	c.stableOutput, c.stableInput, c.shapeChanged = c.a.noteFleetShape(
		c.input.Namespace, c.input.ModelID,
		c.fleetInput, c.fleetOutput, arriving, arrivingOK, holdFor, c.logger)
}

// resolvePricing decides the output length the derived mu divides by, and the
// prompt length it is priced against. They move together or not at all.
//
// The derived mu's divisor. The SHORT window is right only WHILE A SHAPE
// CHANGE IS OUTSTANDING, which is the case it was added for: a [5m]
// count-weighted mean carries the previous shape's long stragglers for
// minutes after they stop arriving -- measured decaying 3750 -> 250 across
// one 6000 -> 250 switch -- and the divisor is where that error reaches mu
// undamped.
//
// On a STEADY shape it is wrong, and expensively so. Phase 1 of the
// shape-swap trace holds 6000-token generations in flight for about two
// minutes before any of them completes, so a [1m] mean over COMPLETED
// requests reads far below 6000 while the fleet ramps. rate =
// tokenSec/avgOutput then OVER-states mu, and an over-stated mu
// UNDER-orders replicas (mu_from_itl.go says so explicitly).
//
// Bisected over five runs on 2026-10-03, EPP version, EPP config,
// max_num_seqs and cluster held identical with the WVA image the only
// variable. The unconditional short window took phase 1 from a 305-request
// router queue at 33,071 output tok/s to 2,503 at 8,763 -- eight times the
// queue for a quarter of the throughput. Every build before it measured
// good.
//
// max(recent, [5m]) is NOT the fix, and that is worth recording because it
// is the obvious one: the recent figure is the LOWER of the two in both
// cases -- an artefact while ramping, the truth after a switch -- so no
// magnitude test can separate them. Whether the shape changed can.
// Keyed on shapeChangedWithin, NOT on the outstanding hold. The hold's flag
// is zero whenever an operator sets DisableShapeChangeHold, which would
// leave this reading the short window for the single cycle the tracker
// declares a change and the [5m] mean for the rest of the straggler window
// -- reinstating the bug above through a flag documented as only turning
// off the fleet hold. ShapeChangeWindow says why the two are separate.
// The divisor falls back the same way the queue's price does, and for the
// same reason: rate = tokenSec/avgOutput, so a fleet that has completed
// nothing divides by ZERO and the derived mu reports not-ok. With no mu the
// demand floor emits nothing at all -- and the floor is where the router
// queue is projected forward over a replica's start time
// (floor.backlogAtLanding). So the one window the projection exists for is
// the one window it could not run in.
//
// Measured on run QM (2026-10-03, shape-swap phase 1): the first queued
// cycle was 14:39:44 and the first throughput-demand-floor line 14:41:59 --
// 135 s later, by which time the router queue had already peaked at 522 and
// begun draining. Of 56 not-ok derived-mu cycles, 38 carried a COMPLETE
// ITL fit (itlA 0.0277, itlB 0.0066, itlAtKPrice 0.0254) and failed on
// "avgOutputTokens": 0 alone.
//
// A measurement still wins, so a warm fleet is unaffected; this only answers
// where there was otherwise a zero. The error direction is also the safe one
// for a seed that is too LARGE: a bigger divisor under-states mu, and an
// under-stated mu over-orders during the cold window rather than
// under-ordering, which is the failure this is for.
//
// muInput is the prompt length the same mu is priced at, and it moves to
// the short window with the divisor or not at all.
//
// deriveMu reads the output length twice over: once as the divisor of
// rate = tokenSec/avgOutput, and once inside the shape, where KVreq =
// ILeff + OL/2 sets how many sequences fit. Moving only the divisor to the
// short window leaves the two halves of that division on different
// timescales, which is at its worst exactly here: a swap that raises the
// generation and drops the prompt has the divisor reach the new output in
// about a minute while KVreq still carries the old prompt, so the priced
// request is larger than either shape ever was and mu collapses.
//
// Measured on run QS (2026-10-04, 6000/1000 -> 1000/4000, a scenario whose
// documented answer is 2 decode replicas then 3): across the switch the
// divisor went 1088 -> 2897 -> 4000 while kvReq lagged 6251 -> 6030 ->
// 5896 toward a settled 3000, and the derived mu went 5.47 -> 2.12 -> 1.57
// against the 2.76 it settled at. The floor read replicasImplied 5.77 and
// the fleet sat at 7 decode replicas for about seven minutes. The router
// queue was ZERO on every one of those cycles, so none of it was a backlog
// response -- it was the price.
//
// Both halves or neither. If an engine publishes the short-window output
// but not the short-window prompt the divisor still moves alone, which is
// this mismatch -- but the alternative is the straggler bug the short
// window exists for, and that one is the larger error by an order of
// magnitude (3750 against 250). vLLM and SGLang both publish the pair, so
// the single-sided path is the engine-has-no-counter case, not a race.
func (c *cycle) resolvePricing() {
	c.muDivisor = c.cfg.ExpectedOutputTokens(c.fleetOutput, c.stableOutput, DefaultExpectedOutputTokens)
	c.muInput, c.muShortWindow = c.fleetInput, false
	if c.a.shapeChangedWithin(c.input.Namespace, c.input.ModelID,
		c.cfg.ShapeChangeWindow(ShapeChangeHoldMax), time.Now()) {
		if recent := fleetOutputLengthRecent(c.input.ReplicaMetrics, c.rolesByVariant); recent > 0 {
			c.muDivisor = recent
			if recentIn := servedPromptLengthRecent(c.input.ReplicaMetrics); recentIn > 0 {
				c.muInput, c.muShortWindow = recentIn, true
			}
		}
	}
}

// fitLines fits one ITL(k) line per variant per cycle, from readings its
// replicas report at whatever load they are at. This is what lets mu be
// priced for the shape arriving now instead of the one the fleet last
// saturated under (mu_from_itl.go).
func (c *cycle) fitLines() {
	c.itlModels = make(map[string]itl.Model, len(c.gpusByVariant))
	for variant := range c.gpusByVariant {
		// Decode only. ITL is the latency between GENERATED tokens, and
		// deriveMu divides a token rate by an output length; prefill emits
		// about one token per request -- its work is the prompt -- so the
		// same arithmetic would read three orders of magnitude low, which is
		// why saturatedCompletionRate special-cases it on the measured path
		// too (shape_change.go).
		// How long this variant's replicas take to become Ready, folded in from
		// whatever finished starting since the last cycle.
		//
		// BEFORE the decode guard, deliberately. That guard is about ITL -- the
		// latency between generated tokens, which prefill barely has -- while a
		// start time is an image and a node, which prefill has exactly like
		// decode. A P/D fleet projects its prefill backlog too, and leaving
		// prefill out meant it published neither series, so a run could not even
		// be reviewed for it.
		c.a.noteReplicaStart(c.a.itlWindowKey(c.input.Namespace, c.input.ModelID, variant,
			c.accelByVariant[variant], c.gpusByVariant[variant]),
			c.input.Namespace, variant, c.input.ReplicaMetrics, c.logger)

		if canonicalRole(c.rolesByVariant[variant]) != domain.RoleDecode {
			continue
		}
		// Keyed by what ITL(k) is a property of: the accelerator and how many
		// of them a replica has. Pooling two GPU products under one key is the
		// bug stableAccelerator exists to prevent for k2 (the k1<->k2
		// oscillation of PR #40 on a heterogeneous cluster), and a blended
		// ITL line is meaningless for either product.
		key := c.a.itlWindowKey(c.input.Namespace, c.input.ModelID, variant,
			c.accelByVariant[variant], c.gpusByVariant[variant])
		c.itlModels[variant] = c.a.noteITL(key, c.input.ReplicaMetrics, variant, c.a.now(), c.logger)
	}
}

// priceReplicas computes one capacity record per replica: its bounds, and the
// service rate the floor will divide the arrival rate by.
func (c *cycle) priceReplicas(ctx context.Context) ([]capacity.ReplicaCapacity, error) {
	replicaCapacities := make([]capacity.ReplicaCapacity, 0, len(c.input.ReplicaMetrics))
	for _, rm := range c.input.ReplicaMetrics {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		gpuCount := c.gpusByVariant[rm.VariantName]
		role := c.rolesByVariant[rm.VariantName]
		downstreamSaturated := c.decodeSaturated && canonicalRole(role) == domain.RolePrefill
		// Priced, then reported on. noteLineMismatch says when the observed
		// generation-token rate disagrees with the line, and does not act on
		// it: see its comment for the run that decided that.
		itlModel := c.itlModels[rm.VariantName]
		engineParams := engineParamsFor(c.a, c.input.Namespace, c.input.ModelID, rm.VariantName)
		fleetShape := shape.New(c.fleetInput, c.fleetOutput, rm.PrefixCacheHitRate)
		// muShape is the shape mu is PRICED at: fleetShape on a steady fleet,
		// and both halves on the short window while a shape change is
		// outstanding (see resolvePricing). fleetShape stays the fleet's own
		// [5m] reading, because that is the figure noteLineMismatch compares
		// an observed token rate against.
		muShape := fleetShape
		if c.muShortWindow {
			muShape = shape.New(c.muInput, c.muDivisor, rm.PrefixCacheHitRate)
		}
		// Only a generating role is priced from the ITL line. ITL is the gap
		// between two generated tokens, and a prefill replica emits one and
		// hands the KV to decode, so tokenSec/avgOutput does not describe it.
		//
		// This has always held, but only by accident: fitLines populates
		// itlModels for RoleDecode alone, so a prefill lookup returned the
		// zero Model and deriveMu declined on IsZero() in another file, keyed
		// on the opposite condition. A role-classification slip, or anyone
		// legitimately extending that population to RoleBoth, would have
		// started pricing prefill from decode's physics with nothing to say
		// so. Stated here instead, beside the call it governs.
		var derived derivedMu
		if canonicalRole(role) == domain.RoleDecode {
			kPrice := pricingK(c.cfg)
			derived = deriveMu(itlModel, engineParams, rm.TotalKvCapacityTokens,
				muShape, kPrice, c.muDivisor)
			// Every term, because the result alone cannot be attributed to one.
			// A derived mu that is wrong by 8x looks identical in the log
			// whether the fault is the output length, the sequence count, the
			// ITL line or the pricing point -- and that ambiguity cost four
			// discarded diagnoses in one session. maxNumSeqs is reported
			// separately from seqs so the cap is visible when it binds.
			var maxSeqs int64
			if engineParams != nil {
				maxSeqs = engineParams.MaxNumSeqs
			}
			c.logger.V(logging.DEFAULT).Info("derived-mu",
				"variant", rm.VariantName, "pod", rm.PodName,
				"ok", derived.ok,
				"rate", derived.rate, "seqs", derived.seqs, "tokenSec", derived.tokenSec,
				"kPrice", kPrice, "itlAtKPrice", itlModel.ITLAt(kPrice),
				"itlA", itlModel.A, "itlB", itlModel.B, "itlZero", itlModel.IsZero(),
				"avgOutputTokens", fleetShape.AvgOutputTokens,
				"muDivisor", c.muDivisor,
				"avgInputTokens", fleetShape.AvgInputTokens,
				"muInputTokens", c.muInput,
				"muShortWindow", c.muShortWindow,
				"kvReqPerSeq", muShape.KVreq,
				"kvReqFleet", fleetShape.KVreq,
				"replicaKvTokens", rm.TotalKvCapacityTokens,
				"maxNumSeqs", maxSeqs)
		}
		c.a.noteLineMismatch(itlModel, engineParams, rm, role, fleetShape.KVreq, c.logger)
		rc := c.a.computeReplicaCapacity(rm, c.cfg, c.input.ModelID, c.input.Namespace, gpuCount,
			role, c.accelByVariant[rm.VariantName], c.stableOutput, c.fleetOutput, c.stableInput,
			c.fleetHitRate, derived, downstreamSaturated, c.logger)
		if rc != nil {
			replicaCapacities = append(replicaCapacities, *rc)
		}
	}
	return replicaCapacities, nil
}

// settleShape releases an outstanding shape change once the fleet has measured
// itself under the shape now arriving.
//
// A replica reading a throughput window of its OWN, rather than a
// neighbouring bucket's, is the fleet having measured itself under the
// shape now arriving -- which is what an outstanding shape change was
// waiting for (shape_change.go).
func (c *cycle) settleShape(caps []capacity.ReplicaCapacity) {
	if !c.shapeChanged {
		return
	}
	measured := fleetHasMeasuredItself(caps)
	c.a.settleFleetShape(c.input.Namespace, c.input.ModelID, measured)
	c.shapeChanged = !measured
}

// priceDemand aggregates the per-replica records to variant and role totals,
// and adds what the router is holding and no pod has started.
func (c *cycle) priceDemand(caps []capacity.ReplicaCapacity) demandParts {
	variantCapacities := c.a.aggregateByVariant(caps, c.input.ReplicaMetrics,
		c.input.VariantStates, c.input.ModelID, c.input.Namespace, c.cfg.KvCacheThreshold, c.logger)

	// Model-level demand D (the analyzer owns demand attribution). Supply,
	// utilization, and RoleCapacities are assembled downstream by the engine's
	// capacity-build step from the per-variant capacities and RoleDemand, so they
	// are not set here — the analyzer emits only the measured (D, P) signal.
	total := aggregation.SumTotalDemand(variantCapacities)

	// Track active roles for queue demand attribution.
	activeRoles := make(map[string]bool)
	for _, vc := range variantCapacities {
		activeRoles[canonicalRole(vc.Role)] = true
	}

	// Add scheduler queue demand (requests queued upstream in llm-d flow control).
	// Price the queue at an output length the fleet has, recalls, or was told
	// -- in that order -- so a cold fleet does not value a growing queue at
	// zero. fleetOutput is this cycle's measurement and wins whenever it
	// exists; stableOutput is what this fleet last knew and carries across an
	// idle period; the rest is configuration.
	expectedOutput := c.cfg.ExpectedOutputTokens(c.fleetOutput, c.stableOutput, DefaultExpectedOutputTokens)
	queueMetrics := withExpectedOutputTokens(c.input.ReplicaMetrics, c.rolesByVariant, expectedOutput)
	queueDemand := estimateSchedulerQueueDemand(c.input.SchedulerQueue, queueMetrics,
		c.rolesByVariant, activeRoles, c.fleetHitRate)
	total += queueDemand.total
	if c.input.SchedulerQueue != nil {
		c.logger.Info("scheduler-queue-demand",
			"modelID", c.input.ModelID, "namespace", c.input.Namespace,
			"eppQueueSize", c.input.SchedulerQueue.QueueSize,
			"eppQueueBytes", c.input.SchedulerQueue.QueueBytes,
			"estimatedTokens", queueDemand.total, "byRole", queueDemand.byRole)
	}

	// Per-role demand attribution (P/D disaggregation); nil when non-disaggregated.
	// The builder pairs this with the per-role supply it recomputes.
	return demandParts{
		variants:    variantCapacities,
		roleDemand:  c.a.aggregateRoleDemand(variantCapacities, queueDemand.byRole),
		queueByRole: queueDemand.byRole,
		total:       total,
	}
}

// applyFloorAndHolds raises the demand to what the load requires, and then to
// what the two holds refuse to release. Every step here only ever increases
// it; nothing below lowers a demand the stages above computed.
func (c *cycle) applyFloorAndHolds(caps []capacity.ReplicaCapacity, d demandParts) float64 {
	total := d.total

	// Floor the demand at what the load requires in THROUGHPUT
	// (signals/floor, applied in throughput_floor.go).
	//
	// Everything above measures occupancy, which falls as capacity rises: a
	// fleet that is keeping up looks idle, and the target follows the signal
	// down. The floor divides the arrival rate by what a saturated replica of
	// each role was seen to complete -- a per-replica constant that does not
	// move when replicas are added -- so it holds the fleet at the size the
	// LOAD implies once occupancy stops implying anything.
	//
	// Strictly a floor: it never lowers demand. It does order -- lambda / mu
	// against a fleet that is short is the earliest signal there is, some 50 s
	// ahead of occupancy on the measured runs -- and it prices a backlog as
	// work to drain within BacklogDrainSeconds rather than as KV that must be
	// resident at once, which is what turned a 350-request queue into five to
	// seven extra replicas.
	//
	// There used to be a second floor here, from Little's law on the service
	// time the engines report. It was retired: service time is ITL x output
	// length and ITL grows with the batch, so the floor priced the same load at
	// 2.5M tokens on two replicas and 450k on four, ordered replicas while the
	// fleet was behind and released them once they arrived -- a positive
	// feedback loop, measured as a 2-4 replica oscillation with a ten-minute
	// period (docs/proposals/backlog-sizing.md, "Phase 2"). Worse, it ordered
	// them BEFORE any replica reached saturation, so the throughput this floor
	// depends on was never observed. The evidence, and what this floor does
	// that one could not, are in docs/developer-guide/saturation-demand-floor.md.
	var eppQueued float64
	if c.input.SchedulerQueue != nil {
		eppQueued = float64(c.input.SchedulerQueue.QueueSize)
	}
	total = c.a.applyThroughputFloor(c.input, c.cfg, caps, d.variants,
		total, d.roleDemand, d.queueByRole, eppQueued, c.shapeChanged, c.logger)

	// While decode is saturated, prefill's DEMAND is not a reading of prefill
	// either: the KV it holds and the queue behind it are decode's backlog
	// (computeK2 explains the mechanism), and the gate above only keeps them
	// out of what is LEARNED. Left in the demand they still act on the
	// decision -- against a k2 prefill learned on some earlier, genuine
	// saturation (small, as a compute bound is) the held KV alone reads as
	// several replicas, and against k1 on a fleet of two or more it reads as
	// a release: 537 800 on a supply of 1 839 718 is 29 %, one replica
	// removed while decode is saturated and re-ordered when it recovers. So
	// the role's demand is clamped into the band where the engine neither
	// orders nor releases (RC = 0 and SC = 0 under applyUniversalThreshold):
	// prefill keeps what it has until decode's numbers are its own again:
	// for as long as any decode replica's one-minute peak reads full and
	// queued, plus the memory -- 195 s and two decode starts replayed on
	// the measured cold pass, 135 s and one on the warm; unbounded while
	// decode is capped and cannot grow. The cycle decode recovers, prefill's
	// own occupancy and floor
	// stand. What the floor of the band buys is not a re-order avoided --
	// a released prefill replica would stay released, prefill reads near
	// zero after an episode -- but the burst that ends one: the scheduler's
	// flow control releases what it held in one go (124 requests, 744k
	// prompt tokens on a 919k k1, on the measured run), and it lands on
	// prefill first. Only the disaggregated case has a prefill
	// entry to hold; the model-level total moves by the same amount so
	// RoleDemand and TotalDemand keep moving together.
	if c.decodeSaturated && d.roleDemand != nil {
		scaleUp, scaleDown := c.cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
		// The scheduler queue's share is exempt: those requests have been
		// given to no pod and have had no first token, so prefill is what
		// they are waiting for whatever decode is doing.
		undispatched := d.queueByRole[domain.RolePrefill]
		if h, held := holdPrefillDemand(d.roleDemand, d.variants, scaleUp, scaleDown, undispatched); held {
			total += h.after - h.before
			c.logger.Info("prefill-demand-held",
				"modelID", c.input.ModelID, "namespace", c.input.Namespace,
				"demandBefore", h.before, "demandHeld", h.after, "holdFloor", h.lo, "holdCap", h.hi,
				"undispatched", undispatched,
				"reason", "decode saturated: the KV prefill holds and the queue behind it are decode's backlog; prefill is neither ordered nor released on them, but the scheduler queue's share is not held -- no pod has started those")
		}
	}

	// The fleet is not released on figures the switch made stale. A floor,
	// not a clamp: an I-up switch genuinely needs more capacity, and
	// occupancy and the throughput floor still order (shape_change.go).
	if c.shapeChanged && d.roleDemand != nil {
		_, scaleDown := c.cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
		if moved, raised := holdFleetFloor(d.roleDemand, d.variants, scaleDown); moved > 0 {
			total += moved
			logShapeHold(c.logger, c.input.ModelID, c.input.Namespace, moved, raised)
		}
	}

	return total
}
