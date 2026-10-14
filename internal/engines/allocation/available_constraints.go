package allocation

import (
	"context"
	"fmt"
)

// SkippedProvider names a constraint provider that was left out of a decision,
// and why.
type SkippedProvider struct {
	Name   string
	Reason string
	// Basis is the usage basis the provider asks for, and Unobserved is true
	// when it was skipped because that basis had no observation (rather than
	// because it failed), so a caller can say why the view is missing.
	Basis      UsageBasis
	Unobserved bool
}

// String renders the skip for logs as "<name>: <reason>".
func (s SkippedProvider) String() string {
	return s.Name + ": " + s.Reason
}

// ComputeAvailableConstraints computes constraints from every provider that can
// supply them this cycle, and names the ones that could not.
//
// A provider is skipped when the usage basis it asked for has not been observed
// (views.Has) -- it is never called then, since a provider handed a nil view
// reports its whole capacity free -- or when ComputeConstraints errors. The
// survivors still bound the decision.
//
// Skipping one is safe because the constraints are intersected
// (mergeConstraints): a decision must fit every provider, so leaving one out can
// only make the answer more permissive, never deny what the full set allows. It
// used to be the other way round. Both engines dropped EVERY constraint when any
// one view was missing or any one provider failed, so a physical limiter whose
// node usage had not been observed yet lifted the quota beside it too, and a
// quota whose Kueue read failed lifted the physical bound. Only when every
// provider is unavailable does the caller have nothing to check against.
//
// observe, when non-nil, is called for each provider that produced constraints,
// with the usage it was fed, so a caller can log what each one decided.
// Constraints are stamped with the views' pools version.
func ComputeAvailableConstraints(
	ctx context.Context,
	providers []ConstraintProvider,
	views GPUUsageViews,
	observe func(cp ConstraintProvider, c *ResourceConstraints, usageByType map[string]int, usageByNS map[string]map[string]int),
) ([]*ResourceConstraints, []SkippedProvider) {
	var constraints []*ResourceConstraints
	var skipped []SkippedProvider
	for _, cp := range providers {
		basis := UsageBasisOf(cp)
		if !views.Has(basis) {
			skipped = append(skipped, SkippedProvider{
				Name:       cp.Name(),
				Reason:     fmt.Sprintf("no %s GPU-usage observation yet", basis),
				Basis:      basis,
				Unobserved: true,
			})
			continue
		}
		usageByType, usageByNS := views.For(cp)
		c, err := cp.ComputeConstraints(ctx, usageByType, usageByNS)
		if err != nil {
			skipped = append(skipped, SkippedProvider{
				Name:   cp.Name(),
				Reason: fmt.Sprintf("could not compute constraints: %v", err),
				Basis:  basis,
			})
			continue
		}
		if c == nil {
			skipped = append(skipped, SkippedProvider{Name: cp.Name(), Reason: "returned no constraints", Basis: basis})
			continue
		}
		constraints = append(constraints, views.Stamp(c))
		if observe != nil {
			observe(cp, c, usageByType, usageByNS)
		}
	}
	return constraints, skipped
}
