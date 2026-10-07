package scalefromzero

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"gopkg.in/yaml.v3"
)

func shareConfig(t *testing.T, shadow bool) *config.Config {
	t.Helper()
	doc := "limiters:\n  - type: quota\n    name: q\n    scope: cluster\n    quotas:\n      A100: 16\n" +
		"optimizer:\n  type: utilizationShare\n"
	if shadow {
		doc += "  utilizationShare:\n    shadow: true\n"
	}
	var p config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	c := config.NewTestConfig()
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
	return c
}

// A wake with no capacity claims a releasing transfer with its cheapest decode
// variant when the optimizer acts; in shadow mode it claims nothing.
func TestClaimShareTransfer(t *testing.T) {
	group := modelGroup{namespace: selNS, modelID: "m"}
	candidates := []Candidate{
		cand("pricey", domain.RoleDecode, "A100", 1, 9),
		cand("cheap", domain.RoleDecode, "A100", 1, 1),
		cand("pre", domain.RolePrefill, "A100", 1, 0),
	}
	publish := func() {
		decision.DefaultShareClaims.Take("", "A100")
		decision.DefaultShareClaims.Publish(map[string][]decision.ShareClaimable{
			decision.ShareGroupKey("", "A100"): {{ID: "t1", ReceiverZ: -0.3, DonorGPUs: 1}},
		}, time.Now())
	}

	publish()
	e := &Engine{config: shareConfig(t, true)}
	if _, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, coverage{}); ok {
		t.Fatal("shadow mode claimed a transfer")
	}

	publish()
	e = &Engine{config: shareConfig(t, false)}
	set, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, coverage{})
	if !ok || len(set) != 1 || set[0].VariantName != "cheap" {
		t.Fatalf("want the cheapest decode variant woken by a claim, got %v %+v", ok, set)
	}
	if got := decision.DefaultShareClaims.Take("", "A100"); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("want claim t1 recorded for the steady-state engine, got %v", got)
	}
}

func pdShareConfig(t *testing.T) *config.Config {
	t.Helper()
	doc := "limiters:\n  - type: quota\n    name: q\n    scope: cluster\n    quotas:\n      A100: 16\n" +
		"optimizer:\n  type: utilizationShare\n" +
		"scaleFromZero:\n  requirePrefill: true\n"
	var p config.ScalingPolicy
	if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	c := config.NewTestConfig()
	c.UpdateScalingPolicyConfig(map[string]config.ScalingPolicy{config.GlobalDefaultsKey: p})
	return c
}

// A model that must wake a prefill with its decode claims two releasing
// transfers, one per role, or none: half a P/D pair serves nothing. A decode
// already serving is not claimed for again.
func TestClaimShareTransferForAPDPair(t *testing.T) {
	group := modelGroup{namespace: selNS, modelID: "m"}
	candidates := []Candidate{
		cand("dec", domain.RoleDecode, "A100", 1, 1),
		cand("pre", domain.RolePrefill, "A100", 1, 1),
	}
	publish := func(n int) {
		decision.DefaultShareClaims.Take("", "A100")
		entries := make([]decision.ShareClaimable, 0, n)
		for i := range n {
			entries = append(entries, decision.ShareClaimable{ID: fmt.Sprint("t", i), ReceiverZ: -0.3, DonorGPUs: 1})
		}
		decision.DefaultShareClaims.Publish(map[string][]decision.ShareClaimable{
			decision.ShareGroupKey("", "A100"): entries,
		}, time.Now())
	}
	if !(&Engine{config: pdShareConfig(t)}).requirePrefill("m", selNS) {
		t.Fatal("setup: requirePrefill did not resolve")
	}

	for _, tc := range []struct {
		name     string
		releases int
		covered  coverage
		want     []string
	}{
		{"one transfer releasing: neither role claims", 1, coverage{}, nil},
		{"two releasing: both roles claim", 2, coverage{}, []string{"dec", "pre"}},
		{"decode serving: only the prefill claims", 1, coverage{decode: true}, []string{"pre"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh store per case: a claim holds the model for two minutes.
			saved := decision.DefaultShareClaims
			decision.DefaultShareClaims = &decision.ShareClaimStore{}
			t.Cleanup(func() { decision.DefaultShareClaims = saved })
			publish(tc.releases)
			e := &Engine{config: pdShareConfig(t)}
			set, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, tc.covered)
			got := make([]string, 0, len(set))
			for _, c := range set {
				got = append(got, c.VariantName)
			}
			if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("claimed %v (%v), want %v", got, ok, tc.want)
			}
			if claims := decision.DefaultShareClaims.Take("", "A100"); len(claims) != len(tc.want) {
				t.Fatalf("recorded %d claims, want %d: a refused pair must claim nothing", len(claims), len(tc.want))
			}
		})
	}
}

// A P/D pair claims in one group: a prefill on another accelerator, or on one
// not resolved, cannot share the decode's claim set.
func TestClaimShareTransferNeedsOneAccelerator(t *testing.T) {
	group := modelGroup{namespace: selNS, modelID: "m"}
	for _, tc := range []struct {
		name    string
		prefill Candidate
		want    bool
	}{
		{"same accelerator (control)", cand("pre", domain.RolePrefill, "A100", 1, 1), true},
		{"another accelerator", cand("pre", domain.RolePrefill, "H100", 1, 1), false},
		{"unresolved accelerator", cand("pre", domain.RolePrefill, "", 1, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := decision.DefaultShareClaims
			decision.DefaultShareClaims = &decision.ShareClaimStore{}
			t.Cleanup(func() { decision.DefaultShareClaims = saved })
			decision.DefaultShareClaims.Publish(map[string][]decision.ShareClaimable{
				decision.ShareGroupKey("", "A100"): {{ID: "t0", ReceiverZ: -0.3, DonorGPUs: 1}, {ID: "t1", ReceiverZ: -0.3, DonorGPUs: 1}},
				decision.ShareGroupKey("", "H100"): {{ID: "h0", ReceiverZ: -0.3, DonorGPUs: 1}},
			}, time.Now())
			e := &Engine{config: pdShareConfig(t)}
			candidates := []Candidate{cand("dec", domain.RoleDecode, "A100", 1, 1), tc.prefill}
			if _, ok := e.claimShareTransfer(context.Background(), group, candidates, nil, coverage{}); ok != tc.want {
				t.Fatalf("claimed %v, want %v", ok, tc.want)
			}
		})
	}
}
