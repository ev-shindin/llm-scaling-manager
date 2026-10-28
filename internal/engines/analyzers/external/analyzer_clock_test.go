package external_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/external"
)

var _ = Describe("external.Analyzer clock", func() {
	It("stamps AnalyzedAt on the clock it is given", func() {
		fs := newFakeSource()
		at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
		a, err := external.New(agnosticDef(), fs)
		Expect(err).NotTo(HaveOccurred())
		a.WithClock(clocktesting.NewFakePassiveClock(at))
		fs.results[agnosticQueryName] = &source.MetricResult{Values: []source.MetricValue{{Value: 1}}}

		res, err := a.Analyze(context.Background(), domain.AnalyzerInput{ModelID: "m", Namespace: "ns"})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.AnalyzedAt).To(Equal(at))
	})
})
