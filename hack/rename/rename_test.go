package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// docsDir keeps fixture paths from reading as links to hack/check-doc-links.py.
const docsDir = "doc" + "s/"

func newEngine(t *testing.T) *Engine {
	t.Helper()
	e, err := NewEngine(DefaultTargets, []string{"wva_desired_replicas", "wva_load_imbalanced"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestRewriteText(t *testing.T) {
	cases := []struct{ name, path, in, want string }{
		{"metric", docsDir + "reference/x.md", "sum(wva_desired_replicas{a=\"b\"})", "sum(llmsm_desired_replicas{a=\"b\"})"},
		{"metric with a suffix", "config/x.yaml", "wva_load_imbalanced_bucket", "llmsm_load_imbalanced_bucket"},
		{"env", "deploy/x.sh", "${WVA_NS:-x} $WVA_SCOPE", "${LLMSM_NS:-x} $LLMSM_SCOPE"},
		{"env after an escape", "hack/x.sh", `printf 'a\nWVA_B=%q'`, `printf 'a\nLLMSM_B=%q'`},
		{"env inside a name", "deploy/x.sh", "A_WVA_B C_WVA", "A_LLMSM_B C_SCALING_MANAGER"},
		{"namespace", "config/x.yaml", "namespace: workload-variant-autoscaler-system", "namespace: scaling-manager-system"},
		{"short namespace", "config/x.yaml", "namespace: wva-system", "namespace: scaling-manager-system"},
		{"make target", docsDir + "guides/a/README.md", "make deploy-wva and undeploy-wva", "make deploy-scaling-manager and undeploy-scaling-manager"},
		{"alert", "config/x.yaml", "- alert: WVAGPUResourceExhausted", "- alert: LLMSMGPUResourceExhausted"},
		{"anchor", "config/x.yaml", "troubleshooting.md#wvakueuereaderstale", "troubleshooting.md#llmsmkueuereaderstale"},
		{"an object after a separator", "hack/x.sh", "$STORE/deployment__wva-controller-manager.json", "$STORE/deployment__scaling-manager-controller-manager.json"},
		{"shell function", "deploy/x.sh", "deploy_wva_controller", "deploy_scaling_manager_controller"},
		{"prose after the", docsDir + "a.md", "ask the WVA now", "ask the scaling manager now"},
		{"prose after every", docsDir + "a.md", "every WVA here", "every scaling manager here"},
		{"prose mid sentence", docsDir + "a.md", "so WVA scales", "so the scaling manager scales"},
		{"prose at a line start", docsDir + "a.md", "WVA scales", "The scaling manager scales"},
		{"prose after a stop", docsDir + "a.md", "It runs. WVA scales", "It runs. The scaling manager scales"},
		{"prose in a heading", docsDir + "a.md", "## WVA install", "## Scaling manager install"},
		{"prose in a table", docsDir + "a.md", "| WVA | x |", "| The scaling manager | x |"},
		{"project name", docsDir + "a.md", "use llm-scaling-manager here", "use llm-d-scaling-manager here"},

		{"a key domain moves", "config/x.yaml", "wva.llmd.ai/policy-namespace: a", "scaling-manager.llmd.ai/policy-namespace: a"},
		{"so does its escaped form", "deploy/x.sh", `{.metadata.labels.wva\.llmd\.ai/x}`, `{.metadata.labels.scaling-manager\.llmd\.ai/x}`},
		{"and the other spelling", "deploy/x.sh", "wva.llm-d.ai/pool", "scaling-manager.llm-d.ai/pool"},
		{"the policy namespace moves", "deploy/x.sh", "ns=wva-policy", "ns=scaling-manager-policy"},
		{"learned state moves", docsDir + "a.md", "ConfigMap `wva-learned-state`", "ConfigMap `scaling-manager-learned-state`"},
		{"upstream issue stays", docsDir + "a.md", "https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1139", "https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1139"},
		{"other repository stays", docsDir + "a.md", "see ig-wva", "see ig-wva"},
		{"proxy image stays", "config/x.yaml", "ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11", "ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11"},
		{"a numbered link stays", docsDir + "a.md", "github.com/ev-shindin/llm-scaling-manager/pull/95", "github.com/ev-shindin/llm-scaling-manager/pull/95"},
		{"held link before the move", docsDir + "a.md", "https://github.com/ev-shindin/llm-scaling-manager/blob/main/x.md", "https://github.com/ev-shindin/llm-scaling-manager/blob/main/x.md"},
		{"held image before the move", "config/x.yaml", "ghcr.io/ev-shindin/llm-scaling-manager:main", "ghcr.io/ev-shindin/llm-scaling-manager:main"},
		{"a fork's clone URL is held", docsDir + "a.md", "git clone https://github.com/<you>/llm-scaling-manager.git", "git clone https://github.com/<you>/llm-scaling-manager.git"},
		{"an applied-by mark moves", "deploy/x.sh", "llm-d.ai/applied-by=wva-epp-metrics-auth", "llm-d.ai/applied-by=scaling-manager-epp-metrics-auth"},
		{"with the bindings named after it", "hack/x.sh", "crb wva-epp-metrics-auth-apps-0000aaaa", "crb scaling-manager-epp-metrics-auth-apps-0000aaaa"},
		{"the field manager moves", "deploy/x.sh", "--field-manager=llm-scaling-manager", "--field-manager=llm-d-scaling-manager"},
		{"a node directory moves", docsDir + "a.md", "/mnt/local/wva-weights", "/mnt/local/scaling-manager-weights"},
		{"the harness schema stays", "Makefile", `select(has("wva")) | .wva.hpa.minReplicas`, `select(has("wva")) | .wva.hpa.minReplicas`},
		{"its key stays", "hack/benchmark/scenarios/guides/a.yaml", "    wva:\n      enabled: true\n", "    wva:\n      enabled: true\n"},
		{"a value in that file still moves", "hack/benchmark/scenarios/guides/a.yaml", "name: wva-a", "name: scaling-manager-a"},
		{"the harness text stays", "hack/benchmark/patch_harness.sh", `if "WVA was never installed" not in second:`, `if "WVA was never installed" not in second:`},
		{"a dotted name is not the harness schema", "hack/x.sh", "scaler.wva-ctl.svc", "scaler.scaling-manager-ctl.svc"},
		{"history is in scope", docsDir + "proposals/a.md", "WVA and wva_foo and wva-bar", "The scaling manager and llmsm_foo and scaling-manager-bar"},
		{"a file name in history moves", docsDir + "plans/a.md", "see proposals/wva-keda-external-scaler.md", "see proposals/scaling-manager-keda-external-scaler.md"},
		{"a before a consonant", docsDir + "a.md", "a WVA here", "a scaling manager here"},
		{"an drops its n", docsDir + "a.md", "an wva-x here", "a scaling-manager-x here"},
		{"a before a name said el", docsDir + "a.md", "a wva_x metric and a `WVA_NS`", "an llmsm_x metric and an `LLMSM_NS`"},
		{"the kind in text", docsDir + "a.md", "the VariantAutoscaling and its VariantAutoscalingSpec", "the Variant and its variant.Spec"},
		{"a qualified kind in text", docsDir + "a.md", "variant.VariantAutoscalingStatus", "variant.Status"},
		{"the harness key stays", "hack/benchmark/scenarios/guides/a.yaml", "      variantAutoscaling:", "      variantAutoscaling:"},
		{"after own", docsDir + "a.md", "the scenario's own WVA install", "the scenario's own scaling manager install"},
		{"after both", docsDir + "a.md", "both WVA roles", "both scaling manager roles"},
		{"after whose", docsDir + "a.md", "a dashboard whose WVA panels", "a dashboard whose scaling manager panels"},
		{"a plural after several", docsDir + "a.md", "several WVAs", "several scaling managers"},
		{"running takes the article", docsDir + "a.md", "a tenant running WVA", "a tenant running the scaling manager"},
		{"an alert prefix in a query", docsDir + "a.md", `select(.labels.alertname | startswith("WVA"))`, `select(.labels.alertname | startswith("LLMSM"))`},
		{"an alert regex", docsDir + "a.md", `alertname=~"WVA.*"`, `alertname=~"LLMSM.*"`},
		{"a name ending in -a is not an article", "hack/x.sh", "tenant-a WVA_QUOTAS", "tenant-a LLMSM_QUOTAS"},
		{"the other product stays", docsDir + "proposals/a.md", "upstream WVA #1003", "upstream WVA #1003"},
		{"an adjective keeps its article", docsDir + "a.md", "a wrong WVA namespace", "a wrong scaling manager namespace"},
		{"a continuation is not a sentence start", "deploy/x.sh", "# it reaches\n# WVA next", "# it reaches\n# the scaling manager next"},
		{"a new sentence on a new line is", "deploy/x.sh", "# it stopped.\n# WVA next", "# it stopped.\n# The scaling manager next"},
		{"a comment hash is not a heading", "deploy/x.sh", "# WVA next", "# The scaling manager next"},
		{"a generated name takes the short suffix", "deploy/x.sh", "name: ${target}-wva", "name: ${target}-llmsm"},
		{"a make target takes the word", docsDir + "a.md", "make deploy-wva", "make deploy-scaling-manager"},
		{"a pool name takes the short prefix", "deploy/x.sh", "wva-warm-pool-${POOL_NAME}", "llmsm-warm-pool-${POOL_NAME}"},
		{"a tenant namespace takes the short prefix", docsDir + "a.md", "`LLMSM_NS=wva-<ns>`", "`LLMSM_NS=llmsm-<ns>`"},
		{"the common name keeps its variable", "deploy/lib/scaler_tls.sh", `serving "wva-external-scaler.${WVA_NS}.svc"`, `serving "llmsm-external-scaler.${LLMSM_NS}.svc"`},
		{"the cert-manager common name too", "deploy/lib/scaler_tls.sh", "wva-x\ncommonName: ${dns1}", "scaling-manager-x\ncommonName: llmsm-external-scaler.${LLMSM_NS}.svc"},
		{"the controller regex is anchored", "deploy/grafana/x.json", `deployment=~\".*workload-variant-autoscaler.*|wva-controller-manager\"`, `deployment=~\"scaling-manager-controller-manager\"`},
		{"another person's fork stays", docsDir + "proposals/a.md", "### `deanlorenz/llm-d-workload-variant-autoscaler` -- a fork", "### `deanlorenz/llm-d-workload-variant-autoscaler` -- a fork"},
		{"an upstream issue stays", docsDir + "proposals/a.md", "[llm-d WVA #1003](x)", "[llm-d WVA #1003](x)"},
		{"the launcher owner stays", docsDir + "guides/warm-pool/install.md", "from `ghcr.io/ev-shindin`.", "from `ghcr.io/ev-shindin`."},
		{"a harness flag stays", "hack/benchmark/scenarios/guides/workload-autoscaling.yaml", "pass --wva to it", "pass --wva to it"},
		{"the old CRD is not a Variant CRD", docsDir + "a.md", "the VariantAutoscaling CRD and a VariantAutoscaling reconciler", "the old CRD and the old reconciler"},
		{"no doubled article across a line", "deploy/x.sh", "# wait until the\n# VariantAutoscaling CRD goes", "# wait until the\n# old CRD goes"},
		{"a name built from a namespace", "hack/x.sh", "wva-external-scaler-wva", "scaling-manager-external-scaler-scaling-manager"},
		{"an article at the end of the line above", "deploy/x.sh", "# used by the\n# WVA controller", "# used by the\n# scaling manager controller"},
		{"a recorded document stays", "internal/engines/steadystate/testdata/learned-state/v0.10.0-v1.json", `{"namespace":"wva-system"}`, `{"namespace":"wva-system"}`},
		{"a namespace called wva is not the harness schema", "hack/x.sh", "wva-external-scaler.wva.svc", "scaling-manager-external-scaler.scaling-manager.svc"},
		{"a shell default moves whole", "deploy/x.sh", "${POLICY_NS:-wva-policy}", "${POLICY_NS:-scaling-manager-policy}"},
		{"so does the unset-only form", "deploy/x.sh", "${POLICY_NS-wva-policy}", "${POLICY_NS-scaling-manager-policy}"},
		{"the assign form", "deploy/x.sh", "${POLICY_NS:=wva-policy}", "${POLICY_NS:=scaling-manager-policy}"},
		{"an HPA metric index moves with its metric", "hack/x.sh", "s0-wva-desired-replicas", "s0-scaling-manager-desired-replicas"},
		{"the harness attribute stays", "hack/benchmark/patch_harness.sh", `"wva_namespace"`, `"wva_namespace"`},
		{"a hyphen-free word stays out of residual", docsDir + "a.md", "newValue windowVariants", "newValue windowVariants"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEngine(t)
			got := string(e.RewriteText(c.path, classify(c.path), CtxText, []byte(c.in), 0))
			if got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
			if n := e.Stats.UnclassifiedCount(); n != 0 {
				t.Errorf("unclassified: %v", e.Stats.unclassified)
			}
			again := string(e.RewriteText(c.path, classify(c.path), CtxText, []byte(got), 0))
			if again != got {
				t.Errorf("not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestCodeMovedTurnsHeldRowsOn(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://github.com/ev-shindin/llm-scaling-manager/blob/main/x.md", "https://github.com/llm-d-incubation/llm-d-scaling-manager/blob/main/x.md"},
		{"git@github.com:ev-shindin/llm-scaling-manager.git", "git@github.com:llm-d-incubation/llm-d-scaling-manager.git"},
		{"ghcr.io/ev-shindin/llm-scaling-manager:main", "ghcr.io/llm-d-incubation/llm-d-scaling-manager:main"},
		{"IMAGE_TAG_BASE ?= ghcr.io/ev-shindin\n", "IMAGE_TAG_BASE ?= ghcr.io/llm-d-incubation\n"},
		{"$(IMAGE_TAG_BASE)/llm-scaling-manager:x", "$(IMAGE_TAG_BASE)/llm-d-scaling-manager:x"},
		{"  cd llm-scaling-manager\n", "  cd llm-d-scaling-manager\n"},
		{"github.com/ev-shindin/llm-scaling-manager/pull/95", "github.com/ev-shindin/llm-scaling-manager/pull/95"},
		{"ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11", "ghcr.io/ev-shindin/llm-scaler/warmpool-proxy:v11"},
	}
	targets := DefaultTargets
	targets.CodeMoved = true
	e, err := NewEngine(targets, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		p := docsDir + "a.md"
		if strings.Contains(c.in, "IMAGE_TAG_BASE ?=") {
			p = "Makefile"
		}
		if got := string(e.RewriteText(p, ClassDocs, CtxText, []byte(c.in), 0)); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestModuleIsOneSwitch(t *testing.T) {
	const src = "package a\n\nimport \"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/x\"\n\nvar _ = x.Y\n"
	for _, module := range []string{"github.com/llm-d-incubation/llm-d-scaling-manager", "example.com/other/place"} {
		e, err := NewEngine(Targets{Module: module}, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := e.RewriteGo("internal/a/a.go", ClassGo, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if want := `import "` + module + `/internal/x"`; !strings.Contains(string(out), want) {
			t.Errorf("module %s: %s", module, out)
		}
	}
	// An earlier target moves on too, so a later change of mind is one edit.
	e := newEngine(t)
	out, err := e.RewriteGo("internal/a/a.go", ClassGo, []byte(strings.ReplaceAll(src, "llm-d/llm-d-workload-variant-autoscaler", "ev-shindin/llm-scaling-manager")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), DefaultTargets.Module+"/internal/x") {
		t.Errorf("an earlier target stayed: %s", out)
	}
}

const goFixture = `// Package a is about WVA.
package a

import (
	"fmt"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/zed/pkg"
)

// WVAThing is a thing. WVA owns it.
type WVAThing struct {
	Name string ` + "`json:\"name\"`" + `
	Long int    // keeps its column
}

const (
	short          = "wva-a"
	wvaNamespace   = "workload-variant-autoscaler-system"
	policyName     = "wva-policy"
	metric         = "wva_desired_replicas"
	unrelatedValue = "newValue"
)

func f(req int) string {
	fmt.Println(constants.WVADesiredReplicas, wvav1alpha1.X, pkg.Y)
	return ` + "`raw wva-b\nWVA_NS=1`" + `
}
`

func TestRewriteGo(t *testing.T) {
	e := newEngine(t)
	out, err := e.RewriteGo("internal/a/a.go", ClassGo, []byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"// Package a is about the scaling manager.",
		"// LLMSMThing is a thing. The scaling manager owns it.",
		"type LLMSMThing struct",
		`llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/api/v1alpha1"`,
		`"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"`,
		"constants.LLMSMDesiredReplicas, llmdvariant.X, pkg.Y",
		`short                   = "scaling-manager-a"`,
		`scalingManagerNamespace = "scaling-manager-system"`,
		`policyName              = "scaling-manager-policy"`,
		`metric                  = "llmsm_desired_replicas"`,
		"raw scaling-manager-b\nLLMSM_NS=1",
		"func f(req int) string {",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if e.Stats.UnclassifiedCount() != 0 {
		t.Errorf("unclassified: %v", e.Stats.unclassified)
	}
	again, err := e.RewriteGo("internal/a/a.go", ClassGo, out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, out) {
		t.Errorf("not idempotent:\n%s", again)
	}
}

func TestVerifyGoCatchesWhatTheTableDidNotDo(t *testing.T) {
	const src = "package a\n\nconst a = \"wva-x\"\n\nvar b = f(req, 25)\n"
	cases := map[string]string{
		"a value drifted":                  "package a\n\nconst a = \"scaling-manager-x\"\n\nvar b = f(req, 26)\n",
		"an argument became a literal":     "package a\n\nconst a = \"scaling-manager-x\"\n\nvar b = f(25000, 25)\n",
		"a literal went off the table":     "package a\n\nconst a = \"something-else\"\n\nvar b = f(req, 25)\n",
		"a token was added at the end":     "package a\n\nconst a = \"scaling-manager-x\"\n\nvar b = f(req, 25)\n\nvar c = 1\n",
		"a token vanished":                 "package a\n\nconst a = \"scaling-manager-x\"\n\nvar b = f(req)\n",
		"an identifier was renamed freely": "package a\n\nconst z = \"scaling-manager-x\"\n\nvar b = f(req, 25)\n",
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			if err := newEngine(t).verifyGo("a.go", []byte(src), []byte(after)); err == nil {
				t.Error("verify passed")
			}
		})
	}
	good := "package a\n\nconst a = \"scaling-manager-x\"\n\nvar b = f(req, 25)\n"
	if err := newEngine(t).verifyGo("a.go", []byte(src), []byte(good)); err != nil {
		t.Errorf("verify refused the right rewrite: %v", err)
	}
}

func TestRewriteGoStopsWhenVerifyRefuses(t *testing.T) {
	e := newEngine(t)
	e.verify = func(string, []byte, []byte) error { return errors.New("refused") }
	if _, err := e.RewriteGo("internal/a/a.go", ClassGo, []byte(goFixture)); err == nil {
		t.Fatal("a rewrite verify refused went through")
	}
}

// repo builds a small git repository for the end-to-end tests.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.autocrlf=false", "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "x")
	return dir
}

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestApplyEndToEnd(t *testing.T) {
	recording := gzipped(t, "wva_desired_replicas 1\n")
	dir := repo(t, map[string]string{
		"internal/constants/metrics.go": "package constants\r\n\r\n// WVADesiredReplicas is a metric.\r\nconst WVADesiredReplicas = \"wva_desired_replicas\"\r\n",
		"deploy/lib/infra_wva.sh":       "#!/bin/bash\nWVA_NS=wva-system\n",
		docsDir + "a.md":                "WVA line\r\nsee deploy/lib/infra_wva.sh\r\n",
		docsDir + "plans/old.md":        "WVA moves\n",
		"data/recording.jsonl.gz":       recording,
		"data/nul.bin":                  "wva\x00wva",
	})
	report := filepath.Join(t.TempDir(), "r.md")
	var out bytes.Buffer

	// A dry run changes nothing and says it would.
	if err := run([]string{"--root", dir, "--report", report}, &out); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, docsDir+"a.md")); !bytes.Contains(b, []byte("WVA line")) {
		t.Fatal("a dry run wrote a file")
	}
	if err := run([]string{"--root", dir, "--report", report, "--expect-noop"}, &out); err == nil {
		t.Fatal("--expect-noop passed on a tree with work to do")
	}

	if err := run([]string{"--root", dir, "--report", report, "--apply"}, &out); err != nil {
		t.Fatal(err)
	}

	// Line endings survive per file, in both directions.
	b, err := os.ReadFile(filepath.Join(dir, "internal/constants/metrics.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("const LLMSMDesiredReplicas = \"llmsm_desired_replicas\"\r\n")) {
		t.Errorf("the Go file lost CRLF or was not rewritten: %q", b)
	}
	if bytes.Count(b, []byte("\n")) != bytes.Count(b, []byte("\r\n")) {
		t.Errorf("mixed endings: %q", b)
	}
	if b, _ = os.ReadFile(filepath.Join(dir, docsDir+"a.md")); string(b) != "The scaling manager line\r\nsee deploy/lib/infra_scaling_manager.sh\r\n" {
		t.Errorf(docsDir+"a.md: %q", b)
	}

	// The move kept content and mode, and git knows about it.
	moved := filepath.Join(dir, "deploy/lib/infra_scaling_manager.sh")
	b, err = os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "#!/bin/bash\nLLMSM_NS=scaling-manager-system\n" {
		t.Errorf("moved file: %q", b)
	}
	if info, _ := os.Stat(moved); info.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode lost: %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy/lib/infra_wva.sh")); !os.IsNotExist(err) {
		t.Errorf("the old path is still there: %v", err)
	}
	staged, _ := exec.Command("git", "-C", dir, "ls-files", "deploy/lib").Output()
	if !strings.Contains(string(staged), "infra_scaling_manager.sh") {
		t.Errorf("the index does not have the move: %s", staged)
	}

	// Binary and compressed files are byte-identical.
	if b, _ = os.ReadFile(filepath.Join(dir, "data/recording.jsonl.gz")); string(b) != recording {
		t.Error("the recording changed")
	}
	if b, _ = os.ReadFile(filepath.Join(dir, "data/nul.bin")); string(b) != "wva\x00wva" {
		t.Error("a binary file changed")
	}
	if b, _ = os.ReadFile(filepath.Join(dir, docsDir+"plans/old.md")); string(b) != "The scaling manager moves\n" {
		t.Errorf("history did not move: %q", b)
	}

	// A second run is a no-op.
	if err := run([]string{"--root", dir, "--report", report, "--expect-noop"}, &out); err != nil {
		t.Errorf("second run: %v", err)
	}
}

func TestApplyRefusesUnclassified(t *testing.T) {
	dir := repo(t, map[string]string{docsDir + "a.md": "xwvax in the middle of a word\n", docsDir + "b.md": "WVA\n"})
	report := filepath.Join(t.TempDir(), "r.md")
	err := run([]string{"--root", dir, "--report", report, "--apply"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unclassified") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, docsDir+"b.md")); string(b) != "WVA\n" {
		t.Error("a refused run wrote a file")
	}
	b, _ := os.ReadFile(report)
	if !bytes.Contains(b, []byte(docsDir+"a.md:1 [text] xwvax")) {
		t.Errorf("the report does not list it:\n%s", b)
	}
	if err := run([]string{"--root", dir, "--report", report, "--apply", "--allow-unclassified"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, docsDir+"b.md")); string(b) != "The scaling manager\n" {
		t.Errorf(docsDir+"b.md: %q", b)
	}
}

func TestEveryRuleCompilesAndIsNamedOnce(t *testing.T) {
	if _, err := compileRules(RulesFor(DefaultTargets, []string{"wva_a"})); err != nil {
		t.Fatal(err)
	}
	if _, err := compileRules(RulesFor(DefaultTargets, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestMixedLineEndingsInGoAreRefused(t *testing.T) {
	e := newEngine(t)
	_, err := e.rewriteFile("a.go", ClassGo, []byte("package a\r\n\n// WVA\n"))
	if err == nil {
		t.Fatal("a Go file with mixed endings went through gofmt")
	}
}

func TestRewriteGoVariantNames(t *testing.T) {
	const src = `package a

import llmdVariantAutoscalingV1alpha1 "example.com/internal/variant"

// VariantAutoscalingList holds them.
type T struct {
	llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec
	Items []llmdVariantAutoscalingV1alpha1.VariantAutoscaling ` + "`json:\"items\"`" + `
}

func GroupVariantAutoscalingByModel(variantAutoscalings []T) {}
func readyVariantAutoscalings()                       {}
func ActiveVariantAutoscaling()                       {}
func InactiveVariantAutoscalingByModel()              {}
`
	out, err := newEngine(t).RewriteGo("internal/a/a.go", ClassGo, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"llmdvariant.Spec", "[]llmdvariant.Variant `json:\"items\"`", "// List holds them.",
		"func GroupVariantsByModel(variants []T)", "func readyVariants()", "func ActiveVariants()", "func InactiveVariantsByModel()"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSweepGivesEveryHitAReason(t *testing.T) {
	dir := repo(t, map[string]string{
		"hack/rename/x.go": "// wva\n",
		docsDir + "a.md":   "upstream WVA and newValue and a stray xWVAx\n",
	})
	e := newEngine(t)
	files, err := trackedFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.FinalSweep(dir, files)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]int{}
	for _, h := range s.Hits {
		reasons[h.Reason]++
	}
	if reasons[reasonTool] != 1 || reasons[reasonPartial] != 1 ||
		reasons["keep-upstream-prose"] != 1 || reasons[reasonNone] != 1 || s.Unexplained != 1 {
		t.Errorf("reasons = %v", reasons)
	}
	if err := run([]string{"--root", dir, "--report", filepath.Join(t.TempDir(), "r.md"), "--sweep"}, io.Discard); err == nil {
		t.Error("--sweep passed with a hit that has no reason")
	}
}

// TestReplacementsNameRealGroups catches a literal $ in a Replace: regexp.Expand
// reads ${LLMSM_NS} as a group that does not exist and writes nothing.
func TestReplacementsNameRealGroups(t *testing.T) {
	ref := regexp.MustCompile(`\$\$|\$\{?(\w+)\}?`)
	rules, err := compileRules(RulesFor(DefaultTargets, []string{"wva_a"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		for _, m := range ref.FindAllStringSubmatch(r.Replace, -1) {
			if m[0] == "$$" {
				continue
			}
			if _, err := strconv.Atoi(m[1]); err == nil {
				continue
			}
			if r.re.SubexpIndex(m[1]) < 0 {
				t.Errorf("rule %s: %s in Replace is not a group", r.Name, m[0])
			}
		}
	}
}
