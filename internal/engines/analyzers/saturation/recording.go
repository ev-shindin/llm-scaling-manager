package saturation

import (
	"context"
	"crypto/sha256"
	"maps"
	"slices"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The analyzer's recorder (docs/proposals/analyzer-learner.md, 2.4). Each
// exported method the engine calls records the call and what it returned,
// and does the unexported method of the same name. The analyzer's own calls
// go to the unexported ones, so a nested call is not recorded again: one
// Analyze is one Analyze event, whatever it calls inside. With recording off
// (a nil recorder) each wrapper is one nil test and the call.

// The kinds of event this analyzer records.
const (
	KindAnalyze              = "saturation.Analyze"
	KindNoteVariantsPresent  = "saturation.NoteVariantsPresent"
	KindEvictStaleHistory    = "saturation.EvictStaleHistory"
	KindForgetVariant        = "saturation.ForgetVariant"
	KindForgottenToken       = "saturation.ForgottenToken"
	KindHoldLowerBounds      = "saturation.HoldLowerBounds"
	KindExportLearnedState   = "saturation.ExportLearnedState"
	KindRestoreLearnedState  = "saturation.RestoreLearnedState"
	KindTakeRestoreOutcomes  = "saturation.TakeRestoreOutcomes"
	KindForgetLearnedState   = "saturation.ForgetLearnedState"
	KindStableAcceleratorFor = "saturation.StableAcceleratorFor"
	// KindPublishLine and KindBorrowLine are made inside Analyze when one
	// variant's ITL line is published for, or borrowed by, another: a check
	// on the replay, which reproduces them by replaying Analyze, never
	// applies them.
	KindPublishLine = "saturation.publishLine"
	KindBorrowLine  = "saturation.borrowLine"
)

// AnalyzeEvent is one Analyze: its input, each variant's capacity record as
// Analyze found it, and what it returned. Written whether or not it erred.
type AnalyzeEvent struct {
	// Input is a learnrec.Input: the rows raw, as Analyze received them.
	Input learnrec.Raw
	// Records is each variant's capacity.Record before Analyze read it,
	// keyed by variant name, null where the store had none: a
	// map[string]*capacity.Record. Every variant in the input's states and
	// rows is named.
	Records learnrec.Raw
	// Result is the result without AnalyzedAt; Error is the error, if any.
	Result *domain.AnalyzerResult
	Error  string
}

// PresentEvent is NoteVariantsPresent's arguments.
type PresentEvent struct {
	Namespace, ModelID string
	Variants           []string
}

// ForgetVariantEvent is ForgetVariant's arguments and how many entries it
// dropped.
type ForgetVariantEvent struct {
	Namespace, ModelID, Variant, Token string
	Dropped                            int
}

// ForgottenTokenEvent is ForgottenToken's arguments and its answer. Recorded
// because reading the token refreshes the mark's liveness stamp.
type ForgottenTokenEvent struct {
	Namespace, Variant string
	Token              string
}

// HoldEvent is HoldLowerBounds' flag.
type HoldEvent struct {
	Held bool
}

// RestoreEvent is the document RestoreLearnedState received and its report.
type RestoreEvent struct {
	State  learnrec.Raw // the LearnedState as received
	Report RestoreReport
}

// AcceleratorEvent is StableAcceleratorFor's arguments and its answer.
type AcceleratorEvent struct {
	Namespace, Variant, Accelerator string
	Answer                          string
}

// LineEvent is one ITL line published for an engine configuration, or
// borrowed by a variant from the variant named in From.
type LineEvent struct {
	PhysicsKey, Variant, From string
	A, B                      float64
}

// WithRecorder records the analyzer's calls to rec; nil turns recording off.
// Call it before the analyzer is used.
func (a *SaturationAnalyzer) WithRecorder(rec *learnrec.Recorder) *SaturationAnalyzer {
	a.rec, a.lastExport = rec, [32]byte{}
	return a
}

// SetSwitchesForReplay sets the package's replay switches by name: the test
// seam a replay drives one named change through (analyzer-learner.md, 2.5).
// The switches are SwitchK2ReadsSettle, SwitchK2WaitsAfterRestart,
// SwitchMuRepeatIsUse, SwitchMuUnknownAgeSafe, SwitchMuLatchRestoresSafe,
// SwitchMuWeighsFleet, SwitchMuYieldCappedAtMeasurement, SwitchMuUnreadAtTarget,
// SwitchMuMinorityFigureExpires, SwitchIdleBand, SwitchK2KeyBucketHeld,
// SwitchBatchCapDeclared,
// SwitchBatchCapLearned, SwitchMuKeyBucketHeld, SwitchMuSwapFreeze,
// SwitchMuMarginClamp, SwitchK2FirstReadingWaits, SwitchBatchCapITLAdmission,
// SwitchBatchCapDecodeView, SwitchBatchCapDerivedMu, SwitchITLPerToken,
// SwitchKVPerRequestOutputDispersion, SwitchBatchCapZeroReplica,
// SwitchITLReadsSettle, SwitchITLWaitsAfterRestart,
// SwitchITLKeepsSettleThroughMissingRow, SwitchITLReadsFinitePodMean,
// SwitchITLRestartUnderName, SwitchITLIdleOnEvidence,
// SwitchKVPerRequestHitsResident, SwitchZeroReplicaDataParallel,
// SwitchITLFitsAggregated, SwitchITLAggregatedShapeTerm,
// SwitchITLAggregatedSizes, SwitchPrefillCapacityObserve,
// SwitchStartConfirmsByDistinctPod, SwitchPrefillBusyMinutes,
// SwitchPrefillAQuadratic, SwitchITLOneSidedAtCap and
// SwitchQueuePricedAsDrain; a name it does not
// know is an error, and sets nothing. Never called on a production path;
// a test holds that.
func (a *SaturationAnalyzer) SetSwitchesForReplay(switches map[string]bool) error {
	rest := maps.Clone(switches)
	fields := a.switchFields()
	for name := range fields {
		delete(rest, name)
	}
	if err := learnrec.UnknownSwitches("saturation", rest); err != nil {
		return err
	}
	for name, on := range switches {
		*fields[name] = on
	}
	return nil
}

// switchFields is each replay switch by name, and the field it sets.
func (a *SaturationAnalyzer) switchFields() map[string]*bool {
	return map[string]*bool{
		SwitchK2ReadsSettle:              &a.k2ReadsSettle,
		SwitchK2WaitsAfterRestart:        &a.k2WaitsAfterRestart,
		SwitchK2WaitsAfterRollout:        &a.k2WaitsAfterRollout,
		SwitchMuRepeatIsUse:              &a.muRepeatIsUse,
		SwitchMuUnknownAgeSafe:           &a.muUnknownAgeSafe,
		SwitchMuLatchRestoresSafe:        &a.muLatchRestoresSafe,
		SwitchMuWeighsFleet:              &a.muWeighsFleet,
		SwitchMuYieldCappedAtMeasurement: &a.muYieldCapped,
		SwitchMuUnreadAtTarget:           &a.muUnreadAtTarget,
		SwitchMuMinorityFigureExpires:    &a.muMinorityExpires,
		SwitchIdleBand:                   &a.idleBand,
		SwitchK2KeyBucketHeld:            &a.k2KeyBucketHeld,
		SwitchBatchCapDeclared:           &a.batchCapDeclared,
		SwitchBatchCapLearned:            &a.batchCapLearned,
		SwitchMuKeyBucketHeld:            &a.muKeyBucketHeld,
		SwitchMuSwapFreeze:               &a.muSwapFreeze,
		SwitchMuMarginClamp:              &a.muMarginClamp,
		SwitchK2FirstReadingWaits:        &a.k2FirstReadingWaits,
		SwitchBatchCapITLAdmission:       &a.batchCapITLAdmission,
		SwitchBatchCapDecodeView:         &a.batchCapDecodeView,
		SwitchBatchCapDerivedMu:          &a.batchCapDerivedMu,
		SwitchITLPerToken:                &a.itlPerToken,

		// #495: mu on learn's list of learners (mu_learner.go).
		SwitchMuForgetReported: &a.muForgetReported,

		// #495: the ITL baseline's last-admitted stamp in the snapshot
		// (learned_snapshot.go).
		SwitchITLBaselineStamped: &a.itlBaselineStamped,

		// Learner PR 5b: the start guard opens on distinct Pods
		// (start_est.go).
		SwitchStartConfirmsByDistinctPod: &a.startConfirmsByPod,

		// Learner PR 5c: the seed is the prior, and the first samples blend
		// with it (start_est.go).
		SwitchStartReadsSeed:          &a.startReadsSeed,
		SwitchStartBlendsFirstSamples: &a.startBlends,

		// Learner PR 5d: counted Pods survive a restart (start_est.go).
		SwitchStartCountedPodsRestored: &a.startCountsRestored,

		// #276: the spread of output lengths in the KV per request.
		SwitchKVPerRequestOutputDispersion: &a.kvOutputDispersion,

		// #427: the zero-replica estimate reads the declared batch cap
		// where the flags state none.
		SwitchBatchCapZeroReplica: &a.batchCapZeroReplica,

		// Learner PR 4a: the ITL line on the settle tracker (itl_settle.go).
		SwitchITLReadsSettle:                  &a.itlReadsSettle,
		SwitchITLWaitsAfterRestart:            &a.itlWaitsAfterRestart,
		SwitchITLKeepsSettleThroughMissingRow: &a.itlKeepsSettle,
		SwitchITLReadsFinitePodMean:           &a.itlFiniteOnly,
		SwitchITLRestartUnderName:             &a.itlRestartUnderName,
		SwitchITLIdleOnEvidence:               &a.itlIdleOnEvidence,

		// #277: a prefix-cache hit stays resident.
		SwitchKVPerRequestHitsResident: &a.kvHitsResident,

		// #438: a data-parallel variant at zero replicas is priced at
		// every engine rank its pod runs.
		SwitchZeroReplicaDataParallel: &a.zeroReplicaDP,

		// #432: an aggregated variant fits an ITL line.
		SwitchITLFitsAggregated: &a.itlFitsAggregated,

		// #275: an aggregated line priced by request shape, and sizing
		// from it.
		SwitchITLAggregatedShapeTerm: &a.itlAggShapeTerm,
		SwitchITLAggregatedSizes:     &a.itlAggSizes,

		// P1: prefill capacity observed and logged (prefill_observe.go).
		SwitchPrefillCapacityObserve: &a.prefillObserve,

		// P1's busy minutes: the rows' queue minimum read into the
		// observation (prefill_observe.go).
		SwitchPrefillBusyMinutes: &a.prefillBusyMinutes,

		// a's curved fit: the quadratic where the prompt clusters carry it
		// (prefill/fixed.go).
		SwitchPrefillAQuadratic: &a.prefillAQuadratic,
		// Issue #467: the ITL line priced one-sided at the sequence cap
		// while B is unlearned (itl_one_sided.go).
		SwitchITLOneSidedAtCap: &a.itlOneSidedAtCap,

		// The ITL line's tier latch (issue #495).
		SwitchITLTierHysteresis: &a.itlTierHysteresis,
		// The window across a rollout (itl_tier_hysteresis.go).
		SwitchITLClearsOnNewRevision: &a.itlClearsOnRevision,
		// Issue #495: the variants follow a role the throughput floor
		// raises (throughput_floor.go).
		SwitchThroughputFloorRaisesVariants: &a.floorRaisesVariants,
		// Issue #520: queued work priced by drain time before mu, and
		// while the backlog grows (queue_drain.go).
		SwitchQueuePricedAsDrain: &a.queuePricedAsDrain,
	}
}

// ReplaySwitchNames is every name SetSwitchesForReplay accepts, sorted: the
// switch runs hold each to a row (analyzer-learner.md, 2.5). A test seam,
// like SetSwitchesForReplay; nothing on a production path calls it, and a
// test holds that.
func ReplaySwitchNames() []string {
	return slices.Sorted(maps.Keys((&SaturationAnalyzer{}).switchFields()))
}

// Analyze computes capacity signals for a model across all its variants
// (analyze), and records the call.
func (a *SaturationAnalyzer) Analyze(ctx context.Context, input domain.AnalyzerInput) (*domain.AnalyzerResult, error) {
	if !a.rec.Active() {
		return a.analyze(ctx, input)
	}
	now := a.now()
	in := a.rec.Fragment(learnrec.InputOf(input))
	records := a.rec.Fragment(capacity.RecordsOf(a.capacityStore, input.Namespace, input.ModelID, learnrec.VariantNames(input)))
	res, err := a.analyze(ctx, input)
	a.rec.Record(KindAnalyze, now, AnalyzeEvent{Input: in, Records: records,
		Result: learnrec.ResultOf(res), Error: learnrec.ErrorOf(err)})
	return res, err
}

// NoteVariantsPresent refreshes the liveness stamp of every variant named
// (noteVariantsPresent), and records the call.
func (a *SaturationAnalyzer) NoteVariantsPresent(namespace, modelID string, variantNames []string) {
	if a.rec.Active() {
		a.rec.Record(KindNoteVariantsPresent, a.now(), PresentEvent{Namespace: namespace, ModelID: modelID, Variants: variantNames})
	}
	a.noteVariantsPresent(namespace, modelID, variantNames)
}

// EvictStaleHistory removes learned state past its horizon
// (evictStaleHistory), and records the call with what it removed.
func (a *SaturationAnalyzer) EvictStaleHistory() Evicted {
	if !a.rec.Active() {
		return a.evictStaleHistory()
	}
	now := a.now()
	ev := a.evictStaleHistory()
	a.rec.Record(KindEvictStaleHistory, now, ev)
	return ev
}

// ForgetVariant drops everything learned for one variant (forgetVariant),
// and records the call.
func (a *SaturationAnalyzer) ForgetVariant(namespace, modelID, variant, token string) int {
	if !a.rec.Active() {
		return a.forgetVariant(namespace, modelID, variant, token)
	}
	now := a.now()
	n := a.forgetVariant(namespace, modelID, variant, token)
	a.rec.Record(KindForgetVariant, now, ForgetVariantEvent{Namespace: namespace, ModelID: modelID,
		Variant: variant, Token: token, Dropped: n})
	return n
}

// ForgottenToken is the last forget token acted on for a variant
// (forgottenToken), and records the call: reading it refreshes the mark.
func (a *SaturationAnalyzer) ForgottenToken(namespace, variant string) string {
	if !a.rec.Active() {
		return a.forgottenToken(namespace, variant)
	}
	now := a.now()
	tok := a.forgottenToken(namespace, variant)
	a.rec.Record(KindForgottenToken, now, ForgottenTokenEvent{Namespace: namespace, Variant: variant, Token: tok})
	return tok
}

// HoldLowerBounds holds every lower bound as it stands while held is true
// (holdLowerBounds), and records the call.
func (a *SaturationAnalyzer) HoldLowerBounds(held bool) {
	if a.rec.Active() {
		a.rec.Record(KindHoldLowerBounds, a.now(), HoldEvent{Held: held})
	}
	a.holdLowerBounds(held)
}

// ExportLearnedState copies what the analyzer has learned
// (exportLearnedState), and records the document when it differs from the
// last one recorded. The engine exports every cycle and the document is the
// largest event there is; an export changes nothing, so an unchanged one is
// left out and a replay, which skips it the same way, loses nothing.
func (a *SaturationAnalyzer) ExportLearnedState() LearnedState {
	if !a.rec.Active() {
		return a.exportLearnedState()
	}
	now := a.now()
	s := a.exportLearnedState()
	doc := a.rec.Fragment(s)
	digest := sha256.Sum256(doc)
	a.mu.Lock()
	changed := doc != nil && digest != a.lastExport
	if changed {
		a.lastExport = digest
	}
	a.mu.Unlock()
	if changed {
		a.rec.Record(KindExportLearnedState, now, doc)
	}
	return s
}

// RestoreLearnedState takes a stored state in (restoreLearnedState), and
// records the document as it was received, with the report.
func (a *SaturationAnalyzer) RestoreLearnedState(s LearnedState) RestoreReport {
	if !a.rec.Active() {
		return a.restoreLearnedState(s)
	}
	now := a.now()
	doc := a.rec.Fragment(s)
	rep := a.restoreLearnedState(s)
	a.rec.Record(KindRestoreLearnedState, now, RestoreEvent{State: doc, Report: rep})
	return rep
}

// TakeRestoreOutcomes returns and resets the restore outcome counts
// (takeRestoreOutcomes), and records the call.
func (a *SaturationAnalyzer) TakeRestoreOutcomes() map[string]int {
	if !a.rec.Active() {
		return a.takeRestoreOutcomes()
	}
	now := a.now()
	out := a.takeRestoreOutcomes()
	a.rec.Record(KindTakeRestoreOutcomes, now, out)
	return out
}

// ForgetLearnedState drops everything learned and everything pending
// (forgetLearnedState), and records the call.
func (a *SaturationAnalyzer) ForgetLearnedState() {
	if a.rec.Active() {
		a.rec.Record(KindForgetLearnedState, a.now(), struct{}{})
	}
	a.forgetLearnedState()
}

// StableAcceleratorFor reports the accelerator a variant's learned state is
// keyed under (stableAcceleratorFor), and records the call with its answer.
func (a *SaturationAnalyzer) StableAcceleratorFor(namespace, variantName, accelerator string) string {
	if !a.rec.Active() {
		return a.stableAcceleratorFor(namespace, variantName, accelerator)
	}
	now := a.now()
	ans := a.stableAcceleratorFor(namespace, variantName, accelerator)
	a.rec.Record(KindStableAcceleratorFor, now, AcceleratorEvent{Namespace: namespace, Variant: variantName,
		Accelerator: accelerator, Answer: ans})
	return ans
}
