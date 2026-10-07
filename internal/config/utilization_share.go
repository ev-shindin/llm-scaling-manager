package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// OptimizerTypeUtilizationShare selects the utilization-share optimizer: the
// budget of each enabled group is shared as headroom above every role's need,
// by weight. See docs/proposals/utilization-share-optimizer.md.
const OptimizerTypeUtilizationShare = "utilizationShare"

// DefaultUtilizationShareTolerance is the band's relative half-width in GPUs,
// as a fraction of a role's target (§6.1 of the proposal).
const DefaultUtilizationShareTolerance = 0.15

// DefaultWeightClasses are the weight classes users pick from when the policy
// declares none. "standard", at weight 1, is the default class.
var DefaultWeightClasses = map[string]float64{
	"best-effort": 0.5,
	"standard":    1,
	"important":   2,
	"critical":    4,
}

// OptimizerConfig selects and configures the fleet's optimizer. Like Limiters,
// it is a budget-scope setting: it is read only from the cluster policy's
// "default" entry (see Config.UtilizationShare) and never merged from a
// per-model or namespace-local entry.
//
// It is deliberately NOT checked by ScalingPolicy.Validate. A malformed entry is
// rejected whole, and rejecting the "default" entry would drop its limiters with
// it, leaving the fleet unbounded. The optimizer block is validated on its own
// instead, and an invalid one only disables the optimizer.
type OptimizerConfig struct {
	// Type selects the optimizer. Empty keeps today's selection.
	Type string `yaml:"type,omitempty"`

	// UtilizationShare configures the utilization-share optimizer. Every field
	// is optional.
	UtilizationShare *UtilizationShareConfig `yaml:"utilizationShare,omitempty"`

	// decodeErr records a block that did not decode. It is kept instead of
	// returned, so a typo in the optimizer block cannot fail the whole
	// "default" entry -- and drop its limiters with it.
	decodeErr error
}

// UnmarshalYAML decodes the block strictly (unknown keys are errors, so a
// misspelled key is reported rather than silently ignored), but never fails the
// enclosing entry: any error is recorded and surfaces from
// Config.UtilizationShare, which then keeps today's optimizer.
func (o *OptimizerConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain OptimizerConfig
	var p plain
	raw, err := yaml.Marshal(node)
	if err == nil {
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		err = dec.Decode(&p)
	}
	if err != nil {
		*o = OptimizerConfig{decodeErr: fmt.Errorf("optimizer: %w", err)}
		return nil
	}
	*o = OptimizerConfig(p)
	return nil
}

// UtilizationShareConfig is the operator-facing configuration of the
// utilization-share optimizer: six optional keys, each a decision only an
// operator can make. Every other value the optimizer uses is derived from the
// cluster or is a constant (§8.4 of the proposal).
type UtilizationShareConfig struct {
	// Tolerance is the "±threshold percent": how far a role's GPUs may be from
	// its target, as a fraction of the target, before a move is considered.
	// Zero takes DefaultUtilizationShareTolerance.
	Tolerance float64 `yaml:"tolerance,omitempty"`

	// ReserveGPUs are held out of the budget so a scale-from-zero wake need not
	// wait for a transfer.
	ReserveGPUs int `yaml:"reserveGPUs,omitempty"`

	// Shadow computes, reports and logs everything and actuates nothing.
	Shadow bool `yaml:"shadow,omitempty"`

	// PhysicalGroups also enables the optimizer on groups bounded only by the
	// physical GPU inventory, where spending the whole budget means holding
	// every GPU of a type.
	PhysicalGroups bool `yaml:"physicalGroups,omitempty"`

	// WeightClasses maps class names to weights. Empty takes
	// DefaultWeightClasses. Exactly one class must have weight 1; it is the
	// default class.
	WeightClasses map[string]float64 `yaml:"weightClasses,omitempty"`

	// Namespaces disables the optimizer for individual namespace-quota groups.
	Namespaces map[string]UtilizationShareNamespace `yaml:"namespaces,omitempty"`
}

// UtilizationShareNamespace is the per-namespace-quota-group setting.
type UtilizationShareNamespace struct {
	// Enabled false keeps this namespace's quota group on today's optimizer.
	// Nil means enabled.
	Enabled *bool `yaml:"enabled,omitempty"`
}

// UtilizationShare is the resolved, validated configuration the optimizer
// runs with. Build it with ResolveUtilizationShare.
type UtilizationShare struct {
	Tolerance      float64
	ReserveGPUs    int
	Shadow         bool
	PhysicalGroups bool
	// Classes are the weight classes in force.
	Classes map[string]float64
	// DefaultClass is the class of weight 1.
	DefaultClass string
	// MinWeight and MaxWeight bound every weight: the range the classes span.
	MinWeight, MaxWeight float64

	disabled map[string]bool
}

// ResolveUtilizationShare validates cfg and returns the settings in force.
// A nil cfg resolves to every default.
func ResolveUtilizationShare(cfg *UtilizationShareConfig) (UtilizationShare, error) {
	if cfg == nil {
		cfg = &UtilizationShareConfig{}
	}
	us := UtilizationShare{
		Tolerance:      cfg.Tolerance,
		ReserveGPUs:    cfg.ReserveGPUs,
		Shadow:         cfg.Shadow,
		PhysicalGroups: cfg.PhysicalGroups,
	}
	if us.Tolerance == 0 {
		us.Tolerance = DefaultUtilizationShareTolerance
	}
	// `x <= 0` admits NaN, so every range check is written to reject it.
	if !(us.Tolerance > 0 && us.Tolerance < 1) {
		return UtilizationShare{}, fmt.Errorf("utilizationShare.tolerance must be in (0, 1), got %v", cfg.Tolerance)
	}
	if us.ReserveGPUs < 0 {
		return UtilizationShare{}, fmt.Errorf("utilizationShare.reserveGPUs must be >= 0, got %d", us.ReserveGPUs)
	}

	classes := cfg.WeightClasses
	if len(classes) == 0 {
		classes = DefaultWeightClasses
	}
	us.Classes = maps.Clone(classes)
	us.MinWeight, us.MaxWeight = math.Inf(1), math.Inf(-1)
	var unit []string
	for name, w := range us.Classes {
		if name == "" {
			return UtilizationShare{}, errors.New("utilizationShare.weightClasses: empty class name")
		}
		if !(w > 0) || math.IsInf(w, 0) {
			return UtilizationShare{}, fmt.Errorf("utilizationShare.weightClasses[%q] must be a finite weight > 0, got %v", name, w)
		}
		if w == 1 {
			unit = append(unit, name)
		}
		us.MinWeight = math.Min(us.MinWeight, w)
		us.MaxWeight = math.Max(us.MaxWeight, w)
	}
	if len(unit) != 1 {
		sort.Strings(unit)
		return UtilizationShare{}, fmt.Errorf(
			"utilizationShare.weightClasses must have exactly one class of weight 1 (the default class), got %d %v",
			len(unit), unit)
	}
	us.DefaultClass = unit[0]

	for ns, n := range cfg.Namespaces {
		if n.Enabled != nil && !*n.Enabled {
			if us.disabled == nil {
				us.disabled = make(map[string]bool)
			}
			us.disabled[ns] = true
		}
	}
	return us, nil
}

// EnabledForNamespace reports whether a namespace-quota group in namespace takes
// part. It does not apply to cluster-scope groups.
func (u UtilizationShare) EnabledForNamespace(namespace string) bool {
	return !u.disabled[namespace]
}

// DisabledNamespaces lists the namespaces whose quota groups are disabled, sorted.
func (u UtilizationShare) DisabledNamespaces() []string {
	return slices.Sorted(maps.Keys(u.disabled))
}

// Weight resolves a model's weight from its scaling-policy entry. A class name
// takes the class's weight; a number is clamped into [MinWeight, MaxWeight];
// neither takes the default class.
//
// Every problem with the two fields -- an unknown class, both set, a number
// that is not finite and positive, a value that did not decode -- falls back
// to the default class and is reported through the error, which the caller
// logs. None of them fails the scaling-policy entry: the "default" entry also
// carries the limiters, and a typo in a weight must not drop them (§8.3).
func (u UtilizationShare) Weight(class string, weight ModelWeight) (float64, error) {
	def := u.Classes[u.DefaultClass]
	switch {
	case weight.Err() != nil:
		return def, fmt.Errorf("%w, using %q", weight.Err(), u.DefaultClass)
	case class != "" && weight.IsSet():
		return def, fmt.Errorf("weight and weightClass are mutually exclusive (weight=%v weightClass=%q), using %q",
			weight.Value(), class, u.DefaultClass)
	case class != "":
		if w, ok := u.Classes[class]; ok {
			return w, nil
		}
		return def, fmt.Errorf("unknown weightClass %q, using %q", class, u.DefaultClass)
	case weight.IsSet():
		v := weight.Value()
		if !(v > 0) || math.IsInf(v, 0) {
			return def, fmt.Errorf("weight must be a finite number > 0, got %v, using %q", v, u.DefaultClass)
		}
		return math.Min(math.Max(v, u.MinWeight), u.MaxWeight), nil
	default:
		return def, nil
	}
}

// ModelWeight is a scaling-policy entry's numeric weight. It decodes leniently:
// a value that is not a number is recorded rather than returned, so it cannot
// fail the entry -- and, on the "default" entry, drop the limiters with it.
// UtilizationShare.Weight reports it and falls back to the default class.
type ModelWeight struct {
	value float64
	set   bool
	err   error
}

// NewModelWeight returns a set weight.
func NewModelWeight(v float64) ModelWeight { return ModelWeight{value: v, set: true} }

// Value is the weight as written; meaningful only when IsSet.
func (w ModelWeight) Value() float64 { return w.value }

// IsSet reports whether the entry stated a numeric weight that decoded.
func (w ModelWeight) IsSet() bool { return w.set }

// Err is the decode error of a weight that was written but is not a number.
func (w ModelWeight) Err() error { return w.err }

// IsZero reports whether the entry said nothing about the weight, so yaml's
// omitempty and Merge can tell "unset" from a stated value.
func (w ModelWeight) IsZero() bool { return !w.set && w.err == nil }

// UnmarshalYAML records a non-numeric value instead of failing the entry.
func (w *ModelWeight) UnmarshalYAML(node *yaml.Node) error {
	var v float64
	if err := node.Decode(&v); err != nil {
		*w = ModelWeight{err: fmt.Errorf("weight: %w", err)}
		return nil
	}
	*w = ModelWeight{value: v, set: true}
	return nil
}

// MarshalYAML writes the weight back as a number.
func (w ModelWeight) MarshalYAML() (any, error) {
	if !w.set {
		return nil, nil
	}
	return w.value, nil
}

// UtilizationShare returns the utilization-share settings in force, and whether
// the optimizer is selected at all. It reads the same source as the limiters
// (effectiveLimitersLocked): the separated cluster policy when there is one,
// otherwise the global map's "default" entry, never a namespace-local map.
//
// An invalid block returns selected=false and the error, so the caller keeps
// today's optimizer and reports the field. The limiters are unaffected.
// Thread-safe.
func (c *Config) UtilizationShare() (settings UtilizationShare, selected bool, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var opt *OptimizerConfig
	if c.saturation.clusterPolicy != nil {
		opt = c.saturation.clusterPolicy.Optimizer
	} else {
		opt = c.saturation.global[GlobalDefaultsKey].Optimizer
	}
	if opt == nil {
		return UtilizationShare{}, false, nil
	}
	if opt.decodeErr != nil {
		return UtilizationShare{}, false, opt.decodeErr
	}
	if opt.Type == "" {
		return UtilizationShare{}, false, nil
	}
	if opt.Type != OptimizerTypeUtilizationShare {
		return UtilizationShare{}, false, fmt.Errorf("optimizer.type: unknown optimizer %q (valid: %q)", opt.Type, OptimizerTypeUtilizationShare)
	}
	if len(c.effectiveLimitersLocked()) == 0 {
		return UtilizationShare{}, false, fmt.Errorf("optimizer.type %q needs a limiters: list: there is no budget to share", opt.Type)
	}
	settings, err = ResolveUtilizationShare(opt.UtilizationShare)
	if err != nil {
		return UtilizationShare{}, false, err
	}
	return settings, true, nil
}

// IgnoredOptimizerNamespaces lists the namespaces whose namespace-local
// scaling-policy map declares an optimizer block, sorted. Such a block has no
// effect -- the optimizer is configured only beside the limiters -- and the
// caller reports it so whoever wrote it learns that.
// Thread-safe.
func (c *Config) IgnoredOptimizerNamespaces() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []string
	for ns, set := range c.saturation.namespaceConfigs {
		for _, entry := range set {
			if entry.Optimizer != nil {
				out = append(out, ns)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// NamespaceHasLocalPolicy reports whether namespace has its own scaling-policy
// map. Such a map replaces the global one for that namespace's models, so a
// weight resolved there was written by whoever owns the namespace -- on a
// cluster-scoped install, a tenant. The utilization-share optimizer does not
// honor such a weight in the cluster group, where tenants share one budget
// (proposal §8.2).
// Thread-safe.
func (c *Config) NamespaceHasLocalPolicy(namespace string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.saturation.namespaceConfigs[namespace]) > 0
}

// UtilizationShareWakeScores returns a parked model's score in the
// utilization-share optimizer, -w, for a wake's claim on a releasing transfer
// (docs/proposals/utilization-share-optimizer.md, section 6.3): zNamespace in
// its namespace's own quota group, zCluster in the cluster group. A namespace
// with its own scaling-policy map is not planned in the cluster group (section
// 8.2), so there its wakes claim nothing: zCluster is +Inf. ok is false when
// the optimizer is not acting.
func (c *Config) UtilizationShareWakeScores(namespace, modelID string) (zNamespace, zCluster float64, ok bool) {
	us, selected, err := c.UtilizationShare()
	if err != nil || !selected || us.Shadow {
		return 0, 0, false
	}
	policy := ResolveScalingPolicy(c.ScalingPolicyConfigForNamespace(namespace), modelID, namespace)
	w, _ := us.Weight(policy.WeightClass, policy.Weight) // an invalid weight resolves to the default class
	zNamespace, zCluster = -w, -w
	if c.NamespaceHasLocalPolicy(namespace) {
		zCluster = math.Inf(1)
	}
	return zNamespace, zCluster, true
}
