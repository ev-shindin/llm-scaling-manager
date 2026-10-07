package scalefromzero

import (
	"context"
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
	if _, ok := e.claimShareTransfer(context.Background(), group, candidates); ok {
		t.Fatal("shadow mode claimed a transfer")
	}

	publish()
	e = &Engine{config: shareConfig(t, false)}
	c, ok := e.claimShareTransfer(context.Background(), group, candidates)
	if !ok || c.VariantName != "cheap" {
		t.Fatalf("want the cheapest decode variant woken by a claim, got %v %+v", ok, c)
	}
	if got := decision.DefaultShareClaims.Take("", "A100"); len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("want claim t1 recorded for the steady-state engine, got %v", got)
	}
}
