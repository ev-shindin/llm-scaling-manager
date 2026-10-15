package controller

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

const (
	rejectsSystemNS = "wva-rejects-system"
	rejectsPolicyNS = "wva-rejects-policy"
	// rejectsTenantNS is opted in to namespace-local scaling policy.
	rejectsTenantNS = "wva-rejects-tenant"

	// quotaDefault bounds the install with a quota limiter.
	quotaDefault = `limiters:
  - type: quota
    name: install-quota
    scope: cluster
    quotas: { H100: 8 }
`
	// invalidQuotaDefault is quotaDefault with a value Validate rejects.
	invalidQuotaDefault = quotaDefault + "kvCacheThreshold: 1.5\n"
	// typoQuotaDefault is quotaDefault with a misspelled key.
	typoQuotaDefault = quotaDefault + "scaleUpThreshhold: 0.9\n"
)

type rejectsFixture struct {
	t   *testing.T
	cfg *config.Config
	c   client.Client
	rec *record.FakeRecorder
	r   *ConfigMapReconciler
}

func newRejectsFixture(t *testing.T) *rejectsFixture {
	t.Helper()
	t.Setenv("POD_NAMESPACE", rejectsSystemNS)
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	rec := record.NewFakeRecorder(20)
	cfg := config.NewTestConfig()
	return &rejectsFixture{t: t, cfg: cfg, c: c, rec: rec,
		r: &ConfigMapReconciler{Reader: c, Scheme: s, Config: cfg, Recorder: rec}}
}

// apply creates or updates the scaling-policy ConfigMap in namespace with data,
// then reconciles it.
func (f *rejectsFixture) apply(namespace string, data map[string]string) {
	f.t.Helper()
	ctx := context.Background()
	key := types.NamespacedName{Namespace: namespace, Name: config.ScalingPolicyConfigMapName()}
	var cm corev1.ConfigMap
	if err := f.c.Get(ctx, key, &cm); err == nil {
		cm.Data = data
		if err := f.c.Update(ctx, &cm); err != nil {
			f.t.Fatal(err)
		}
	} else {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: key.Name}, Data: data}
		if err := f.c.Create(ctx, &cm); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *rejectsFixture) events() []string {
	var out []string
	for {
		select {
		case e := <-f.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// limiterSet is the declared limiter set of cfg as the logs and the controller
// status render it: "quota", "inventory+quota", or "none".
func limiterSet(cfg *config.Config) string {
	return cfg.EffectiveLimiterTypes().String()
}

// TestRejectedDefaultKeepsTheLimiterInForce: a scaling-policy edit whose
// "default" entry fails validation used to drop the entry, and the surviving
// entries replaced the live set, so one bad value removed every limiter.
func TestRejectedDefaultKeepsTheLimiterInForce(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	f := newRejectsFixture(t)

	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: quotaDefault})
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Fatalf("setup: limiter mode %q, want quota", got)
	}

	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: invalidQuotaDefault})
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Errorf("an invalid default entry removed the limiter: mode %q, want quota still in force", got)
	}
	evs := f.events()
	if len(evs) != 1 || !strings.Contains(evs[0], "Warning "+constants.K8SEventConfigEntryRejected) ||
		!strings.Contains(evs[0], "previous entry") {
		t.Errorf("want one ConfigEntryRejected Warning saying the previous entry stays, got %v", evs)
	}
	if got := gaugeValue(t, registry, constants.WVAConfigRejectedEntries); got != 1 {
		t.Errorf("%s = %v, want 1", constants.WVAConfigRejectedEntries, got)
	}

	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: quotaDefault})
	if got := gaugeValue(t, registry, constants.WVAConfigRejectedEntries); got != 0 {
		t.Errorf("%s = %v after the fix, want 0", constants.WVAConfigRejectedEntries, got)
	}
}

// TestKeptDefaultWarnsOfTheRestart: a bad "default" in the ConfigMap the
// limiters come from is survivable at runtime, where the previous entry is
// kept, and fatal on the next restart, where there is none to keep. The
// warning must say so, and only for that entry of that ConfigMap.
func TestKeptDefaultWarnsOfTheRestart(t *testing.T) {
	const restartWarning = "will refuse to start until the entry is fixed"
	cases := []struct {
		name        string
		policyNS    bool   // a separate policy namespace is in use
		applyNS     string // the ConfigMap the bad entry is applied to
		key         string // the entry that is rejected
		wantWarning bool
	}{
		{name: "default in the controller's own ConfigMap", applyNS: rejectsSystemNS,
			key: config.GlobalDefaultsKey, wantWarning: true},
		{name: "default in the policy namespace", policyNS: true, applyNS: rejectsPolicyNS,
			key: config.GlobalDefaultsKey, wantWarning: true},
		{name: "default in the controller's own ConfigMap while policy is separate", policyNS: true,
			applyNS: rejectsSystemNS, key: config.GlobalDefaultsKey, wantWarning: false},
		{name: "an entry other than default", applyNS: rejectsSystemNS, key: "some-tier", wantWarning: false},
		// A namespace-local "default" tunes one namespace and bounds nothing,
		// so a bad one never stops the controller from starting.
		{name: "default in a namespace-local ConfigMap", applyNS: rejectsTenantNS,
			key: config.GlobalDefaultsKey, wantWarning: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRejectsFixture(t)
			tenant := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: rejectsTenantNS,
				Labels: map[string]string{constants.NamespaceConfigEnabledLabelKey: constants.AnnotationValueTrue}}}
			if err := f.c.Create(context.Background(), tenant); err != nil {
				t.Fatal(err)
			}
			if tc.policyNS {
				f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
			}
			f.cfg.MarkConfigMapsBootstrapComplete()
			good := map[string]string{config.GlobalDefaultsKey: quotaDefault, "some-tier": quotaDefault}
			f.apply(tc.applyNS, good)
			_ = f.events()

			bad := map[string]string{config.GlobalDefaultsKey: quotaDefault, "some-tier": quotaDefault}
			bad[tc.key] = invalidQuotaDefault
			f.apply(tc.applyNS, bad)
			var rejected []string
			for _, e := range f.events() {
				if strings.Contains(e, constants.K8SEventConfigEntryRejected) {
					rejected = append(rejected, e)
				}
			}
			if len(rejected) != 1 {
				t.Fatalf("want one %s event, got %v", constants.K8SEventConfigEntryRejected, rejected)
			}
			if got := strings.Contains(rejected[0], restartWarning); got != tc.wantWarning {
				t.Errorf("event warns of the restart = %v, want %v: %s", got, tc.wantWarning, rejected[0])
			}
		})
	}
}

// TestRejectedDefaultAtStartupSaysItStopsStartup: the startup refusal exits
// the process, and the event may never be sent. What it says must therefore
// be about this start, not the next one.
func TestRejectedDefaultAtStartupSaysItStopsStartup(t *testing.T) {
	got := rejectedEntryOutcome(config.GlobalDefaultsKey, false, true, false)
	if !strings.Contains(got, "refuses to start while the bounding \"default\" entry is invalid") {
		t.Errorf("startup outcome = %q, want it to say the controller refuses to start", got)
	}
}

// TestUnknownKeyIsAppliedAndReported: a misspelled key used to be dropped
// without a word. It must be reported, and it must NOT cost the entry: an
// unknown key in "default" that rejected the entry would drop the limiters with
// it, which is the bound failing open.
func TestUnknownKeyIsAppliedAndReported(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatal(err)
	}
	f := newRejectsFixture(t)
	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: typoQuotaDefault})

	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Errorf("an unknown key cost the entry its limiter: mode %q, want quota", got)
	}
	evs := f.events()
	if len(evs) != 1 || !strings.Contains(evs[0], "Warning "+constants.K8SEventConfigEntryUnknownKeys) ||
		!strings.Contains(evs[0], "scaleUpThreshhold") {
		t.Errorf("want one ConfigEntryUnknownKeys event naming the key, got %v", evs)
	}
	if got := gaugeValue(t, registry, constants.WVAConfigUnknownKeys); got != 1 {
		t.Errorf("%s = %v, want 1", constants.WVAConfigUnknownKeys, got)
	}
}

// bootstrapWith runs the startup ConfigMap load against the given ConfigMaps.
func (f *rejectsFixture) bootstrapWith(cms ...*corev1.ConfigMap) error {
	f.t.Helper()
	for _, cm := range cms {
		if err := f.c.Create(context.Background(), cm); err != nil {
			f.t.Fatal(err)
		}
	}
	return f.r.BootstrapInitialConfigMaps(context.Background())
}

func policyCM(namespace, defaultEntry string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: config.ScalingPolicyConfigMapName()},
		Data:       map[string]string{config.GlobalDefaultsKey: defaultEntry},
	}
}

// TestInvalidDefaultAtStartupRefusesToStart: with nothing earlier to keep, an
// invalid "default" in the ConfigMap the limiters come from would start the
// controller without its GPU bound. It must refuse to start instead.
func TestInvalidDefaultAtStartupRefusesToStart(t *testing.T) {
	f := newRejectsFixture(t)
	err := f.bootstrapWith(policyCM(rejectsSystemNS, invalidQuotaDefault))
	if err == nil || !strings.Contains(err.Error(), "refusing to start unbounded") {
		t.Fatalf("startup with an invalid default: err = %v, want a refusal", err)
	}
}

func TestInvalidClusterPolicyAtStartupRefusesToStart(t *testing.T) {
	f := newRejectsFixture(t)
	f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
	err := f.bootstrapWith(policyCM(rejectsPolicyNS, invalidQuotaDefault))
	if err == nil || !strings.Contains(err.Error(), "refusing to start unbounded") {
		t.Fatalf("startup with an invalid cluster policy: err = %v, want a refusal", err)
	}
}

// TestStartupNotStoppedByEntriesThatCarryNoBound: only the ConfigMap the
// limiters are read from may stop startup. The controller's own ConfigMap,
// while a policy namespace is separate, carries no bound; an unknown key
// rejects nothing.
func TestStartupNotStoppedByEntriesThatCarryNoBound(t *testing.T) {
	f := newRejectsFixture(t)
	f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
	if err := f.bootstrapWith(
		policyCM(rejectsPolicyNS, typoQuotaDefault),
		policyCM(rejectsSystemNS, invalidQuotaDefault),
	); err != nil {
		t.Fatalf("startup stopped: %v", err)
	}
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Errorf("limiter mode %q, want the cluster policy's quota", got)
	}
}

// TestInvalidClusterPolicyKeepsTheBound: in a separate policy namespace a
// default entry that does not parse used to call UpdateClusterPolicy(nil),
// which hands limiter authority back to the controller's own (tenant-writable)
// ConfigMap.
func TestInvalidClusterPolicyKeepsTheBound(t *testing.T) {
	f := newRejectsFixture(t)
	f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)

	f.apply(rejectsPolicyNS, map[string]string{config.GlobalDefaultsKey: quotaDefault})
	if limiterSet(f.cfg) != string(config.LimiterTypeQuota) {
		t.Fatalf("setup: limiter mode %q, want quota", limiterSet(f.cfg))
	}

	_ = f.events()
	f.apply(rejectsPolicyNS, map[string]string{config.GlobalDefaultsKey: invalidQuotaDefault})
	if evs := f.events(); len(evs) != 1 || !strings.Contains(evs[0], "previous entry") {
		t.Errorf("want one event saying the previous cluster policy stays, got %v", evs)
	}
	if !f.cfg.ClusterPolicyIsSeparate() {
		t.Error("an invalid cluster policy handed limiter authority back to the controller's own ConfigMap")
	}
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeQuota) {
		t.Errorf("an invalid cluster policy lifted the quota: mode %q", got)
	}
}

// TestClusterPolicyWithoutDefaultStaysSeparate: a policy ConfigMap with no
// default entry declares no bound, but authority must stay with the policy
// namespace, as on deletion.
func TestClusterPolicyWithoutDefaultStaysSeparate(t *testing.T) {
	f := newRejectsFixture(t)
	f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
	// A limiter in the controller's own ConfigMap, which carries no authority
	// while policy is read from elsewhere.
	f.apply(rejectsSystemNS, map[string]string{config.GlobalDefaultsKey: quotaDefault})

	f.apply(rejectsPolicyNS, map[string]string{"some-tier": "scaleUpThreshold: 0.8\n"})
	if !f.cfg.ClusterPolicyIsSeparate() {
		t.Error("a policy ConfigMap without a default entry handed authority back to the controller's own ConfigMap")
	}
	if got := limiterSet(f.cfg); got != string(config.LimiterTypeNone) {
		t.Errorf("limiter mode %q, want none: the controller's own limiters carry no authority", got)
	}
}

func gaugeValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range families {
		if fam.GetName() == name {
			total := 0.0
			for _, m := range fam.GetMetric() {
				total += m.GetGauge().GetValue()
			}
			return total
		}
	}
	return -1
}

// TestDeletedConfigMapClearsItsEntryGauges: the rejected-entry and unknown-key
// gauges are set on every load, and a deleted ConfigMap is never loaded again,
// so its last counts used to be reported for as long as the controller ran.
func TestDeletedConfigMapClearsItsEntryGauges(t *testing.T) {
	for _, ns := range []string{rejectsSystemNS, rejectsPolicyNS} {
		t.Run(ns, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			if err := metrics.InitMetrics(registry); err != nil {
				t.Fatal(err)
			}
			f := newRejectsFixture(t)
			if ns == rejectsPolicyNS {
				f.cfg.SetPolicyNamespace(rejectsPolicyNS, config.PolicySourceWellKnown)
			}
			f.apply(ns, map[string]string{config.GlobalDefaultsKey: quotaDefault})
			f.apply(ns, map[string]string{config.GlobalDefaultsKey: typoQuotaDefault, "some-tier": invalidQuotaDefault})
			if got := gaugeValue(t, registry, constants.WVAConfigRejectedEntries); got != 1 {
				t.Fatalf("setup: %s = %v, want 1", constants.WVAConfigRejectedEntries, got)
			}
			if got := gaugeValue(t, registry, constants.WVAConfigUnknownKeys); got != 1 {
				t.Fatalf("setup: %s = %v, want 1", constants.WVAConfigUnknownKeys, got)
			}

			ctx := context.Background()
			key := types.NamespacedName{Namespace: ns, Name: config.ScalingPolicyConfigMapName()}
			if err := f.c.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace, Name: key.Name}}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{constants.WVAConfigRejectedEntries, constants.WVAConfigUnknownKeys} {
				if got := gaugeValue(t, registry, name); got != -1 {
					t.Errorf("%s still reported after the ConfigMap was deleted: %v", name, got)
				}
			}
		})
	}
}
