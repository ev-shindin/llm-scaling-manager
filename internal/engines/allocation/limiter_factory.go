package allocation

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/gpunodes"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// NewLimiterFromConfig constructs every GPU limiter Config.EffectiveLimiterTypes
// selects -- the inline limiters: list on the saturation "default" config.
//
//   - No limiters declared: nothing limits. Returns a NoOpLimiter, which
//     provides no constraints, so the optimizer runs unconstrained and a
//     scale-from-zero wake is published without a capacity check. An operator
//     who declares no limiter gets no limiter — there is no implicit default
//     one, and no enable flag that could leave a declared limiter inert.
//   - LimiterTypeInventory: a TypeInventoryWithUsage wrapped in a DefaultLimiter.
//     Discovers physical GPUs via the GPU operator, and counts every GPU held on
//     the nodes.
//   - LimiterTypeQuota: one DefaultLimiter per Config.EffectiveQuotaEntries
//     entry, each wrapping a QuotaInventory and counting only what the scaling
//     manager's own pods hold. An entry with `kueue.enabled` is additionally
//     bounded by the quotas a kueue.Reader reads from Kueue (min per namespace
//     and accelerator type).
//
// Both kinds are built when both are declared, physical first, and more than
// one limiter is grouped in a CompositeLimiter. The engines compute constraints
// from each constituent and intersect them, so a decision must fit the GPUs that
// exist AND every quota. Until the set was built whole, a quota entry made the
// physical one silently inert.
//
// The kubeClient serves the inventory path (GPU operator discovery) and the
// Kueue reader; the none path and a Kueue-less quota entry ignore it. Inline
// limiter entries are validated at ConfigMap parse time
// (ScalingPolicy.validateLimiters).
func NewLimiterFromConfig(cfg *config.Config, kubeClient client.Client) (Limiter, error) {
	var constituents []Limiter
	types := cfg.EffectiveLimiterTypes()
	physical := types.HasPhysical()
	if physical {
		constituents = append(constituents, newInventoryLimiter(kubeClient, types.HasQuota()))
	}
	if types.HasQuota() {
		quotas, err := newQuotaLimiters(cfg, kubeClient)
		if err != nil {
			return nil, err
		}
		constituents = append(constituents, quotas...)
	}
	switch {
	case len(constituents) == 0:
		return NewNoOpLimiter("no-limiter"), nil
	case len(constituents) == 1:
		return constituents[0], nil
	case physical:
		return NewCompositeLimiter("gpu-and-quota-limiter", constituents), nil
	default:
		return NewCompositeLimiter("quota-limiter", constituents), nil
	}
}

// kueueReaders holds one kueue.Reader per quota entry name, shared by every
// limiter built for that entry. Both engines build their own limiter from the
// same config (cmd/main.go), and a Reader each would mean two full reads of
// Kueue per refresh interval and two snapshots that can disagree for up to one
// interval. A rebuild with different reader options (resources, interval,
// namespace) replaces the entry's Reader; the old one is simply dropped.
var kueueReaders = struct {
	sync.Mutex
	byEntry map[string]*sharedReader
}{byEntry: map[string]*sharedReader{}}

type sharedReader struct {
	key    string
	reader *kueue.Reader
}

// sharedKueueReader returns the Reader for entry, creating or replacing it when
// none exists yet with the same options. One process has one client, so the
// client is not part of the key.
func sharedKueueReader(entry config.QuotaLimiterConfig, kubeClient client.Client, namespace string) *kueue.Reader {
	opts := kueue.Options{
		Resources:       entry.Kueue.Resources,
		RefreshInterval: entry.KueueRefreshInterval(),
		Namespace:       namespace,
	}
	key, _ := json.Marshal(opts)
	kueueReaders.Lock()
	defer kueueReaders.Unlock()
	if s, ok := kueueReaders.byEntry[entry.Name]; ok && s.key == string(key) {
		return s.reader
	}
	r := kueue.NewReader(kubeClient, opts)
	kueueReaders.byEntry[entry.Name] = &sharedReader{key: string(key), reader: r}
	return r
}

// LimiterSignature is a deterministic fingerprint of the config inputs that
// determine the GPU limiter, so an engine can detect when a rebuild is needed
// without rebuilding every cycle: the limiter types and every quota entry.
// Quota entry maps are marshaled with sorted keys by encoding/json, so equal
// configs always produce equal signatures.
//
// The types are part of it because adding or removing a physical entry beside
// quota entries changes what is built while the quota entries stay the same; a
// signature of the quota entries alone kept the old limiter after such an
// edit. The physical entry itself carries nothing that changes the build (it
// takes no fields; its name only labels the summary), so the type covers it.
//
// Shared by every engine that holds a limiter. They must agree on what "the
// config changed" means, or one of them keeps serving a stale limiter after an
// edit — which is exactly what happened while the scale-from-zero engine built
// its limiter once at startup: the ConfigMap documented limiters as applied live,
// and half the system ignored the edit.
func LimiterSignature(cfg *config.Config) string {
	sig, _ := json.Marshal(struct {
		Types  config.LimiterTypes         `json:"types"`
		Quotas []config.QuotaLimiterConfig `json:"quotas"`
	}{cfg.EffectiveLimiterTypes(), cfg.EffectiveQuotaEntries()})
	return string(sig)
}

// newInventoryLimiter builds the physical-capacity GPU limiter: a
// TypeInventoryWithUsage (GPUs discovered via the GPU operator) wrapped in a
// DefaultLimiter. besideQuota says quota limiters are built beside it, which
// changes what a node-read denial publishes (TypeInventory.SetBesideQuota).
func newInventoryLimiter(kubeClient client.Client, besideQuota bool) Limiter {
	gpuDiscovery := gpunodes.NewK8sWithGpuOperator(kubeClient)
	gpuInventory := NewTypeInventoryWithUsage("cluster-gpu-inventory", gpuDiscovery)
	gpuInventory.SetBesideQuota(besideQuota)
	return NewDefaultLimiter(PhysicalLimiterName, gpuInventory)
}

// PhysicalLimiterName is the name the physical limiter is built under: the
// provider label it carries in logs and in wva_constraint_provider_skipped.
const PhysicalLimiterName = "gpu-limiter"

// newQuotaLimiters builds one DefaultLimiter per QuotaLimiterConfig entry, each
// wrapping a QuotaInventory. NewLimiterFromConfig groups them, with the physical
// limiter when one is declared, so every entry's constraints are consulted.
//
// An entry that enables Kueue gets a kueue.Reader as its QuotaSource. The reader
// lists LocalQueues only in the watched namespace when the controller is
// namespace-scoped: that is where its RBAC reaches, and a cluster-wide list from
// there is Forbidden — which must surface as the error it is, not as "no quota".
func newQuotaLimiters(cfg *config.Config, kubeClient client.Client) ([]Limiter, error) {
	entries := cfg.EffectiveQuotaEntries()
	if len(entries) == 0 {
		return nil, errors.New("limiter factory: quota mode requires at least one inline " +
			"limiters: quota entry on the saturation \"default\" config")
	}
	constituents := make([]Limiter, 0, len(entries))
	for _, entry := range entries {
		var source QuotaSource
		if entry.KueueEnabled() {
			if kubeClient == nil {
				return nil, fmt.Errorf("limiter factory: quota entry %q enables kueue but no Kubernetes client is available", entry.Name)
			}
			source = sharedKueueReader(entry, kubeClient, cfg.WatchNamespace())
		}
		constituents = append(constituents, NewDefaultLimiter(entry.Name, NewQuotaInventoryWithSource(entry, source)))
	}
	return constituents, nil
}
