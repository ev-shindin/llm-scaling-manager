package allocation

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
)

var _ = Describe("a Kueue-backed quota entry never read", func() {
	ctx := context.Background()
	entry := func(h100 int, kueue bool) config.QuotaLimiterConfig {
		e := config.QuotaLimiterConfig{
			Name: "kq", Type: "quota", Scope: config.QuotaScopeNamespace,
			NamespaceQuotas: map[string]map[string]int{"default": {"H100": h100}},
		}
		if kueue {
			e.Kueue = &config.KueueQuotaSource{Enabled: true}
		}
		return e
	}
	compute := func(e config.QuotaLimiterConfig, src *fakeQuotaSource) *ResourceConstraints {
		lim := NewDefaultLimiter(e.Name, NewQuotaInventoryWithSource(e, src))
		rc, err := lim.ComputeConstraints(ctx, nil, map[string]map[string]int{"team-a": {}})
		Expect(err).NotTo(HaveOccurred())
		return rc
	}
	failing := func() *fakeQuotaSource {
		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{}, errors.New("clusterqueues.kueue.x-k8s.io is forbidden"))
		return src
	}

	It("is reported while a -1 cap is unbounded and no read ever succeeded", func() {
		Expect(compute(entry(config.QuotaUnlimited, true), failing()).KueueNeverRead).To(BeTrue())
	})

	It("is not reported for a finite cap, an entry without Kueue, or once a read succeeded", func() {
		Expect(compute(entry(8, true), failing()).KueueNeverRead).To(BeFalse(), "the static cap bounds the type")
		Expect(compute(entry(config.QuotaUnlimited, false), failing()).KueueNeverRead).To(BeFalse(), "no Kueue to wait for")

		src := &fakeQuotaSource{}
		src.push(config.ExternalQuotas{ObservedAt: time.Unix(1000, 0),
			Namespace: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 4}}}}, nil)
		src.push(config.ExternalQuotas{ObservedAt: time.Unix(1000, 0),
			Namespace: map[string]config.ExternalCaps{"team-a": {ByType: map[string]int{"H100": 4}}}},
			errors.New("timeout"))
		inv := NewQuotaInventoryWithSource(entry(config.QuotaUnlimited, true), src)
		lim := NewDefaultLimiter("kq", inv)
		for range 2 {
			rc, err := lim.ComputeConstraints(ctx, nil, map[string]map[string]int{"team-a": {}})
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.KueueNeverRead).To(BeFalse(), "read once: the last snapshot bounds it, even while a later read fails")
		}
	})
})
