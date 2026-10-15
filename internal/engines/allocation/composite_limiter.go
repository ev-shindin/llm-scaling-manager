package allocation

// CompositeLimiter groups a slice of Limiter instances under one name so that a
// policy declaring several limiters -- the physical one and every quota entry --
// presents as a single limiter to the engine.
//
// It carries no logic of its own: the constituents are constraint providers, and
// the engine reaches them through Constituents() (see gpuConstraintProviders),
// computing constraints from each and merging them. The most-restrictive cap
// wins at merge time, in the optimizer.
//
// CompositeLimiter is the minimal wiring needed to enable an operator to
// deploy a quota config that declares both cluster-scope and namespace-scope
// entries before the full limiter chain (sub-issue #1003) lands. The chain
// will replace it with explicit ordering and physical-bound composition
// (min(physical, quota)) within a single ComputeConstraints.
type CompositeLimiter struct {
	name     string
	limiters []Limiter
}

// NewCompositeLimiter wraps a slice of constituent limiters. The name is used
// in logs. Constituents are not copied; the caller retains ownership.
func NewCompositeLimiter(name string, constituents []Limiter) *CompositeLimiter {
	return &CompositeLimiter{
		name:     name,
		limiters: constituents,
	}
}

// Name returns the composite limiter identifier.
func (c *CompositeLimiter) Name() string {
	return c.name
}

// Constituents returns the wrapped limiters in order. Exposed for tests and
// observability; callers must not mutate the returned slice.
func (c *CompositeLimiter) Constituents() []Limiter {
	return c.limiters
}

// Ensure CompositeLimiter implements Limiter.
var _ Limiter = (*CompositeLimiter)(nil)
