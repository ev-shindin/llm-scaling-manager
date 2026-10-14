package allocation

// CompositeLimiter groups a slice of Limiter instances under one name so that a
// policy declaring several limiters -- the physical one and every quota entry --
// presents as a single limiter to the engine.
//
// It carries no logic of its own: the constituents are constraint providers, and
// the engine reaches them through Constituents() (see gpuConstraintProviders),
// computing constraints from each, each on the usage basis it asked for, and
// merging them by intersection (mergeConstraints): a decision must fit every
// constituent, which is how min(physical, quota) is enforced. A constituent
// whose usage is not observed, or that fails, is skipped and the rest still
// bound (ComputeAvailableConstraints).
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
