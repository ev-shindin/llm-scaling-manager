package collector

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source/prometheus"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
)

// The presence bits are tested through the real Prometheus parse path: a
// NaN from the backend reaches the collector as 0 with the NaN flag set, and
// that path is the one a mock MetricsSource with a raw NaN in Value skips.

// promStub answers each query with a fixed result. parsedSource registers
// every query name as its own text, so the text Query receives is the name.
// Only Query is called; the embedded API is nil.
type promStub struct {
	promv1.API
	series map[string]model.Value
	failed map[string]bool
}

func (s *promStub) Query(_ context.Context, q string, _ time.Time, _ ...promv1.Option) (model.Value, promv1.Warnings, error) {
	if s.failed[q] {
		return nil, nil, errors.New("query timed out")
	}
	if v, ok := s.series[q]; ok {
		return v, nil, nil
	}
	return model.Vector{}, nil, nil
}

// parsedSource is a real PrometheusSource that registers whatever the
// collector asks for, so the collector's query list need not be repeated here.
type parsedSource struct {
	*prometheus.PrometheusSource
}

func (p parsedSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	for _, name := range spec.Queries {
		if err := p.QueryList().Upsert(source.QueryTemplate{Name: name, Type: source.QueryTypePromQL, Template: name}); err != nil {
			return nil, err
		}
	}
	return p.PrometheusSource.Refresh(ctx, spec)
}

const presencePod = "pod-abc"

// presenceSample is one series for the test pod with value v, as Prometheus
// returns it.
func presenceSample(v float64) model.Value {
	return rankSamples("8000", v)
}

// rankSamples is one series per value, all for the test pod's engine on
// port: several series under one instance key.
func rankSamples(port string, vs ...float64) model.Vector {
	vec := make(model.Vector, 0, len(vs))
	for _, v := range vs {
		vec = append(vec, &model.Sample{
			Metric: model.Metric{
				seriesModelLabel: "test-model",
				"pod":            presencePod,
				"instance":       model.LabelValue("10.0.0.1:" + port),
			},
			Value:     model.SampleValue(v),
			Timestamp: model.TimeFromUnixNano(time.Now().UnixNano()),
		})
	}
	return vec
}

// collectParsed runs one collection of the test pod through the Prometheus
// parse path and returns its one row. A queue-length series is added when
// series has none, so the pod has a row whatever else is missing. reader
// lists the Pods; nil fails open to Ready.
func collectParsed(t *testing.T, series map[string]model.Value, failed map[string]bool, reader client.Reader) domain.ReplicaMetrics {
	t.Helper()
	got := collectParsedRows(t, series, failed, reader)
	if len(got) != 1 {
		t.Fatalf("expected 1 replica row, got %d", len(got))
	}
	return got[0]
}

// collectParsedRows is collectParsed returning every row.
func collectParsedRows(t *testing.T, series map[string]model.Value, failed map[string]bool, reader client.Reader) []domain.ReplicaMetrics {
	t.Helper()
	reg := promclient.NewRegistry()
	if err := metrics.InitMetrics(reg); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	all := map[string]model.Value{}
	for name, v := range series {
		all[name] = v
	}
	if _, ok := all[registration.QueryQueueLength]; !ok {
		all[registration.QueryQueueLength] = presenceSample(1)
	}
	cfg := prometheus.DefaultPrometheusSourceConfig()
	// Bounds the backoff on a failed query.
	cfg.QueryTimeout = 100 * time.Millisecond
	src := prometheus.NewPrometheusSource(context.Background(), &promStub{series: all, failed: failed}, cfg)
	t.Cleanup(src.Close)

	c := NewReplicaMetricsCollector(parsedSource{src}, fake.NewClientBuilder().WithScheme(scheme).Build(), reader, nil,
		scalerLocator(map[string]string{presencePod: "va-1"}))
	got, err := c.CollectReplicaMetrics(context.Background(), "test-model", "test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling), nil)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}
	return got
}

// presenceBits names each bit, the query it reports on and a reading that
// is valid for it.
var presenceBits = []struct {
	name  string
	query string
	valid float64
	// zeroKnown says a read zero is a reading (no work), as it is for every
	// gauge and rate here; an ITL of zero is not a measured mean.
	zeroKnown bool
	// negKnown says a finite negative value is kept as a reading. Only the
	// peak KV usage is, as its block takes the series without a range test;
	// every other block rejects it.
	negKnown bool
	bit      func(domain.ReplicaMetrics) bool
}{
	{"KvCacheUsageKnown", registration.QueryKvCacheUsage, 0.5, true, true,
		func(m domain.ReplicaMetrics) bool { return m.KvCacheUsageKnown }},
	{"KvUsageInstantKnown", registration.QueryKvUsageInstant, 0.4, true, false,
		func(m domain.ReplicaMetrics) bool { return m.KvUsageInstantKnown }},
	{"CapacityQueueKnown", registration.QueryCapacityQueueLength, 2, true, false,
		func(m domain.ReplicaMetrics) bool { return m.CapacityQueueKnown }},
	{"PreemptionRateKnown", registration.QueryPreemptionRate, 0.1, true, false,
		func(m domain.ReplicaMetrics) bool { return m.PreemptionRateKnown }},
	{"PrefillComputedTokenRateKnown", registration.QueryPrefillComputedTokenRate, 1000, true, false,
		func(m domain.ReplicaMetrics) bool { return m.PrefillComputedTokenRateKnown }},
	{"GenerationTokenRateKnown", registration.QueryGenerationTokenRate, 500, true, false,
		func(m domain.ReplicaMetrics) bool { return m.GenerationTokenRateKnown }},
	{"RequestRateKnown", registration.QueryRequestRate, 2, true, false,
		func(m domain.ReplicaMetrics) bool { return m.RequestRateKnown }},
	{"RunningRequestsKnown", registration.QueryRunningRequests, 8, true, false,
		func(m domain.ReplicaMetrics) bool { return m.RunningRequestsKnown }},
	{"AvgITLKnown", registration.QueryAvgITL, 0.02, false, false,
		func(m domain.ReplicaMetrics) bool { return m.AvgITLKnown }},
}

// Each bit is set by a present finite series and by nothing else: a NaN or
// an infinity the source turned into 0 is not a reading, and neither is a
// series that was not returned.
func TestPresenceBitsThroughPrometheusParse(t *testing.T) {
	for _, b := range presenceBits {
		t.Run(b.name, func(t *testing.T) {
			cases := []struct {
				name   string
				series map[string]model.Value
				want   bool
			}{
				{"present", map[string]model.Value{b.query: presenceSample(b.valid)}, true},
				{"present zero", map[string]model.Value{b.query: presenceSample(0)}, b.zeroKnown},
				{"NaN", map[string]model.Value{b.query: presenceSample(math.NaN())}, false},
				{"+Inf", map[string]model.Value{b.query: presenceSample(math.Inf(1))}, false},
				{"-Inf", map[string]model.Value{b.query: presenceSample(math.Inf(-1))}, false},
				{"negative", map[string]model.Value{b.query: presenceSample(-1)}, b.negKnown},
				{"absent", nil, false},
			}
			for _, tc := range cases {
				if got := b.bit(collectParsed(t, tc.series, nil, nil)); got != tc.want {
					t.Errorf("%s series: %s = %v, want %v", tc.name, b.name, got, tc.want)
				}
			}
		})
	}
}

// ITL's three states, on a live, an empty and a failed query, and on the two
// other ways a reading can be unusable.
func TestITLStateThroughPrometheusParse(t *testing.T) {
	cases := []struct {
		name    string
		series  map[string]model.Value
		failed  map[string]bool
		want    domain.ITLState
		wantITL float64
	}{
		{"live", map[string]model.Value{registration.QueryAvgITL: presenceSample(0.02)}, nil, domain.ITLFinite, 0.02},
		{"empty window (0/0 = NaN)", map[string]model.Value{registration.QueryAvgITL: presenceSample(math.NaN())}, nil, domain.ITLEmpty, 0},
		{"failed query", nil, map[string]bool{registration.QueryAvgITL: true}, domain.ITLAbsent, 0},
		{"absent series", nil, nil, domain.ITLAbsent, 0},
		{"+Inf", map[string]model.Value{registration.QueryAvgITL: presenceSample(math.Inf(1))}, nil, domain.ITLAbsent, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectParsed(t, tc.series, tc.failed, nil)
			if s := got.ITLState(); s != tc.want {
				t.Errorf("ITLState = %v, want %v", s, tc.want)
			}
			if got.AvgITLKnown && got.AvgITLEmpty {
				t.Error("AvgITLKnown and AvgITLEmpty are both set")
			}
			if got.AvgITL != tc.wantITL {
				t.Errorf("AvgITL = %v, want %v", got.AvgITL, tc.wantITL)
			}
		})
	}
}

// A not-Ready Pod's timing is discarded, and with it the ITL state: a live
// or an empty reading reads as absent.
func TestITLStateNotReadyIsAbsent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: presencePod, Namespace: "test-ns"},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}},
		},
	}).Build()
	for _, v := range []float64{0.02, math.NaN()} {
		got := collectParsed(t, map[string]model.Value{registration.QueryAvgITL: presenceSample(v)}, nil, reader)
		if got.Ready {
			t.Fatal("the Pod should read not Ready")
		}
		if s := got.ITLState(); s != domain.ITLAbsent {
			t.Errorf("not-Ready pod with ITL %v: ITLState = %v, want absent", v, s)
		}
	}
}

// A NaN or an infinity on every series gives the same numbers as a 0 did
// before the bits existed: only the bits tell the two apart.
func TestNonFiniteValuesUnchanged(t *testing.T) {
	all := func(v float64) map[string]model.Value {
		m := map[string]model.Value{}
		for _, b := range presenceBits {
			m[b.query] = presenceSample(v)
		}
		return m
	}
	clearBits := func(m domain.ReplicaMetrics) domain.ReplicaMetrics {
		m.KvCacheUsageKnown, m.KvUsageInstantKnown, m.CapacityQueueKnown, m.PreemptionRateKnown = false, false, false, false
		m.PrefillComputedTokenRateKnown, m.GenerationTokenRateKnown, m.RequestRateKnown, m.RunningRequestsKnown = false, false, false, false
		m.AvgITLKnown, m.AvgITLEmpty = false, false
		m.Metadata = nil
		return m
	}
	zero := clearBits(collectParsed(t, all(0), nil, nil))
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := clearBits(collectParsed(t, all(v), nil, nil)); !reflect.DeepEqual(got, zero) {
			t.Errorf("input %v:\n got %+v\nwant %+v", v, got, zero)
		}
	}
}

// A pod's ranks merge into one row: an ordinary bit is known only if every
// rank's is; ITL is known if one rank measured a mean and none is absent, and
// empty only if every rank is.
func TestMergePodInstances_PresenceBits(t *testing.T) {
	rank := func(known bool, itl domain.ITLState) domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m",
			KvCacheUsageKnown: known, PrefillComputedTokenRateKnown: known,
			GenerationTokenRateKnown: known, RequestRateKnown: known, RunningRequestsKnown: known,
			AvgITLKnown: itl == domain.ITLFinite, AvgITLEmpty: itl == domain.ITLEmpty}
	}
	ordinary := func(m domain.ReplicaMetrics) []bool {
		return []bool{m.KvCacheUsageKnown, m.PrefillComputedTokenRateKnown,
			m.GenerationTokenRateKnown, m.RequestRateKnown, m.RunningRequestsKnown}
	}
	for _, tc := range []struct {
		a, b      bool
		wantKnown bool
	}{{true, true, true}, {true, false, false}, {false, true, false}, {false, false, false}} {
		got := collapseToPods([]domain.ReplicaMetrics{rank(tc.a, domain.ITLAbsent), rank(tc.b, domain.ITLAbsent)})
		if len(got) != 1 {
			t.Fatalf("got %d rows, want 1", len(got))
		}
		for i, k := range ordinary(got[0]) {
			if k != tc.wantKnown {
				t.Errorf("ranks %v,%v: bit %d = %v, want %v", tc.a, tc.b, i, k, tc.wantKnown)
			}
		}
	}

	F, E, A := domain.ITLFinite, domain.ITLEmpty, domain.ITLAbsent
	for _, tc := range []struct {
		ranks []domain.ITLState
		want  domain.ITLState
	}{
		{[]domain.ITLState{F, F}, F},
		{[]domain.ITLState{F, E}, F},
		{[]domain.ITLState{E, F, E}, F},
		{[]domain.ITLState{E, E}, E},
		{[]domain.ITLState{F, A}, A},
		{[]domain.ITLState{E, A}, A},
		{[]domain.ITLState{A, A}, A},
		{[]domain.ITLState{F, E, A}, A},
	} {
		group := make([]domain.ReplicaMetrics, 0, len(tc.ranks))
		for _, s := range tc.ranks {
			group = append(group, rank(true, s))
		}
		got := collapseToPods(group)[0]
		if s := got.ITLState(); s != tc.want {
			t.Errorf("ranks %v: ITLState = %v, want %v", tc.ranks, s, tc.want)
		}
		if got.AvgITLKnown && got.AvgITLEmpty {
			t.Errorf("ranks %v: AvgITLKnown and AvgITLEmpty are both set", tc.ranks)
		}
	}
}

// Each ordinary bit merges on its own: with every bit set on both ranks but
// one cleared on one rank, the merged row loses that bit and keeps the rest.
// A cross-wired AND (one bit merged from another's field) fails here.
func TestMergePodInstances_PresenceBitsIndependent(t *testing.T) {
	bits := []struct {
		name  string
		clear func(*domain.ReplicaMetrics)
		get   func(domain.ReplicaMetrics) bool
	}{
		{"KvCacheUsageKnown", func(m *domain.ReplicaMetrics) { m.KvCacheUsageKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.KvCacheUsageKnown }},
		{"KvUsageInstantKnown", func(m *domain.ReplicaMetrics) { m.KvUsageInstantKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.KvUsageInstantKnown }},
		{"CapacityQueueKnown", func(m *domain.ReplicaMetrics) { m.CapacityQueueKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.CapacityQueueKnown }},
		{"PreemptionRateKnown", func(m *domain.ReplicaMetrics) { m.PreemptionRateKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.PreemptionRateKnown }},
		{"PrefillComputedTokenRateKnown", func(m *domain.ReplicaMetrics) { m.PrefillComputedTokenRateKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.PrefillComputedTokenRateKnown }},
		{"GenerationTokenRateKnown", func(m *domain.ReplicaMetrics) { m.GenerationTokenRateKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.GenerationTokenRateKnown }},
		{"RequestRateKnown", func(m *domain.ReplicaMetrics) { m.RequestRateKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.RequestRateKnown }},
		{"RunningRequestsKnown", func(m *domain.ReplicaMetrics) { m.RunningRequestsKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.RunningRequestsKnown }},
		{"AvgITLKnown", func(m *domain.ReplicaMetrics) { m.AvgITLKnown = false },
			func(m domain.ReplicaMetrics) bool { return m.AvgITLKnown }},
	}
	full := func() domain.ReplicaMetrics {
		return domain.ReplicaMetrics{PodName: "p", ModelID: "m",
			KvCacheUsageKnown: true, KvUsageInstantKnown: true, CapacityQueueKnown: true,
			PreemptionRateKnown: true, PrefillComputedTokenRateKnown: true,
			GenerationTokenRateKnown: true, RequestRateKnown: true, RunningRequestsKnown: true,
			AvgITLKnown: true}
	}
	for _, cleared := range bits {
		for _, onRank := range []int{0, 1} {
			ranks := []domain.ReplicaMetrics{full(), full()}
			cleared.clear(&ranks[onRank])
			got := collapseToPods(ranks)
			if len(got) != 1 {
				t.Fatalf("got %d rows, want 1", len(got))
			}
			for _, b := range bits {
				if want := b.name != cleared.name; b.get(got[0]) != want {
					t.Errorf("%s cleared on rank %d: %s = %v, want %v",
						cleared.name, onRank, b.name, b.get(got[0]), want)
				}
			}
		}
	}
}

// Several ITL series under one instance key: the state follows the value
// kept. A known mean stays known whatever order a NaN arrives in, and the
// row's AvgITL is that mean.
func TestITLStateSeveralSeriesOneKey(t *testing.T) {
	nan := math.NaN()
	cases := []struct {
		name    string
		values  []float64
		want    domain.ITLState
		wantITL float64
	}{
		{"finite then NaN", []float64{0.02, nan}, domain.ITLFinite, 0.02},
		{"NaN then finite", []float64{nan, 0.02}, domain.ITLFinite, 0.02},
		{"NaN then NaN", []float64{nan, nan}, domain.ITLEmpty, 0},
		{"finite then +Inf", []float64{0.02, math.Inf(1)}, domain.ITLFinite, 0.02},
		{"+Inf then NaN", []float64{math.Inf(1), nan}, domain.ITLEmpty, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collectParsed(t, map[string]model.Value{registration.QueryAvgITL: rankSamples("8000", tc.values...)}, nil, nil)
			if s := got.ITLState(); s != tc.want {
				t.Errorf("ITLState = %v, want %v", s, tc.want)
			}
			if got.AvgITLKnown && got.AvgITLEmpty {
				t.Error("AvgITLKnown and AvgITLEmpty are both set")
			}
			if got.AvgITL != tc.wantITL {
				t.Errorf("AvgITL = %v, want %v", got.AvgITL, tc.wantITL)
			}
		})
	}
}

// Two ranks of one pod, each its own scrape target, from parsed series
// through attributeInstance and collapseToPods: the pod's ITL state is the
// merge of the ranks' states, an absent rank being one with no ITL series.
func TestITLStateTwoRanksFullPath(t *testing.T) {
	const absent = -1.0 // marks a rank with no ITL series
	nan := math.NaN()
	cases := []struct {
		name   string
		r0, r1 float64
		want   domain.ITLState
	}{
		{"finite + finite", 0.02, 0.03, domain.ITLFinite},
		{"finite + empty", 0.02, nan, domain.ITLFinite},
		{"empty + empty", nan, nan, domain.ITLEmpty},
		{"finite + absent", 0.02, absent, domain.ITLAbsent},
		{"empty + absent", nan, absent, domain.ITLAbsent},
		{"absent + absent", absent, absent, domain.ITLAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			itl := model.Vector{}
			for port, v := range map[string]float64{"8000": tc.r0, "8001": tc.r1} {
				if v != absent {
					itl = append(itl, rankSamples(port, v)...)
				}
			}
			queue := append(rankSamples("8000", 1), rankSamples("8001", 1)...)
			rows := collectParsedRows(t, map[string]model.Value{
				registration.QueryQueueLength: queue,
				registration.QueryAvgITL:      itl,
			}, nil, nil)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want the two ranks merged into 1", len(rows))
			}
			if rows[0].EngineRanks != 2 {
				t.Fatalf("EngineRanks = %d, want 2", rows[0].EngineRanks)
			}
			if s := rows[0].ITLState(); s != tc.want {
				t.Errorf("ITLState = %v, want %v", s, tc.want)
			}
			if rows[0].AvgITLKnown && rows[0].AvgITLEmpty {
				t.Error("AvgITLKnown and AvgITLEmpty are both set")
			}
		})
	}
}
