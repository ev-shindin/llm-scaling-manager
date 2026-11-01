package saturation

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"

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

// analyze computes capacity signals for a model across all its variants.
//
// The stages run in this order and the order matters: the shape has to be
// observed before mu can be priced at it, replicas have to be priced before
// the fleet can be said to have measured itself, and the floor and the holds
// come last because they only ever raise a demand the stages above computed.
func (a *SaturationAnalyzer) analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	satConfig, ok := input.Config.(*config.ScalingPolicy)
	if !ok {
		return nil, fmt.Errorf("expected *ScalingPolicy, got %T", input.Config)
	}
	// Without the engine's settle tracker, the analyzer observes its own,
	// first, as the engine does before any analyzer runs (settle_view.go).
	a.observeOwnSettle(input, a.now())

	// Learned state restored after a controller restart waits for this: the
	// engine has just loaded each variant's configuration from its scale
	// target, so a variant's restored entries can be checked against the
	// configuration they were learned under, and installed or rejected, before
	// anything below reads them (learned_snapshot.go). Once per variant.
	for _, vs := range input.VariantStates {
		r, done := a.installRestored(input.Namespace, input.ModelID, vs.VariantName)
		if !done {
			continue
		}
		msg := "learned state restored"
		switch r.Outcome {
		case RestoredWhole:
		case RestoredArgs:
			msg = "learned state restored without the ITL baseline: the engine image changed, the arguments did not"
		case RestoredPrior:
			msg = "learned state restored as lower-only figures: the variant's engine arguments changed, so its k2 windows become previous figures (P2-prior) for the buckets they were measured in"
		case RestoredIncomplete:
			msg = "learned state restored under the unread configuration: the variant's engine configuration could not be read completely; its windows are its own only if it runs the same unread configuration, and lower-only figures otherwise"
		case RestoredDiffers:
			msg = "learned state restored as lower-only figures: the document recorded no engine arguments and its configuration differs, so its k2 windows become previous figures (P2-prior)"
		}
		switch {
		case r.Outcome == RestoredIncomplete && r.Installed:
			msg = "learned state restored under the unread configuration: the variant's engine configuration could not be read completely, and it runs the same unread configuration it was exported under, so its windows are its own"
		case r.Outcome == RestoredIncomplete && r.NoArgs:
			msg = "learned state not restored: the variant's engine configuration could not be read completely, and the snapshot recorded no engine arguments to check its windows against"
		case r.Outcome == RestoredWhole || r.Outcome == RestoredArgs || r.Priors > 0:
		case r.BoundPriors > 0:
			msg = "learned state restored as lower-only figures: the variant's own previous figures, kept with their lower bounds, price it (P2-prior) until it measures under the arguments it runs"
		case r.Throughput > 0:
			msg = "learned state restored as a hold-only service rate: no k2 window was recent enough, on the hardware the variant runs now, to keep, and the old saturated throughput holds the demand floor, hold-only, until the new arguments measure their own"
		case r.Outcome == RestoredIncomplete && r.Stored == 0:
			msg = "learned state not restored: the variant's engine configuration could not be read completely, and the snapshot held no k2 window of the variant's own to keep"
		case r.Outcome == RestoredIncomplete:
			msg = "learned state not restored: the variant's engine configuration could not be read completely, and no k2 window was recent enough, on the hardware it runs now, to keep"
		case r.Stored == 0:
			msg = "learned state not restored: the variant's engine configuration changed, and the snapshot held no k2 window of the variant's own to keep as a lower-only figure"
		default:
			msg = "learned state not restored: the variant's engine configuration changed, and no k2 window was recent enough, on the hardware it runs now, to keep as a lower-only figure"
		}
		// Counted by outcome (wva_learned_state_restores_total).
		a.mu.Lock()
		a.restoreOutcomes[r.Outcome]++
		a.mu.Unlock()
		ctrl.LoggerFrom(ctx).Info(msg, "namespace", r.Namespace, "variant", r.Variant, "outcome", r.Outcome,
			"entries", r.Entries, "priors", r.Priors, "throughput", r.Throughput, "stored", r.Stored,
			"boundPriors", r.BoundPriors)
	}

	// One learner-start line per model, at its first cycle (k2_learner.go).
	variantNames := make([]string, 0, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		variantNames = append(variantNames, vs.VariantName)
	}
	a.logLearnerStart(ctx, input.Namespace, input.ModelID, variantNames)
	a.beginK2Cycle(input.Namespace, input.ModelID)
	a.beginMuCycle(input.Namespace, input.ModelID)

	c := a.newCycle(ctx, input, satConfig)
	c.observeFleetShape()
	c.resolvePricing()
	c.fitLines()

	caps, err := c.priceReplicas(ctx)
	if err != nil {
		return nil, err
	}
	// After the last replica is priced: k2's and mu's reports and
	// learner-write lines.
	a.closeK2Cycle(ctx, input.Namespace, input.ModelID)
	a.closeMuCycle(ctx, input.Namespace, input.ModelID, caps)
	c.settleShape(caps)

	d := c.priceDemand(caps)
	total, held, hotRank := c.applyFloorAndHolds(caps, d)

	roleOf := make(map[string]string, len(c.rolesByVariant))
	for variant, role := range c.rolesByVariant {
		roleOf[variant] = domain.CanonicalRole(role)
	}

	return &domain.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        a.now(),
		VariantCapacities: d.variants,
		TotalDemand:       total,
		RoleDemand:        d.roleDemand,
		HeldRoleDemand:    held,
		HotRankDemand:     hotRank,
		RoleStartSeconds:  a.slowestStartSecondsByRole(input, roleOf),
		// The raw rows, deferred requests included: a queue read too deep
		// here only keeps a replica (decodeCapacityView says which readers
		// take the view).
		VariantBusiestQueueLoad: busiestQueueLoadByVariant(input.ReplicaMetrics, c.rolesByVariant,
			satConfig.QueueLengthThreshold, satConfig.KvCacheThreshold),
		PricingK: pricingK(satConfig),
	}, nil
}

// cycle is the state of one Analyze call, carried between its stages.
//
// A struct rather than a dozen parameters: the stages genuinely share this
// much, and threading it positionally is how an argument ends up in the wrong
// slot.
//
// Every field here is written by exactly ONE stage and read only by later
// ones -- except shapeChanged, which is deliberately two-phase: raised by
// observeFleetShape when the tracker declares a change that raises a hold
// (or one raised earlier is still outstanding), and cleared by
// settleShape once the fleet has measured itself under the new shape. That
// one exception is the reason the holds in applyFloorAndHolds can ask "is a
// change still outstanding" rather than "did one happen this cycle".
//
// The single-writer property for everything else is what makes the stage
// order above checkable rather than conventional, so a new field should keep
// it and a second exception should be argued for here.
type cycle struct {
	a     *SaturationAnalyzer
	input domain.AnalyzerInput
	cfg   *config.ScalingPolicy
	// tune is cfg's analyzer tuning, defaults filled in (tuningOf).
	tune   tuning
	logger logr.Logger

	// newCycle: variant-level identity, and whether decode is saturated.
	gpusByVariant   map[string]int
	rolesByVariant  map[string]string
	accelByVariant  map[string]string
	decodeSaturated bool
	// busyNowRoles is the roles with a Ready replica busy NOW by its
	// instant KV (rolesBusyNow). A sibling idle now is charged its instant
	// KV, not its one-minute peak, while its role is in it.
	busyNowRoles map[string]bool

	// priceReplicas: how many decode replicas have read idle for
	// IdleSettledFor (settledIdleDecode), found once every row is priced.
	settledIdleDecode int

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

	// newCycle: the engine-configuration fingerprint per variant. It decides
	// WHOSE fitted line a variant may borrow when it has none of its own --
	// not which window its own readings go into, which is always its own.
	fpByVariant map[string]string

	// fitLines: one ITL(k) line per decode variant, and for each one whether
	// it was BORROWED from a sibling that shares the engine configuration
	// rather than fitted from this variant's own readings. A borrowed line may
	// hold the fleet but not grow it -- see priceReplicas.
	itlModels map[string]itl.Model
	// itlBorrowedFrom maps a variant to the variant whose fitted line it is
	// using, and is empty for a variant using its own. The lender rather than
	// a bool because a mismatch on a borrowed line needs the lender named --
	// see noteLineMismatch -- and a second map holding the same fact could
	// drift from the first.
	itlBorrowedFrom map[string]string
	// itlHeld marks a variant whose own line is its last slope held for want
	// of readings (itl.Reading.SlopeHeld), not a fit made this cycle.
	itlHeld map[string]bool
	// itlSamples is how many readings each decode variant's OWN ITL window
	// holds after this cycle's, whatever line it ends up priced on. Carried
	// for the service-rate signal (attachServiceRates), where it is the
	// number that explains a variant with no line.
	itlSamples map[string]int
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
		tune:   tuningOf(cfg),
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
	c.fpByVariant = make(map[string]string, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		c.gpusByVariant[vs.VariantName] = vs.GPUsPerReplica
		c.rolesByVariant[vs.VariantName] = vs.Role
		c.accelByVariant[vs.VariantName] = vs.AcceleratorName
		// Resolved once per cycle, here, because the ITL window is keyed by
		// it and reading the store per replica would take its lock on every
		// row. An empty fingerprint is a usable answer: itlPhysicsKey falls
		// back to the variant key and pools nothing.
		//
		// Which is exactly what a variant that may not reuse wants, so it is
		// expressed by withholding the fingerprint rather than by a second
		// branch inside the key function. The operator switch and an
		// unreadable engine configuration then reach the same code path, and
		// there is only one of them to reason about.
		//
		// Reuse is OFF unless EnableLearnedStateReuse says otherwise, so this
		// is the branch taken on a default install.
		if !cfg.EnableLearnedStateReuse {
			c.fpByVariant[vs.VariantName] = ""
		} else {
			c.fpByVariant[vs.VariantName] = a.engineFingerprint(
				input.Namespace, input.ModelID, vs.VariantName)
		}
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
	// (rememberDecodeSaturation). Read on the raw rows: on a full decode a
	// landed pull that cannot be scheduled reads deferred too, so the
	// capacity part alone would clear a decode that is full and queued, and
	// prefill would learn decode's back-pressure as its own k2
	// (decodeCapacityView).
	c.decodeSaturated = a.rememberDecodeSaturation(input.Namespace, input.ModelID,
		roleSaturated(input.ReplicaMetrics, c.rolesByVariant, domain.RoleDecode,
			cfg.QueueLengthThreshold, cfg.KvCacheThreshold))
	// Decided over every row before any is priced, for the same reason.
	c.busyNowRoles = c.tune.rolesBusyNow(input.ReplicaMetrics, c.rolesByVariant)

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
// The SHORT window is right only WHILE A SHAPE CHANGE IS OUTSTANDING. A [5m]
// count-weighted mean carries the previous shape's long stragglers for minutes
// after they stop arriving, and the divisor is where that error reaches mu
// undamped. On a STEADY shape the short window is wrong the other way: it
// averages over requests that have COMPLETED, so a fleet ramping into long
// generations reads far below the length being served, which over-states mu --
// and an over-stated mu UNDER-orders replicas.
//
// max(recent, [5m]) is NOT the fix, and that is worth saying because it is the
// obvious one: the recent figure is the LOWER of the two in both cases -- an
// artefact while ramping, the truth after a switch -- so no magnitude test can
// separate them. Whether the shape changed can.
//
// Keyed on shapeChangedWithin, NOT on the outstanding hold. The hold's flag is
// zero whenever an operator sets DisableShapeChangeHold, which would leave
// this reading the short window for the single cycle the tracker declares a
// change and the [5m] mean for the rest of the straggler window -- reinstating
// the bug through a flag documented as only turning off the fleet hold.
// ShapeChangeWindow says why the two are separate.
//
// The divisor falls back to a seed rather than dividing by zero, the same way
// the queue's price does and for the same reason: rate = tokenSec/avgOutput,
// so a fleet that has completed nothing divides by ZERO, the derived mu reports
// not-ok, and with no mu the demand floor emits nothing at all -- in the one
// window the backlog projection exists for. A measurement always wins, so a
// warm fleet is unaffected.
//
// BOTH HALVES OR NEITHER. deriveMu reads the output length twice: as the
// divisor, and inside the shape, where KVreq = ILeff + OL/2 sets how many
// sequences fit. Moving only the divisor leaves the two halves of one division
// on different timescales, and a swap that raises the generation while dropping
// the prompt then prices a request larger than either shape ever was. An engine
// that publishes the short-window output but not the short-window prompt moves
// the divisor alone, which IS that mismatch -- accepted deliberately, because
// the alternative is the straggler bug above and that is the larger error by an
// order of magnitude.
//
// Measured: docs/developer-guide/analyzer-evidence.md, "The mu divisor follows
// the shape change, not the ramp" and "Both halves of the shape move to the
// short window together".
func (c *cycle) resolvePricing() {
	c.muDivisor = c.cfg.ExpectedOutputTokens(c.fleetOutput, c.stableOutput, DefaultExpectedOutputTokens)
	c.muInput, c.muShortWindow = c.fleetInput, false
	// On a.now: noteFleetShape stamps lastChangedAt with it.
	if c.a.shapeChangedWithin(c.input.Namespace, c.input.ModelID,
		c.cfg.ShapeChangeWindow(ShapeChangeHoldMax), c.a.now()) {
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
	c.itlBorrowedFrom = make(map[string]string, len(c.gpusByVariant))
	c.itlHeld = make(map[string]bool, len(c.gpusByVariant))
	c.itlSamples = make(map[string]int, len(c.gpusByVariant))
	variants := make([]string, 0, len(c.gpusByVariant))
	for variant := range c.gpusByVariant {
		variants = append(variants, variant)
	}
	// Sorted so a cycle's log reads the same way twice, and because the order
	// DOES still decide something.
	//
	// An earlier comment here claimed it did not -- that "a line is only ever
	// borrowed from a PREVIOUS cycle's publication". That is false, and was
	// measured to be: publishLine writes a.itlLines and borrowLine reads it
	// inside this one loop, so a lender sorting before a borrower is borrowed
	// from in the SAME cycle. Renaming the lender from `a-fitter` to
	// `z-fitter` flips a first-cycle borrow from happening to not happening,
	// on otherwise identical input.
	//
	// Sorting is what makes that deterministic, which is the property worth
	// having: the alternative is a borrow that depends on Go's map iteration
	// order and so differs between two runs of the same fleet. What sorting
	// does NOT give is the BEST lender -- publishLine overwrites
	// unconditionally, so with two publishers on one fingerprint the stored
	// line is the alphabetically greatest variant's, whatever its fit quality.
	// itl.Model carries neither the tier nor the sample count, so there is
	// nothing to tie-break on here yet; that is a known gap, not a property.
	sort.Strings(variants)
	mixed := revisionsMixed(c.input.ReplicaMetrics)
	for _, variant := range variants {
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
		variantKey := c.a.itlWindowKey(c.input.Namespace, c.input.ModelID, variant,
			c.accelByVariant[variant], c.gpusByVariant[variant])
		c.a.noteReplicaStart(variantKey,
			c.input.Namespace, variant, c.input.ReplicaMetrics, c.logger)

		if domain.CanonicalRole(c.rolesByVariant[variant]) != domain.RoleDecode {
			continue
		}
		// Fit this variant's OWN readings first, into its own window, keyed
		// by what ITL(k) is a property of: the accelerator and how many of
		// them a replica has. Pooling two GPU products under one key is the
		// bug stableAccelerator exists to prevent for k2 (the k1<->k2
		// oscillation of PR #40 on a heterogeneous cluster), and a blended
		// ITL line is meaningless for either product.
		// Under the arguments the variant runs, and, mid-rollout, fitted from
		// the window it holds with no new readings: a reading may be an old
		// pod's, as for k2 and the throughput (replica_capacity.go).
		rec := c.a.capacityStore.Get(c.input.Namespace, c.input.ModelID, variant)
		var params *capacity.EngineParams
		if rec != nil {
			params = rec.EngineParams
		}
		c.a.itlUnderArgs(variantKey, windowArgs(params))
		rows := c.input.ReplicaMetrics
		if rec.InRollout() || mixed[variant] {
			rows = nil
		}
		// The namespace rides on every ITL line because the variant name alone
		// does not identify one: the same Deployment applied to two namespaces
		// logs two identical "variant" values, and their fits could not be
		// told apart in the log (issue #271).
		itlLogger := c.logger.WithValues("namespace", c.input.Namespace)
		// The policy's band, and the sequence cap only when the engine's flag
		// stated it, so a replica at it may teach the line from above the
		// band (itl.Admission).
		lo, hi := c.cfg.ITLBand()
		// A held slope is bounded by the horizon a finished fit is kept on
		// (BucketRetention, which ages the shared lines too) and by the
		// arguments it was fitted under (issue #284).
		adm := itl.Admission{MinK: lo, MaxK: hi, MaxNumSeqs: params.ExplicitMaxNumSeqs(),
			CapMaxPreemptionRate: c.cfg.ITLCapPreemptionRate(), Steady: c.tune.steadiness(),
			SlopeHoldFor: c.a.Horizons().BucketRetention, Args: windowArgs(params)}
		reading := c.a.noteITL(variantKey, rows, variant, adm, c.a.now(), itlLogger)
		own := reading.Model
		// The variant's own window, by the same key noteITL just wrote, and
		// before any borrow below: a borrowed line says nothing about how
		// close this variant is to fitting one of its own.
		c.itlSamples[variant] = c.a.itlWindowLen(variantKey)
		physicsKey := c.a.itlPhysicsKey(c.input.Namespace, c.input.ModelID, variant,
			c.accelByVariant[variant], c.gpusByVariant[variant], c.fpByVariant[variant])

		if !own.IsZero() {
			// A measured line becomes the line for this engine configuration,
			// so a sibling with nothing of its own can start from it.
			//
			// A HELD one does not. It is a fit from some time ago, and
			// publishLine stamps learnedAt with now: republished every cycle,
			// a slope held through days of light load, or by a variant parked
			// at zero, would never age out of itlLines (EvictStaleHistory
			// ages a line on when it was last written, because it is a
			// finished fit) and would overwrite a sibling's fresh line on the
			// same configuration. What was published when it was fitted stays
			// published, and ages from then.
			if !reading.SlopeHeld {
				c.a.publishLine(physicsKey, variantKey, own, c.a.now())
			}
			c.itlModels[variant] = own
			c.itlHeld[variant] = reading.SlopeHeld
			continue
		}

		// Nothing of its own. Borrow the line another variant measured for the
		// same configuration, if there is one. This is the saving the
		// fingerprint was built for: a rename, a second identical variant or
		// the same deployment in another namespace gets a line immediately
		// instead of paying the ~28 cycles a fit takes -- measured from load
		// start on run QT, which is the figure a controller-boot anchor had
		// inflated to ~60.
		borrowed, from, ok := c.a.borrowLine(physicsKey, variantKey)
		if !ok {
			// No line at all: none of its own -- a fresh variant whose
			// replicas sit below the band offers the window nothing to fit
			// -- and none to borrow. So no mu is derived, and unless a
			// saturated reading is on record the throughput floor does not
			// apply and the variant is sized by KV occupancy alone, which at
			// light load reads far below the share of its throughput it is
			// serving (issue #284). Said in so many words, because the
			// itl-window line alone reads as a window that is broken.
			itlLogger.V(logging.DEFAULT).Info("itl-line-none",
				"variant", variant, "key", variantKey, "physicsKey", physicsKey,
				"windowSamples", c.itlSamples[variant], "minSamples", itl.DefaultMinSamples,
				"derivedMu", false)
			continue
		}
		c.itlModels[variant] = borrowed
		c.itlBorrowedFrom[variant] = from
		itlLogger.V(logging.DEFAULT).Info("itl-line-borrowed",
			"variant", variant, "from", from, "physicsKey", physicsKey,
			"a", borrowed.A, "b", borrowed.B)
	}
}

// explicitMaxNumSeqs is, per variant with replica rows this cycle, the
// engine's batch cap when its flags stated it -- max_num_seqs, lowered by
// --max-num-active-seqs (capacity.EngineParams.ExplicitMaxNumSeqs) -- and zero for one whose cap is
// unknown or only a default.
func (c *cycle) explicitMaxNumSeqs() map[string]int64 {
	out := map[string]int64{}
	for _, rm := range c.input.ReplicaMetrics {
		if _, seen := out[rm.VariantName]; seen {
			continue
		}
		var params *capacity.EngineParams
		if rec := c.a.capacityStore.Get(c.input.Namespace, c.input.ModelID, rm.VariantName); rec != nil {
			params = rec.EngineParams
		}
		out[rm.VariantName] = params.ExplicitMaxNumSeqs()
	}
	return out
}

// priceReplicas computes one capacity record per replica: its bounds, and the
// service rate the floor will divide the arrival rate by.
func (c *cycle) priceReplicas(ctx context.Context) ([]capacity.ReplicaCapacity, error) {
	replicaCapacities := make([]capacity.ReplicaCapacity, 0, len(c.input.ReplicaMetrics))
	pricedRows := make([]domain.ReplicaMetrics, 0, len(c.input.ReplicaMetrics))
	// Why a decode replica has no derived mu, by pod, for
	// replica-capacity-summary: derived-mu says it at perReplicaLogLevel only.
	muMissing := make(map[string]string)
	mixed := revisionsMixed(c.input.ReplicaMetrics)
	// Priced on the queue decode capacity drains: on decode of a P/D model,
	// requests waiting for their KV from prefill are not one
	// (decodeCapacityView), except on a replica at its sequence cap. The
	// idle tests below read the raw row.
	rows := decodeCapacityViews(c.input.ReplicaMetrics, c.rolesByVariant, c.explicitMaxNumSeqs())
	served := c.tune.servedBounds(rows, c.rolesByVariant, c.cfg.QueueLengthThreshold, c.cfg.KvCacheThreshold, c.fleetOutput,
		c.stableInput, c.stableOutput, c.fleetHitRate)
	for i, rm := range rows {
		raw := c.input.ReplicaMetrics[i]
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		gpuCount := c.gpusByVariant[rm.VariantName]
		role := c.rolesByVariant[rm.VariantName]
		downstreamSaturated := c.decodeSaturated && domain.CanonicalRole(role) == domain.RolePrefill
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
		if domain.CanonicalRole(role) == domain.RoleDecode {
			kPrice := pricingK(c.cfg)
			derived = deriveMu(itlModel, engineParams, rm.Ranks(), rm.TotalKvCapacityTokens,
				muShape, kPrice, c.muDivisor)
			if !derived.ok {
				muMissing[rm.PodName] = derived.why
			}
			// Every term, because the result alone cannot be attributed to one.
			// A derived mu that is wrong by 8x looks identical in the log
			// whether the fault is the output length, the sequence count, the
			// ITL line or the pricing point -- and that ambiguity cost four
			// discarded diagnoses in one session. maxNumSeqs is reported
			// separately from seqs so the cap is visible when it binds. One
			// line per replica, so at perReplicaLogLevel.
			var maxSeqs, activeSeqs int64
			if engineParams != nil {
				maxSeqs, activeSeqs = engineParams.MaxNumSeqs, engineParams.MaxNumActiveSeqs
			}
			c.logger.V(perReplicaLogLevel).Info("derived-mu",
				"namespace", c.input.Namespace, "variant", rm.VariantName, "pod", rm.PodName,
				"ok", derived.ok,
				"rate", derived.rate, "seqs", derived.seqs, "tokenSec", derived.tokenSec,
				"kPrice", kPrice, "itlAtKPrice", itlModel.ITLAt(kPrice),
				"itlA", itlModel.A, "itlB", itlModel.B, "itlZero", itlModel.IsZero(),
				// Whose line it is. Without this the record reports a rate
				// built on a sibling's physics identically to one built on the
				// variant's own, and the two carry different authority
				// downstream: a borrowed line may hold a fleet and not grow
				// one. itl-line-borrowed says it once per cycle; this says it
				// beside the number it produced.
				"lineBorrowed", c.itlBorrowedFrom[rm.VariantName] != "",
				"lineBorrowedFrom", c.itlBorrowedFrom[rm.VariantName],
				// The variant's own last slope, held for want of readings:
				// an old fit, not this cycle's.
				"lineHeld", c.itlHeld[rm.VariantName],
				"avgOutputTokens", fleetShape.AvgOutputTokens,
				"muDivisor", c.muDivisor,
				"avgInputTokens", fleetShape.AvgInputTokens,
				"muInputTokens", c.muInput,
				"muShortWindow", c.muShortWindow,
				"kvReqPerSeq", muShape.KVreq,
				"kvReqFleet", fleetShape.KVreq,
				"replicaKvTokens", rm.TotalKvCapacityTokens,
				// Per engine rank; the replica admits engineRanks times the
				// lower of the two (0 when the active cap is unset).
				"maxNumSeqs", maxSeqs, "maxNumActiveSeqs", activeSeqs, "engineRanks", rm.Ranks())
		}
		c.a.noteLineMismatch(itlModel, engineParams, rm, role, fleetShape.KVreq,
			c.itlBorrowedFrom[rm.VariantName], c.logger)
		rc := c.a.computeReplicaCapacity(rm, c.cfg, c.tune, c.input.ModelID, c.input.Namespace, gpuCount,
			role, c.accelByVariant[rm.VariantName], c.stableOutput, c.fleetOutput, c.stableInput,
			c.fleetHitRate, derived, served[rm.VariantName], downstreamSaturated, mixed[rm.VariantName],
			replicaRouting{
				kvFromInstant: c.tune.kvFromInstant(raw, c.busyNowRoles[domain.CanonicalRole(role)]),
			}, c.logger)
		if rc != nil {
			// A derived figure inherits the borrowed-ness of the line it came
			// from. Set here rather than threaded through
			// computeReplicaCapacity's parameter list, because it is a
			// property of the CYCLE's line resolution (fitLines) and not of
			// the replica being priced.
			// Only when the derived figure is the one in use. A variant
			// holding a borrowed line but priced from its OWN measured window
			// is not relying on the borrow, and flagging it would deny it an
			// order it has earned.
			rc.SaturatedThroughputLineBorrowed =
				rc.SaturatedThroughputDerived && c.itlBorrowedFrom[rm.VariantName] != ""
			replicaCapacities = append(replicaCapacities, *rc)
			pricedRows = append(pricedRows, raw)
		}
	}
	logReplicaCapacitySummary(c.logger, c.input.ModelID, c.input.Namespace, replicaCapacities, muMissing)
	// Whether a decode replica is idle against what it can do needs its
	// effective capacity, so every row priced first (idle_room.go).
	c.settledIdleDecode = c.a.settledIdleDecode(c.input.Namespace, c.input.ModelID,
		replicaCapacities, pricedRows, c.rolesByVariant, c.tune)
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

// variantsPricedFromBorrowedLine is the set of variants at least one of whose
// own replicas was priced from a sibling's ITL line this cycle. A bridge reads
// the variant's line too, but does not count: the throughput floor, the one
// consumer of a derived rate, skips bridges.
func variantsPricedFromBorrowedLine(caps []capacity.ReplicaCapacity) map[string]bool {
	out := make(map[string]bool)
	for _, rc := range ownRowsOf(caps) {
		if rc.SaturatedThroughputLineBorrowed {
			out[rc.VariantName] = true
		}
	}
	return out
}

// priceDemand aggregates the per-replica records to variant and role totals,
// and adds what the router is holding and no pod has started.
func (c *cycle) priceDemand(caps []capacity.ReplicaCapacity) demandParts {
	variantCapacities := c.a.aggregateByVariant(caps, c.input.ReplicaMetrics,
		c.input.VariantStates, c.input.ModelID, c.input.Namespace, c.cfg.KvCacheThreshold,
		!c.cfg.EnableLearnedStateReuse, c.logger)
	pricedFromLine := variantsPricedFromBorrowedLine(caps)
	for i := range variantCapacities {
		vc := &variantCapacities[i]
		from := c.itlBorrowedFrom[vc.VariantName]
		// Only a variant one of whose own replicas was PRICED from the line
		// borrowed it: fitLines resolves a line for every known variant -- one
		// at zero, carried by bridges alone, or priced from its own measured
		// window included -- and holding a line is not using it.
		if from == "" || !pricedFromLine[vc.VariantName] {
			continue
		}
		// from is the lender's itlWindowKey, namespace|model|variant|...: the
		// line is shared by configuration, so the lender may be in another
		// namespace, and is named with it.
		b := domain.LearnedStateBorrow{Quantity: domain.BorrowedITLLine, Donor: from}
		if f := strings.Split(from, "|"); len(f) > 2 {
			b.DonorNamespace, b.Donor = f[0], f[2]
		}
		vc.Borrowed = append(vc.Borrowed, b)
	}
	// The service-rate signal rides on the variant records, so it reaches the
	// engine on the same path as the target it explains.
	c.attachServiceRates(variantCapacities, caps)

	// Model-level demand D (the analyzer owns demand attribution). Supply,
	// utilization, and RoleCapacities are assembled downstream by the engine's
	// capacity-build step from the per-variant capacities and RoleDemand, so they
	// are not set here — the analyzer emits only the measured (D, P) signal.
	total := aggregation.SumTotalDemand(variantCapacities)

	// Track active roles for queue demand attribution.
	activeRoles := make(map[string]bool)
	for _, vc := range variantCapacities {
		activeRoles[domain.CanonicalRole(vc.Role)] = true
	}

	// Add scheduler queue demand (requests queued upstream in llm-d flow control).
	// Price the queue at an output length the fleet has, recalls, or was told
	// -- in that order -- so a cold fleet does not value a growing queue at
	// zero. fleetOutput is this cycle's measurement and wins whenever it
	// exists; stableOutput is what this fleet last knew and carries across an
	// idle period; the rest is configuration.
	expectedOutput := c.cfg.ExpectedOutputTokens(c.fleetOutput, c.stableOutput, DefaultExpectedOutputTokens)
	queueMetrics := withExpectedOutputTokens(c.input.ReplicaMetrics, c.rolesByVariant, expectedOutput)
	// With no replica reporting a shape at all, both halves fall back: the
	// output as above, the prompt to what the fleet last knew
	// (estimateSchedulerQueueDemandAt).
	queueDemand := estimateSchedulerQueueDemandAt(c.input.SchedulerQueue, queueMetrics,
		c.rolesByVariant, activeRoles, c.fleetHitRate,
		queueShape{input: c.stableInput, output: expectedOutput})
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
// what the holds refuse to release. Every step here only ever increases
// it; nothing below lowers a demand the stages above computed. It returns the
// total, each held role's demand from before its first hold
// (AnalyzerResult.HeldRoleDemand), and what the hot-rank hold added per role
// (AnalyzerResult.HotRankDemand).
func (c *cycle) applyFloorAndHolds(caps []capacity.ReplicaCapacity, d demandParts) (float64, map[string]float64, map[string]float64) {
	total := d.total
	var held map[string]float64
	hold := func(role string, before float64) {
		if held == nil {
			held = make(map[string]float64)
		}
		if _, ok := held[role]; !ok {
			held[role] = before
		}
	}

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
	//
	// One thing relaxes the hold and nothing else: a decode replica that
	// has read idle for IdleSettledFor (settledIdleDecode). Then decode has
	// room the router is not using, and prefill's demand is its own to be
	// ordered on; what prefill may LEARN stays gated on decodeSaturated
	// alone (roleSaturated says why). Relaxing a hold only raises an order.
	prefillHeld := c.decodeSaturated && c.settledIdleDecode == 0
	if prefillHeld && d.roleDemand != nil {
		scaleUp, scaleDown := c.cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
		// The scheduler queue's share is exempt: those requests have been
		// given to no pod and have had no first token, so prefill is what
		// they are waiting for whatever decode is doing.
		undispatched := d.queueByRole[domain.RolePrefill]
		if h, ok := holdPrefillDemand(d.roleDemand, d.variants, scaleUp, scaleDown, undispatched); ok {
			total += h.after - h.before
			hold(domain.RolePrefill, h.before)
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
		before := maps.Clone(d.roleDemand)
		if moved, raised := holdFleetFloor(d.roleDemand, d.variants, scaleDown); moved > 0 {
			total += moved
			for role := range raised {
				hold(role, before[role])
			}
			logShapeHold(c.logger, c.input.ModelID, c.input.Namespace, moved, raised)
		}
	}

	// A replica with a full engine rank is not released on its mean
	// (hot_rank_hold.go). It keeps that replica, not the role, so it is
	// reported apart from the holds (hotRank, not held): a held role releases
	// nothing, and the other replicas' spare is still theirs to give. Last,
	// so its cap under the scale-up threshold sees every raise above: run
	// before the prefill hold, it filled decode's room to the model's
	// ceiling and the prefill hold's raise then took the model over it.
	// Prefill is judged only while decode is not saturated, so this never
	// raises a role the prefill hold set.
	var hotRank map[string]float64
	{
		scaleUp, scaleDown := c.cfg.AnalyzerThresholds(domain.SaturationAnalyzerName)
		hot := c.a.hotRankPods(c.input.Namespace, c.input.ModelID, c.input.ReplicaMetrics,
			c.rolesByVariant, c.cfg.KvCacheThreshold, prefillHeld)
		after, byRole := holdHotRanks(hot, caps, d.variants, c.input.VariantStates,
			d.roleDemand, total, scaleUp, scaleDown)
		if after > total {
			moved := after - total
			total = after
			hotRank = byRole
			if d.roleDemand == nil {
				hotRank = map[string]float64{domain.RoleBoth: moved}
			}
			c.logger.V(logging.DEFAULT).Info("hot-rank-hold",
				"modelID", c.input.ModelID, "namespace", c.input.Namespace,
				"raisedByRole", hotRank, "totalDemandMoved", moved, "hotReplicas", len(hot),
				"reason", "a replica's busiest engine rank is full: its capacity is kept, and only the other replicas' spare may be released")
		}
	}

	return total, held, hotRank
}
