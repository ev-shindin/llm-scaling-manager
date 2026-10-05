/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package aggregation provides pure helper functions for aggregating
// per-variant capacity data into model-level and per-role totals. The helpers
// are split along the analyzer/engine contract:
//
// Analyzers own demand. They use SumTotalDemand and DemandByRole (plus
// IsDisaggregated to decide whether a per-role breakdown applies) to populate
// AnalyzerResult.TotalDemand and AnalyzerResult.RoleDemand.
//
// The engine's capacity-build step owns supply. It uses SumTotalSupply,
// SumTotalAnticipatedSupply, and AggregateByRole to derive
// AnalyzerResult.Total*/Utilization and to assemble
// AnalyzerResult.RoleCapacities from the analyzer's RoleDemand.
//
// Deriving both halves from the same VariantCapacities is what makes the
// linearity invariant required by the optimizer's per-variant scaling math hold
// by construction:
//
//	r.TotalSupply            == Σ_v vc.ReplicaCount × vc.PerReplicaCapacity
//	r.TotalAnticipatedSupply == Σ_v (vc.ReplicaCount + startingReplicas(vc)) × vc.PerReplicaCapacity
//	   where startingReplicas(vc) == vc.PendingReplicas - vc.StuckReplicas
//	r.TotalDemand            == Σ_v vc.TotalDemand
//	r.RoleCapacities[role].* == same sums filtered by vc.Role == role
package aggregation

import (
	"math"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

// ScopeTotals holds the three model-level (or per-role) aggregates that the
// engine's universal threshold post-step reads to compute RC and SC.
type ScopeTotals struct {
	TotalSupply            float64
	TotalAnticipatedSupply float64
	TotalDemand            float64
}

// perReplica is vc.PerReplicaCapacity as a sum may carry it: a non-finite
// capacity counts as none.
//
// Every total here is multiplied by a replica count and then compared with `>`
// or `<` by its consumers, and a NaN makes every such comparison false -- so one
// variant with a NaN capacity does not merely corrupt its own term, it disables
// the guard the consumer applies to the whole sum. The engine's
// `if rc < 0 { rc = 0 }` and the floor's two caps all failed open that way.
//
// Written `!(p > 0)` rather than `p <= 0` because a NaN fails every comparison,
// so `p <= 0` would admit one; +Inf is excluded separately because it passes
// `p > 0`. A capacity of zero is already worth zero in the product, so treating
// a non-finite one the same way changes nothing for any finite input.
//
// Only the capacity is sanitized. The replica counts are left alone, so a
// negative PendingReplicas still yields a negative supply and its consumers'
// existing clamps still see it: that case is real, guarded downstream, and not
// this function's to reinterpret.
func perReplica(vc domain.VariantCapacity) float64 {
	p := vc.PerReplicaCapacity
	if !(p > 0) || math.IsInf(p, 1) {
		return 0
	}
	return p
}

// SumTotalSupply returns Σ_v vc.ReplicaCount × vc.PerReplicaCapacity.
func SumTotalSupply(vcs []domain.VariantCapacity) float64 {
	var total float64
	for _, vc := range vcs {
		total += float64(vc.ReplicaCount) * perReplica(vc)
	}
	return total
}

// SumTotalAnticipatedSupply returns
// Σ_v (vc.ReplicaCount + startingReplicas(vc)) × vc.PerReplicaCapacity.
// Replicas on their way count toward anticipated supply so an in-flight
// scale-up reduces the computed RC and prevents double-scaling.
func SumTotalAnticipatedSupply(vcs []domain.VariantCapacity) float64 {
	var total float64
	for _, vc := range vcs {
		total += AnticipatedSupply(vc)
	}
	return total
}

// startingReplicas is how many of a variant's replicas are actually on their way
// to serving, which is not the same as how many are not serving now.
//
// PendingReplicas is CurrentReplicas less the ready ones, so it counts a Pod
// stuck on an image pull, in a crash loop, or unschedulable on GPU quota. Those
// are what anticipated supply is subtracted for -- the engine reads them as
// capacity arriving and withholds the scale-up that would clear the queue, and
// since they never become Ready they never stop withholding it.
//
// Only trusted when the Pod listing behind it SUCCEEDED. An empty list otherwise
// reads as "nothing is starting", which would double-order a fleet that is
// already scaling up, so an unknown count falls back to PendingReplicas.
// A negative PendingReplicas is still passed through: that is this package's
// existing contract (aggregation_nonfinite_test.go, "the negative survives, for
// the downstream clamp to see"), and nonNegativeSupply is where that judgement
// belongs.
//
// The SUBTRACTION is clamped, which is a different thing. nonNegativeSupply
// acts on the already-summed role figure, so a negative term from one variant
// cancels a sibling's positive one before any clamp can see it -- and that
// cancellation is unrecoverable, because the sum has already happened. It is
// also reachable without a negative PendingReplicas at all: PendingReplicas is
// max(0, CurrentReplicas-ownReplicas) while StuckReplicas counts Pods, so a
// replica whose metrics row is still cached after it crashed is counted once as
// supply and once as a negative arrival, putting anticipated supply BELOW
// supply. Clamping the subtraction leaves each variant's own term at worst
// zero, which is the most a stuck Pod can honestly say.
func startingReplicas(vc domain.VariantCapacity) int {
	if vc.StuckReplicas <= 0 {
		return vc.PendingReplicas
	}
	if n := vc.PendingReplicas - vc.StuckReplicas; n > 0 {
		return n
	}
	return 0
}

// AnticipatedSupply is one variant's ready-plus-starting capacity: exactly the
// term AggregateByRole sums into ScopeTotals.TotalAnticipatedSupply.
//
// Exported so that a caller computing a variant's SHARE of that total uses the
// same rule for the numerator as the aggregate used for the denominator.
// holdPrefillDemand did not, and its shares summed to more than 1 -- over-
// distributing the held demand -- the moment any replica was stuck.
func AnticipatedSupply(vc domain.VariantCapacity) float64 {
	return float64(vc.ReplicaCount+startingReplicas(vc)) * perReplica(vc)
}

// DemandByRole groups vcs by role and sums each group's TotalDemand. It is the
// demand-only projection of AggregateByRole: analyzers own demand attribution
// and emit it as AnalyzerResult.RoleDemand, while per-role supply is recomputed
// by the engine's capacity-build step. The returned map's keys are also the set
// of roles present in vcs, which analyzers use to decide how to distribute
// demand. Role canonicalization therefore matches AggregateByRole by
// construction: an empty role is treated as domain.RoleBoth.
func DemandByRole(vcs []domain.VariantCapacity) map[string]float64 {
	totals := AggregateByRole(vcs)
	result := make(map[string]float64, len(totals))
	for role, t := range totals {
		result[role] = t.TotalDemand
	}
	return result
}

// IsDisaggregated reports whether vcs describes a P/D-disaggregated model, i.e.
// whether any variant serves a role other than domain.RoleBoth. An empty role is
// canonicalized to domain.RoleBoth, so a fleet of only empty/"both" variants is
// not disaggregated and its analyzer emits model-level demand with no per-role
// breakdown.
func IsDisaggregated(vcs []domain.VariantCapacity) bool {
	for _, vc := range vcs {
		if vc.Role != "" && vc.Role != domain.RoleBoth {
			return true
		}
	}
	return false
}

// SumTotalDemand returns Σ_v vc.TotalDemand.
func SumTotalDemand(vcs []domain.VariantCapacity) float64 {
	var total float64
	for _, vc := range vcs {
		total += vc.TotalDemand
	}
	return total
}

// AggregateByRole groups vcs by role and computes ScopeTotals for each group.
// An empty or blank role string is canonicalized to domain.RoleBoth,
// consistent with saturation's role normalization.
func AggregateByRole(vcs []domain.VariantCapacity) map[string]ScopeTotals {
	result := make(map[string]ScopeTotals)
	for _, vc := range vcs {
		role := vc.Role
		if role == "" {
			role = domain.RoleBoth
		}
		t := result[role]
		t.TotalSupply += float64(vc.ReplicaCount) * perReplica(vc)
		t.TotalAnticipatedSupply += AnticipatedSupply(vc)
		t.TotalDemand += vc.TotalDemand
		result[role] = t
	}
	return result
}
