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

// Package annotations defines the annotations WVA reads or writes: one on the
// in-memory VariantAutoscaling objects it synthesizes, and one an operator
// sets on a scale target to ask for its learned state to be forgotten.
//
// It used to hold the discovery schema — llm-d.ai/managed, model-id and
// variant-cost, read off ScaledObjects and HPAs. That is gone: WVA no longer
// looks for the workloads it manages, so there is nothing for an opt-in
// annotation to opt into.
package annotations

import (
	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The llm-d.ai/managed, llm-d.ai/model-id and llm-d.ai/variant-cost annotations
// are gone. WVA no longer looks for the workloads it manages, so there is nothing
// for an opt-in annotation to opt into: KEDA only calls a scaler its trigger
// names, and being called is what being managed means. The configuration those
// annotations carried now lives in the trigger's metadata, which KEDA delivers on
// every call — see internal/registry and
// docs/plans/engine/keda-driven-discovery.md.
const (
	// Synthetic marks an in-memory VariantAutoscaling as one WVA synthesized
	// rather than read. Objects carrying this annotation are never written to the
	// Kubernetes API server; they exist only within the WVA optimization pipeline.
	Synthetic = "llm-d.ai/synthetic"

	// enabledValue is the annotation value that turns Synthetic on.
	enabledValue = "true"

	// ForgetLearnedState, set on a variant's scale target (the Deployment or
	// LeaderWorkerSet itself, not its pod template, so nothing rolls), asks
	// the scaling manager to forget what it has learned about that variant.
	// The value is a token, acted on once: change it to ask again. Any
	// non-empty value works; a timestamp is a convenient one.
	ForgetLearnedState = "llm-d.ai/forget-learned-state"

	// MaxNumSeqs, set on a variant's scale target (the Deployment or
	// LeaderWorkerSet itself, not its pod template, so nothing rolls), states
	// one engine rank's batch cap -- vLLM's max_num_seqs, SGLang's
	// max_running_requests -- for an engine whose flags the scaling manager
	// cannot read: a launcher script, or the value passed as a variable
	// nothing resolves. A positive integer; anything else is ignored. A cap
	// the engine's flags state wins over it.
	MaxNumSeqs = "llm-d.ai/max-num-seqs"
)

// IsSynthetic reports whether va was synthesized by WVA rather than read from a
// VariantAutoscaling CRD instance. Synthetic VAs exist only in memory and must
// never be written to the Kubernetes API server.
//
// Every variant is synthetic today — they are built from the registry, i.e. from
// what KEDA has called WVA about. The predicate remains as the guard that keeps
// an in-memory object from reaching the API server.
func IsSynthetic(va *wvav1alpha1.VariantAutoscaling) bool {
	return va.GetAnnotations()[Synthetic] == enabledValue
}
