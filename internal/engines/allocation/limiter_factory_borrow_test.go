package allocation

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

// The factory turns borrowing on for a Kueue-backed namespace-scope entry
// unless it says countBorrowable: false, and passes the physical limiter's
// presence as the GPU bound. What the factory decided is then visible in what
// the inventory counts: Borrowing() and the effective entry.
func TestLimiterFactoryCountBorrowable(t *testing.T) {
	ctx := context.Background()
	observed := time.Unix(1000, 0)
	on, off := true, false
	snapshot := config.ExternalQuotas{
		Namespace:              map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 8}}},
		NamespaceGuaranteed:    map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 0}}},
		NamespaceWithinNominal: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 8}}},
		Borrow: map[string][]config.ExternalBorrow{"team-a": {{
			Queue: "team-a-cq", Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Nominal: 8, Guaranteed: 0, Borrowable: 16, BorrowCap: 8, WithinNominal: 8,
		}}},
		SharedPools: []config.ExternalSharedPool{{Cohort: "c", Flavor: "h100", Resource: "nvidia.com/gpu",
			Accelerator: "H100", Free: 16}},
		ObservedAt: observed,
	}
	kube := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()

	for _, tc := range []struct {
		name           string
		count          *bool
		wantBorrowing  bool
		wantNamespaceQ int
	}{
		{"unset counts borrowing", nil, true, 0},
		{"true counts borrowing", &on, true, 0},
		{"false keeps the nominal cap", &off, false, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.NewTestConfig()
			cfg.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{"default": {Limiters: []config.QuotaLimiterConfig{
				{Type: "gpu-inventory", Name: "gpu"},
				{
					Name: "tenant-quota", Type: "quota", Scope: config.QuotaScopeNamespace,
					NamespaceQuotas: map[string]map[string]int{"default": {"H100": -1}},
					Kueue:           &config.KueueQuotaSource{Enabled: true, CountBorrowable: tc.count},
				},
			}}})
			l, err := NewLimiterFromConfig(cfg, kube)
			if err != nil {
				t.Fatal(err)
			}
			var inv *QuotaInventory
			for _, cp := range ConstraintProvidersFrom(l) {
				if d, ok := cp.(*DefaultLimiter); ok && cp.Name() == "tenant-quota" {
					inv, _ = d.inventory.(*QuotaInventory)
				}
			}
			if inv == nil {
				t.Fatal("the factory built no QuotaInventory for tenant-quota")
			}
			enabled := inv.borrowOpts.Eligibility != nil
			if enabled != tc.wantBorrowing {
				t.Fatalf("factory enabled borrowing = %t, want %t", enabled, tc.wantBorrowing)
			}
			if enabled && !inv.borrowOpts.Physical {
				t.Error("a physical limiter is declared beside the entry: it must be passed as the GPU bound")
			}

			// Read Kueue through a scripted source and, where the factory
			// enabled borrowing, an eligibility that passes every namespace.
			src := &fakeQuotaSource{}
			src.push(snapshot, nil)
			inv.source = src
			inv.now = func() time.Time { return observed.Add(10 * time.Second) }
			if enabled {
				inv.borrowOpts.Eligibility = &fakeEligibility{}
			}
			if err := inv.Refresh(ctx); err != nil {
				t.Fatal(err)
			}

			byNS, pools, _ := inv.Borrowing([]string{"team-a"})
			if got := byNS != nil && pools != nil; got != tc.wantBorrowing {
				t.Errorf("Borrowing() counted = %t (byNS %v, pools %v), want %t", got, byNS, pools, tc.wantBorrowing)
			}
			if tc.wantBorrowing && byNS["team-a"]["H100"].Cap != 16 {
				t.Errorf("borrow cap = %+v, want the borrowable 16 past a guaranteed 0", byNS["team-a"])
			}
			if got := inv.EffectiveConfig().NamespaceQuotas["team-a"]["H100"]; got != tc.wantNamespaceQ {
				t.Errorf("effective team-a H100 = %d, want %d (guaranteed when borrowing is counted, nominal otherwise)",
					got, tc.wantNamespaceQ)
			}
		})
	}
}
