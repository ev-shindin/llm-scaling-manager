package learnrec

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
)

type sample struct {
	F        float64
	Floats   []float64
	At       time.Time
	Nil      map[string]int
	Empty    map[string]int
	NilList  []string
	Ptr      *int
	Keys     map[int]string
	Nested   *sample
	Duration time.Duration
	Any      any
}

func TestCodec_RoundTripsWhatJSONCannot(t *testing.T) {
	seven := 7
	in := sample{
		F:        0.1,
		Floats:   []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1e-310, 1e-300, 123456789.125},
		At:       time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("x", 3600)),
		Empty:    map[string]int{},
		Ptr:      &seven,
		Keys:     map[int]string{10: "b", 2: "a"},
		Nested:   &sample{F: 2},
		Duration: 90 * time.Second,
		Any:      map[string]any{"k": 1.5},
	}
	b, err := Marshal(in)
	require.NoError(t, err)
	var out sample
	require.NoError(t, Unmarshal(b, &out))
	again, err := Marshal(out)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again), "Marshal of what Unmarshal read is the same bytes")
	assert.True(t, math.IsNaN(out.Floats[0]))
	assert.True(t, math.IsInf(out.Floats[1], 1))
	assert.True(t, math.IsInf(out.Floats[2], -1))
	assert.Nil(t, out.Nil, "nil stays nil")
	assert.NotNil(t, out.Empty, "empty stays empty")
	assert.Nil(t, out.NilList)
	assert.True(t, in.At.Equal(out.At), "a time reads back as the same instant")
	assert.Equal(t, time.UTC, out.At.Location(), "in UTC")
	assert.Equal(t, 90*time.Second, out.Duration)
	assert.Contains(t, string(b), `"Keys":{"10":"b","2":"a"}`, "map keys are sorted")
}

func TestCodec_IsTheSameWhateverTheMapOrder(t *testing.T) {
	m := map[string]float64{}
	for i := range 200 {
		m[string(rune('a'+i%26))+string(rune('A'+i/26))] = float64(i) / 7
	}
	first, err := Marshal(m)
	require.NoError(t, err)
	for range 20 {
		b, err := Marshal(m)
		require.NoError(t, err)
		require.Equal(t, string(first), string(b))
	}
}

func TestCodec_RefusesAStructWithAnUnexportedField(t *testing.T) {
	type hidden struct {
		Shown int
		gone  int
	}
	_, err := Marshal(hidden{Shown: 1, gone: 2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexported field gone")
}

func TestCodec_RefusesAFieldTheTypeDoesNotHave(t *testing.T) {
	var s sample
	err := Unmarshal([]byte(`{"F":1,"Renamed":2}`), &s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no field Renamed")
}

// The engine's inputs encode: every row field with its presence bits, and the
// policy, the model weight (unexported fields, read and written through
// RecordedValue) included, a weight that failed to parse as well.
func TestCodec_CarriesAnInputWithAPolicy(t *testing.T) {
	in := domain.AnalyzerInput{ModelID: "m", Namespace: "ns",
		ReplicaMetrics: []domain.ReplicaMetrics{{PodName: "p", AvgITL: math.NaN(), AvgITLEmpty: true, AvgITLKnown: true,
			ReadySince: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}},
		Config: &config.ScalingPolicy{KvCacheThreshold: 0.8, Weight: config.NewModelWeight(2.5),
			AnalyzerTuning: config.AnalyzerTuning{SteadyGate: config.SteadyGateTuning{IdleRows: 4}}},
	}
	rec := InputOf(in)
	assert.Equal(t, "*config.ScalingPolicy", rec.ConfigType)
	b, err := Marshal(rec)
	require.NoError(t, err)
	var back struct {
		ModelID, Namespace string
		ReplicaMetrics     []domain.ReplicaMetrics
		VariantStates      []domain.VariantReplicaState
		Config             *config.ScalingPolicy
		ConfigType         string
		SchedulerQueue     *domain.SchedulerQueueMetrics
		ArrivalRate        float64
	}
	require.NoError(t, Unmarshal(b, &back))
	assert.Equal(t, 2.5, back.Config.Weight.Value())
	assert.True(t, back.Config.Weight.IsSet())
	assert.Equal(t, 4, back.Config.AnalyzerTuning.SteadyGate.IdleRows)
	assert.True(t, math.IsNaN(back.ReplicaMetrics[0].AvgITL))
	assert.True(t, back.ReplicaMetrics[0].AvgITLEmpty)
	again, err := Marshal(InputOf(domain.AnalyzerInput{ModelID: back.ModelID, Namespace: back.Namespace,
		ReplicaMetrics: back.ReplicaMetrics, Config: back.Config}))
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again))
}

func TestCodec_CarriesAWeightThatFailedToParse(t *testing.T) {
	var p config.ScalingPolicy
	require.NoError(t, yaml.Unmarshal([]byte("weight: heavy\n"), &p))
	require.Error(t, p.Weight.Err(), "precondition: the weight did not parse")
	b, err := Marshal(p)
	require.NoError(t, err)
	var back config.ScalingPolicy
	require.NoError(t, Unmarshal(b, &back))
	require.Error(t, back.Weight.Err())
	assert.Equal(t, p.Weight.Err().Error(), back.Weight.Err().Error())
	assert.False(t, back.Weight.IsSet())
}

func TestRecorder_NumbersEventsFromOne(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 0)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, uint64(1), r.Record("a", at, 1))
	assert.Equal(t, uint64(2), r.Record("b", at, 2))
	events, err := readEvents(buf.Bytes())
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "b", events[1].Kind)
	assert.Equal(t, uint64(2), events[1].Seq)
	var v int
	require.NoError(t, events[1].DecodeData(&v))
	assert.Equal(t, 2, v)
}

func TestRecorder_ANilRecorderRecordsNothing(t *testing.T) {
	var r *Recorder
	assert.Zero(t, r.Record("a", time.Time{}, 1))
	assert.Nil(t, r.Fragment(1))
	assert.False(t, r.Active())
	assert.NoError(t, r.Close())
}

// At the cap the recording keeps its head, stops, and counts what it drops.
func TestRecorder_TheCapKeepsTheHeadAndCountsWhatItDrops(t *testing.T) {
	var buf bytes.Buffer
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	line, err := Marshal(Event{Seq: 1, Now: at, Kind: "k", Data: 1})
	require.NoError(t, err)
	r := New(&buf, int64(3*(len(line)+1)))
	for range 3 {
		require.NotZero(t, r.Record("k", at, 1))
	}
	// The fourth would pass the cap; so does every line after it, short or not.
	assert.Zero(t, r.Record("k", at, 1))
	assert.Zero(t, r.Record("k", at, nil))
	assert.Equal(t, uint64(2), r.droppedCount())
	events, err := readEvents(buf.Bytes())
	require.NoError(t, err)
	require.Len(t, events, 3, "the head is kept whole")
	assert.Equal(t, uint64(3), events[2].Seq)
	assert.Equal(t, int64(buf.Len()), r.writtenBytes())
	assert.LessOrEqual(t, r.writtenBytes(), int64(3*(len(line)+1)))
}

func TestRecorder_AnEncodingErrorStopsTheRecording(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 0)
	r.Record("bad", time.Time{}, struct{ hidden int }{1})
	require.Error(t, r.Err())
	assert.Zero(t, r.Record("good", time.Time{}, 1), "nothing is written after the error")
	assert.Equal(t, uint64(2), r.droppedCount())
}

// readEvents is Read's events.
func readEvents(b []byte) ([]RawEvent, error) {
	r, err := Read(b)
	return r.Events, err
}

// A Raw reads back as the bytes it was written with, whatever their key
// order: Marshal writes struct fields in declaration order, which a sorted
// re-encoding would not keep.
func TestCodec_ARawKeepsItsBytes(t *testing.T) {
	type inner struct{ Zeta, Alpha float64 }
	type outer struct {
		Before string
		Doc    Raw
	}
	doc, err := Marshal(inner{Zeta: 1, Alpha: 0.5})
	require.NoError(t, err)
	b, err := Marshal(outer{Before: "x", Doc: Raw(doc)})
	require.NoError(t, err)
	var back outer
	require.NoError(t, Unmarshal(b, &back))
	assert.Equal(t, string(doc), string(back.Doc))
	again, err := Marshal(back)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(again))
}
