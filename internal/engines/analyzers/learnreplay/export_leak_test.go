package learnreplay

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// The adversarial verification of PR #388 (round 2), kept as regression
// tests: each puts a name where the export at cc54182b2 let it through, and
// fails when the export commits it. A refusal passes; a refusal that repeats
// the name fails.

// zzOpts shape the probe recording.
type zzOpts struct {
	ns, model, accel string
	mutate           func(*domain.VariantReplicaState)
	before           func(*learnrec.Recorder, *fixture)
	pol              *config.ScalingPolicy
}

// zzRec is a probe recording: one variant, one Analyze, before first.
func zzRec(t *testing.T, o zzOpts) []byte {
	t.Helper()
	if o.ns == "" {
		o.ns = "ns"
	}
	if o.model == "" {
		o.model = "mdl"
	}
	if o.accel == "" {
		o.accel = "H100"
	}
	if o.pol == nil {
		o.pol = &config.ScalingPolicy{KvCacheThreshold: 0.8}
	}
	f := newFixture(replayAt)
	f.buf.Reset()
	rec := learnrec.New(f.buf, 0)
	f.attach(rec)
	rec.Record(learnrec.KindStart, replayAt, defaultStart())
	if o.before != nil {
		o.before(rec, f)
	}
	params := capacity.EngineParams{Engine: inferenceengine.EngineVLLM, MaxNumSeqs: 64, ModelPath: o.model, Image: "img:1"}
	f.store.FileScaleTarget(o.ns, o.model, "v", o.accel, 1, params, false, replayAt)
	st := domain.VariantReplicaState{VariantName: "v", GPUsPerReplica: 1, AcceleratorName: o.accel, CurrentReplicas: 1}
	if o.mutate != nil {
		o.mutate(&st)
	}
	_, err := f.sat.Analyze(context.Background(), domain.AnalyzerInput{ModelID: o.model, Namespace: o.ns, Config: o.pol, VariantStates: []domain.VariantReplicaState{st}})
	require.NoError(t, err)
	require.NoError(t, rec.Err())
	return bytes.Clone(f.buf.Bytes())
}

// zzLeak fails when the export succeeds and still holds a secret, or when
// a refusal repeats one, or when a renamed export does not replay.
func zzLeak(t *testing.T, rec []byte, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		require.Contains(t, string(rec), s, "precondition")
	}
	out, rep, err := Export(rec, ExportOptions{Key: []byte("k")})
	if err != nil {
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("REFUSAL REPEATS %q: %v", s, err)
			}
		}
		t.Logf("REFUSED: %v", err)
		return
	}
	leaked := false
	for _, s := range secrets {
		if strings.Contains(string(out), s) {
			leaked = true
			inKept := false
			for _, k := range rep.Kept {
				if strings.Contains(k, s) {
					inKept = true
				}
			}
			for _, line := range strings.Split(string(out), "\n") {
				if i := strings.Index(line, s); i >= 0 {
					lo, hi := max(0, i-120), min(len(line), i+len(s)+40)
					t.Errorf("LEAK %q (listed in report.Kept: %t): ...%s...", s, inKept, line[lo:hi])
					break
				}
			}
		}
	}
	if !leaked {
		t.Logf("RENAMED: %s", rep)
	}
	r, err := Replay(context.Background(), out, Options{Mode: Fidelity})
	require.NoError(t, err)
	if len(r.Failed()) > 0 || len(r.Mismatches) > 0 {
		t.Errorf("the export does not replay: %d mismatches", len(r.Mismatches))
	}
}

// plan records v as an event of an owner the replay does not drive.
func plan(r *learnrec.Recorder, v any) { r.Record("composite.Plan", replayAt, v) }

func TestExportLeak_NodeSelectorWellKnownDomainName(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"payroll.kubernetes.io/team": "x1", "nvidia.com/payrollpool": "true"}}}
	}}), "payroll.kubernetes.io", "payrollpool")
}

func TestExportLeak_NodeSelectorShapedLikeStruct(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"AcceleratorName": "payroll-secret-pool"}}}
	}}), "payroll-secret-pool")
}

func TestExportLeak_NodeSelectorShapedLikeStructUnresolved(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"Unresolved": "--payroll-flag", "Commit": "deadbeef"}}}
	}}), "--payroll-flag")
}

func TestExportLeak_NodeSelectorValueRenamed(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.Placements = []domain.PodPlacement{{NodeSelector: map[string]string{"topology.kubernetes.io/zone": "payroll-east-1"}}}
	}}), "payroll-east-1")
}

func TestExportLeak_AffinityExpr(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.Placements = []domain.PodPlacement{{RequiredNodeAffinity: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "node.kubernetes.io/payrollrack", Operator: "In", Values: []string{"rack-payroll-7"}}},
		}}}}}
	}}), "payrollrack", "rack-payroll-7")
}

func TestExportLeak_TolerationKey(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.Tolerations = []corev1.Toleration{{Key: "payroll.k8s.io/dedicated", Operator: "Equal", Value: "payroll-ded", Effect: "NoSchedule"}}
		s.TopologyKey = "payroll.x-k8s.io/rack"
	}}), "payroll.k8s.io", "payroll-ded", "payroll.x-k8s.io")
}

func TestExportLeak_ReplicaRequestsKey(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{mutate: func(s *domain.VariantReplicaState) {
		s.ReplicaRequests = map[string]int64{"payroll.amd.com/fpga": 1, "nvidia.com/gpu": 1}
	}}), "payroll.amd.com")
}

func TestExportLeak_AcceleratorName(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{accel: "payroll-team-pool"}), "payroll-team-pool")
}

func TestExportLeak_AcceleratorReusedAsCompositePart(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{accel: "payrollgpu", before: func(_ *learnrec.Recorder, f *fixture) {
		f.sat.RestoreLearnedState(saturation.LearnedState{VariantSeenAt: map[string]time.Time{"payrollgpu/llama": replayAt.Add(-time.Hour)}})
	}}), "payrollgpu/")
}

func TestExportLeak_ErrorPlainWords(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{`lookup prometheus in northzone failed: cluster="eastprod" team=payroll <acmecorp>`})
	}}), "northzone", "eastprod", "payroll", "acmecorp")
}

func TestExportLeak_ErrorCaseVariantOfAName(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "payroll", before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"quota of PAYROLL and Payroll and payrolls exhausted"})
	}}), "PAYROLL", "Payroll", "payrolls")
}

func TestExportLeak_ErrorNetworkShapes(t *testing.T) {
	for name, c := range map[string][2]string{
		"ipv6":       {"dial [fd00:ab::12]:8443 refused", "fd00:ab"},
		"ipv6bare":   {"dial fd00:ab::12 refused", "fd00:ab"},
		"noscheme":   {"get metrics.payroll.io/api failed", "payroll"},
		"twolabel":   {"get payroll.io:9090 failed", "payroll"},
		"image":      {"pull quay.io/payroll/llama:v1 failed", "payroll"},
		"port":       {"dial payrollhost:9090 failed", "payroll"},
		"ipport":     {"dial 10.1.2.3:9 failed", "10.1.2.3"},
		"unicode":    {"dial café-payroll failed", "payroll"},
		"nbsp":       {"dial payroll east failed", "payroll"},
		"digits":     {"node payroll42 failed", "payroll"},
		"hexword":    {"id deadbeefcafe0123 failed", "deadbeefcafe0123"},
		"slashwords": {"path payroll/finance failed", "payroll"},
		"dotted":     {"svc payroll.finance failed", "payroll"},
		"pipe":       {"key payroll|finance failed", "payroll"},
		"tab":        {"x\tpayroll\tfinance", "payroll"},
		"colon":      {"payroll: finance.", "payroll"},
		"duration":   {"5m payroll", "payroll"},
		"ipv4mapped": {"dial ::ffff:10.1.2.3 refused", "10.1.2.3"},
		"email":      {"owner bob@payroll failed", "payroll"},
		"hyphenword": {"owner payroll-team failed", "payroll"},
		"leadingdot": {"owner .payroll failed", "payroll"},
		"hostnodots": {"dial http://payrollsvc failed", "payrollsvc"},
		"urlcreds":   {"dial user:payrollpw@host failed", "payrollpw"},
	} {
		t.Run(name, func(t *testing.T) {
			msg := c[0]
			zzLeak(t, zzRec(t, zzOpts{before: func(r *learnrec.Recorder, _ *fixture) {
				plan(r, struct{ Error string }{msg})
			}}), c[1])
		})
	}
}

func TestExportLeak_ErrorIPv6Specific(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"dial [fd00:ab::12]:8443 refused"})
	}}), "fd00:ab::12")
}

func TestExportLeak_VocabularyShapedNamespaceInComposite(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{before: func(_ *learnrec.Recorder, f *fixture) {
		f.sat.RestoreLearnedState(saturation.LearnedState{VariantSeenAt: map[string]time.Time{
			"store/llama-finance": replayAt.Add(-time.Hour), "kueue/x": replayAt.Add(-time.Hour), "infinity/nan": replayAt.Add(-time.Hour),
			"1e3/1234": replayAt.Add(-time.Hour), "deadbeefcafe0123/y": replayAt.Add(-time.Hour), "H100/payrollv": replayAt.Add(-time.Hour),
		}})
	}}), "llama-finance", "store/", "kueue/", "infinity/nan", "1e3/1234", "deadbeefcafe0123", "payrollv")
}

func TestExportLeak_CompositeOddSeparators(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{before: func(_ *learnrec.Recorder, f *fixture) {
		f.sat.RestoreLearnedState(saturation.LearnedState{VariantSeenAt: map[string]time.Time{
			"ns|payroll#fin|v": replayAt, "ns/payroll:fin": replayAt, `ns\payrollbs`: replayAt, "ns payrollsp/x": replayAt,
			"ns,payrollcomma": replayAt, "ns||payrollempty": replayAt, "|payrolllead": replayAt,
		}})
	}}), "payroll")
}

func TestExportLeak_NamespaceDefault(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "default"}), `"Namespace":"default"`)
}

func TestExportLeak_NamespaceH100(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "h100", accel: "h100"}), `"Namespace":"h100"`)
}

func TestExportLeak_NamespaceHex16(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "deadbeefcafe0123"}), "deadbeefcafe0123")
}

func TestExportLeak_NamespaceDigits(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "4711"}), `"Namespace":"4711"`)
}

func TestExportLeak_NamespaceKubernetesIO(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "kube-system", model: "team.kubernetes.io/owner"}), "kube-system", "team.kubernetes.io/owner")
}

func TestExportLeak_PseudonymShapedNames(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{ns: "ns-02", model: "model-01"}), `"Namespace":"ns-02"`)
}

func TestExportLeak_KeptClasses(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct {
			Unresolved []string
			ConfigType string
			Resources  []string
			Commit     string
			Answer     string
		}{[]string{"--payroll-api-key"}, "payroll.Secret", []string{"payroll.nvidia.com/fpga"}, "deadbeefdeadbeef", "payroll-gpu-x"})
	}}), "--payroll-api-key", "payroll.Secret", "payroll.nvidia.com", "payroll-gpu-x")
}

func TestExportLeak_SourceCase(t *testing.T) {
	rec := zzRec(t, zzOpts{ns: "payroll"})
	_, _, err := Export(rec, ExportOptions{Key: []byte("k"), Text: []string{"E1 run in Payroll", "E1 run in PAYROLL namespace"}})
	if err == nil {
		t.Errorf("LEAK source text with a case variant of the namespace was accepted")
	} else {
		t.Logf("REFUSED: %v", err)
	}
	_, _, err = Export(rec, ExportOptions{Key: []byte("k"), Text: []string{"E1 run in payroll-ns"}})
	t.Logf("payroll-ns in source: err=%v", err)
	_, _, err = Export(rec, ExportOptions{Key: []byte("k"), Text: []string{"E1 run in payroll_ns"}})
	t.Logf("payroll_ns in source: err=%v", err)
}

func TestExportLeak_ImageAndModelInText(t *testing.T) {
	zzLeak(t, zzRec(t, zzOpts{model: "acme/llama-payroll", before: func(r *learnrec.Recorder, _ *fixture) {
		plan(r, struct{ Error string }{"model acme/llama-payroll not found; llama-payroll missing; acme missing"})
	}}), "llama-payroll", "acme")
}

func TestExportLeak_WeightAndQuota(t *testing.T) {
	pol := &config.ScalingPolicy{KvCacheThreshold: 0.8, Limiters: []config.QuotaLimiterConfig{{
		Name: "payrollq", Type: "quota", Scope: config.QuotaScopeCluster,
		ClusterQuotas: map[string]int{"payroll.nvidia.com/gpu": 8},
	}}}
	zzLeak(t, zzRec(t, zzOpts{pol: pol}), "payrollq", "payroll.nvidia.com")
}
