package main

import (
	"regexp"
	"strings"
)

// Targets are the switches of a run.
type Targets struct {
	// Module is the Go module path, and the only place the target is written.
	// The two paths in oldModules are matched whatever it says, so a tree on
	// either moves to this value. A tree already on some other path is not
	// matched: add that path to oldModules to move it.
	Module string
	// CodeMoved turns the held rows on. See HoldUntilMoved.
	CodeMoved bool
}

// DefaultTargets is what a run does without flags.
var DefaultTargets = Targets{
	Module:    "github.com/llm-d-incubation/llm-d-scaling-manager",
	CodeMoved: false,
}

// oldModules are the module paths the module rows replace.
var oldModules = []string{"github.com/llm-d/llm-d-workload-variant-autoscaler", "github.com/ev-shindin/llm-scaling-manager"}

const (
	newRepoSlug = "llm-d-incubation/llm-d-scaling-manager"
	newImage    = "ghcr.io/llm-d-incubation/llm-d-scaling-manager"
	newName     = "llm-d-scaling-manager"
	kubeName    = "scaling-manager"
	metricPre   = "llmsm_"
	envPre      = "LLMSM_"

	// residualPattern is every spelling of an old name. Whatever it finds
	// outside a rule's claim is unclassified, and --apply refuses it.
	residualPattern = `wva|Wva|WVA|(?i:workload[-_ ]?variant|variant-autoscaler|variantautoscal)|llm-scaling-manager|ev-shindin|llm-d/llm-d-workload`

	// sweepPattern is what the final sweep looks for, in any case. Unlike the
	// residual pattern it also finds a capital inside a word, so nothing is
	// left out before a person has seen why.
	sweepPattern = `(?i)wva|workload-variant-autoscaler|workload_variant|variant-autoscaler|variantautoscaler`
)

// Path classes for the report.
const (
	ClassTool      = "tool"
	ClassGo        = "go"
	ClassGoTest    = "go-test"
	ClassGoE2E     = "go-e2e-support"
	ClassTestdata  = "testdata"
	ClassHistory   = "docs-history"
	ClassDocs      = "docs"
	ClassManifest  = "manifest"
	ClassDashboard = "dashboard"
	ClassShell     = "shell"
	ClassPython    = "python"
	ClassCI        = "ci"
	ClassBuild     = "build"
	ClassOther     = "other"
)

// classify gives a path its class.
func classify(p string) string {
	switch {
	case strings.HasPrefix(p, "hack/rename/"):
		return ClassTool
	case strings.Contains(p, "/testdata/"):
		return ClassTestdata
	case strings.HasSuffix(p, "_test.go"):
		return ClassGoTest
	case strings.HasPrefix(p, "test/") && strings.HasSuffix(p, ".go"):
		return ClassGoE2E
	case strings.HasSuffix(p, ".go"):
		return ClassGo
	case strings.HasPrefix(p, "docs/plans/"), strings.HasPrefix(p, "docs/proposals/"), strings.HasPrefix(p, "comparison/"):
		return ClassHistory
	case strings.HasSuffix(p, ".md"):
		return ClassDocs
	case strings.HasPrefix(p, ".github/"):
		return ClassCI
	case strings.HasSuffix(p, ".json") && strings.Contains(p, "dashboard"):
		return ClassDashboard
	case strings.HasPrefix(p, "config/") || strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".j2"):
		return ClassManifest
	case strings.HasSuffix(p, ".sh"):
		return ClassShell
	case strings.HasSuffix(p, ".py"):
		return ClassPython
	case p == "Makefile" || strings.HasPrefix(p, "Dockerfile") || p == "go.mod" || p == "PROJECT":
		return ClassBuild
	}
	return ClassOther
}

// binaryExtensions are never edited, whatever their bytes look like.
var binaryExtensions = []string{
	".gz", ".tgz", ".zip", ".png", ".jpg", ".jpeg", ".gif", ".ico", ".pdf",
	".docx", ".pptx", ".xlsx", ".so", ".a", ".bin", ".zst", ".xz",
}

// scalerTLS mints the external scaler's certificates.
var scalerTLS = []string{"deploy/lib/scaler_tls.sh"}

// keepWholeFiles keep every old name except the module path. The page about the
// rename has to name what it renames.
var keepWholeFiles = []string{
	"docs/developer-guide/renaming.md",
}

// RulesFor builds the table. Order is meaning: a row claims its text before
// the rows below it run, so what must stay comes first and the broad rows come
// last. metrics are the metric names read from internal/constants, so a metric
// is counted as a metric and not as a word.
func RulesFor(t Targets, metrics []string) []Rule {
	kept := func(name, class, pattern, why string, notPaths ...string) Rule {
		return Rule{Name: name, Class: class, Pattern: pattern, Action: Keep, Why: why, NotPaths: notPaths}
	}
	held := func(name, pattern, replace, why string) Rule {
		return Rule{Name: name, Class: "repo-link", Pattern: pattern, Replace: replace, Action: HoldUntilMoved, Why: why}
	}
	pathToo := func(r Rule) Rule {
		r.Contexts = []string{CtxText, CtxComment, CtxString, CtxPath}
		return r
	}

	rules := []Rule{
		// Links and paths into other repositories. They name someone else's
		// content, so renaming them breaks them.
		kept("keep-upstream-fork", "external", `github\.com/lionelvillard/[^\s)\]"'`+"`"+`>]*`, "another person's fork"),
		{Name: "hold-proxy-source", Class: "repo-link", Pattern: `https://github\.com/llm-d/llm-d-workload-variant-autoscaler`, Replace: "https://github.com/" + newRepoSlug,
			Action: HoldUntilMoved, Paths: []string{"Dockerfile.warmpool-proxy"}, Why: "the source label of an image built here"},
		kept("keep-upstream-url", "external", `https?://github\.com/llm-d/llm-d-workload-variant-autoscaler[^\s)\]"'`+"`"+`>]*`, "the upstream repository: issue and pull links must keep resolving"),
		// The Go module path: the one switch. It runs before every other
		// keep, because a kept file still has to import the renamed module.
		{Name: "module-path", Class: "module", Pattern: `(?:^|[^/.\w-])(?P<m>github\.com/(?:llm-d/llm-d-workload-variant-autoscaler|ev-shindin/llm-scaling-manager)(?:/internal|/api|/cmd|/test|/pkg)?)(?:$|[^\w.-])`,
			Fn: moduleSwap(t.Module), NotPaths: []string{"README.md", "CONTRIBUTING.md", "docs/**", ".github/**", "Dockerfile.warmpool-proxy", "OWNERS"},
			Contexts: []string{CtxText, CtxComment, CtxString, CtxImport}},
		{Name: "module-path-docs", Class: "module", Pattern: `github\.com/llm-d/llm-d-workload-variant-autoscaler/(?:internal|api|cmd|test|pkg)\b`,
			Fn: moduleSwap(t.Module), Contexts: []string{CtxText}},

		kept("keep-upstream-slug", "external", `[\w.-]+/llm-d-workload-variant-autoscaler`, "names llm-d's workload-variant-autoscaler repository, or a fork of it"),
		kept("keep-upstream-work", "external", `\[llm-d[^\]\n]*#[0-9]+\]`, "work and writing of the other project"),
		kept("keep-ig-wva", "external", `(?:github\.com/llm-d-incubation/)?ig-wva`, "a separate repository"),
		kept("keep-llmd-docs", "external", `llm-d\.ai/docs/architecture/Components/workload-variant-autoscaler|README\.wva\.md`, "a page in another project's docs"),
		kept("keep-upstream-prose", "external", `(?i:llm-d's) (?:workload-variant-autoscaler|WVA)(?: \(WVA\))?|workload-variant-autoscaler \(WVA\)|still named WVA|[Uu]pstream WVA|WVA upstream`, "names the other product"),

		// What an older release wrote, kept as it wrote it: a test reads these
		// to prove the new code still understands them.
		{Name: "keep-recorded-fixture", Class: "testdata", Pattern: residualPattern, Action: Keep, Contexts: []string{CtxText},
			Paths: []string{"internal/engines/steadystate/testdata/learned-state/**"},
			Why:   "written by an older release: the test is that this still loads"},

		// The test that reads those documents has to name their writer.
		{Name: "keep-recorded-writer", Class: "testdata", Pattern: `wva-system|wva-controller-[a-z0-9-]*`, Action: Keep, Contexts: []string{CtxString},
			Paths: []string{"internal/engines/steadystate/learned_state_backcompat_test.go"}, Why: "must equal the writer inside the recorded document"},

		// Four fixtures assert a sorted list, and the new name sorts before team-a
		// where the old one sorted after it. Three can take a name that still sorts
		// last; one checks the real namespace, so its expected order moves.
		{Name: "fixture-sort-order-go", Class: "testdata", Pattern: `wva-system`, Replace: "zz-system", Contexts: []string{CtxString},
			Paths: []string{"cmd/cache_namespaces_test.go"}},
		{Name: "fixture-sort-order-grants", Class: "testdata", Pattern: `"tenant-a","wva-policy"`, Replace: `"scaling-manager-policy","tenant-a"`,
			Paths: []string{"hack/check-policy-namespace-grants.sh"}},
		{Name: "fixture-sort-order-watch", Class: "testdata", Pattern: `team-a,team-b,wva-ctl,wva-policy`, Replace: `scaling-manager-policy,team-a,team-b,zz-ctl`,
			Paths: []string{"hack/check-watch-namespace-list.sh"}},
		{Name: "fixture-sort-order-shell", Class: "testdata", Pattern: `wva-ctl`, Replace: "zz-ctl",
			Paths: []string{"hack/check-watch-namespace-list.sh"}},

		{Name: "keep-whole-file", Class: "legacy-alias", Pattern: residualPattern, Action: Keep, Paths: keepWholeFiles,
			Contexts: []string{CtxText, CtxComment, CtxString, CtxTag, CtxPath}, Why: "the fall-back to the old names: the old names are the point"},

		// Images and people that are not this repository.
		kept("keep-warmpool-proxy", "image", `ghcr\.io/ev-shindin/(?:llm-scaler/)?warmpool-proxy`, "pinned by digest; no image exists under the new owner (see the report)"),
		kept("keep-fma-image", "image", `ghcr\.io/ev-shindin/(?:dual-pods-controller|fma-launcher)|ev-shindin/llm-d-fast-model-actuation|ev-shindin/vllm-conf`, "another project's image or fork"),
		kept("keep-old-repo-name", "repo-link", `github\.com/ev-shindin/llm-scaler/[^\s)\]"'`+"`"+`>]*`, "a numbered link on the repository's earlier name"),

		{Name: "keep-owners", Class: "person", Pattern: `ev-shindin`, Action: Keep, Paths: []string{"OWNERS"}, Why: "a GitHub login"},

		// Links that record something on the old repository: the new one does
		// not have these numbers.
		kept("keep-numbered-links", "repo-link", `github\.com/ev-shindin/llm-scaling-manager/(?:pull|issues)/[0-9]+[^\s)\]"'`+"`"+`>]*|github\.com/ev-shindin/llm-scaling-manager/commit/[0-9a-f]+|github\.com/ev-shindin/llm-scaling-manager/releases/tag/v[0-9][^\s)\]"'`+"`"+`>]*`,
			"pull requests, issues, commits and tags stay on the old repository: their numbers do not carry over"),

		// Held until the code, the issues and the images are in the new home.
		held("hold-image", `ghcr\.io/ev-shindin/llm-scaling-manager`, newImage, "the image does not exist under the new name until CI publishes it there"),
		{Name: "hold-image-base", Class: "repo-link", Pattern: `(?:^|[^\w/.-])(?P<m>ghcr\.io/ev-shindin)(?:$|[^\w/-])`, Replace: "ghcr.io/llm-d-incubation", Action: HoldUntilMoved,
			Paths: []string{"Makefile"}, Why: "the Makefile's image base"},
		{Name: "keep-launcher-owner", Class: "image", Pattern: `ghcr\.io/ev-shindin`, Action: Keep,
			Paths: []string{"docs/guides/warm-pool/install.md", "docs/proposals/fma-post-mortem.md"}, Why: "the launcher image stays with its owner"},
		held("hold-image-suffix", `\$\(IMAGE_TAG_BASE\)/llm-scaling-manager`, "$$(IMAGE_TAG_BASE)/"+newName, "the Makefile's image name"),
		held("hold-repo-url", `github\.com(?P<sep>[/:])ev-shindin/llm-scaling-manager`, "github.com${sep}"+newRepoSlug, "a link or a remote: resolves only after the move"),
		held("hold-packages", `github\.com/users/ev-shindin/packages\?repo_name=llm-scaling-manager`, "github.com/orgs/llm-d-incubation/packages?repo_name="+newName, "the package page"),
		held("hold-repo-slug", `ev-shindin/llm-scaling-manager`, newRepoSlug, "a repository slug: resolves only after the move"),
		held("hold-fork-clone", `/llm-scaling-manager\.git`, "/"+newName+".git", "a fork's clone URL: a fork of the new repository does not exist yet"),
		held("hold-clone-dir", `(?:^|[^\w/-])(?P<m>cd llm-scaling-manager)(?:$|[^\w/-])`, "cd "+newName, "the directory a clone makes"),
		held("hold-upstream-remote", `(?:^|[\s\x60])(?P<m>ev-shindin)(?:$|[\s\x60])`, "llm-d-incubation", "an owner named in text"),

		// llm-d-benchmark is another repository. Its scenario schema has a wva
		// block, and patch_harness.sh anchors on the text of its source, so these
		// name its code, not ours. No test here reads them: a test needs its clone.
		kept("keep-harness-schema", "external", `\.wva\.(?:enabled|hpa|prometheus|image|namespace|repoRef)\b|chartVersions\.wva|has\("wva"\)|\bwva\.(?:enabled|hpa|prometheus|image|namespace|repoRef)\b|harness,wva\b|wva-config/platform`,
			"llm-d-benchmark's scenario schema"),
		{Name: "keep-harness-scenario-key", Class: "external", Pattern: `(?:^|[\s\x60])(?P<m>wva):`, Action: Keep,
			Paths: []string{"hack/benchmark/scenarios/**"}, Why: "the wva block of llm-d-benchmark's scenario schema"},
		{Name: "keep-harness-step", Class: "external", Pattern: `--wva\b|28_wva-[a-z_.-]*`, Action: Keep,
			Paths: []string{"hack/benchmark/scenarios/**"}, Why: "a flag and a step of llm-d-benchmark"},
		{Name: "keep-harness-attribute", Class: "external", Pattern: `\bwva_namespace\b`, Action: Keep,
			Paths: []string{"hack/benchmark/patch_harness.sh", "hack/check-harness-teardown.sh"}, Why: "context.wva_namespace is an attribute of llm-d-benchmark's context"},
		{Name: "keep-harness-anchor", Class: "external", Pattern: `WVA teardown skipped|WVA was never installed`, Action: Keep,
			Paths: []string{"hack/benchmark/patch_harness.sh", "hack/check-harness-teardown.sh"}, Why: "text in llm-d-benchmark's source that the patcher anchors on"},

		// An X.509 common name holds 64 characters, and the longer service name
		// leaves room for a namespace of only 28, which the old default of 34
		// does not fit. Verification reads the SANs, so the common name can be
		// shorter than the Service: these keep every namespace up to 38 working.
		{Name: "tls-common-name-ca", Class: "tls", Pattern: `(?P<k>CN=|commonName: )wva-external-scaler-ca\.`, Replace: "${k}llmsm-scaler-ca.", Paths: scalerTLS},
		{Name: "tls-common-name-leaf", Class: "tls", Pattern: `serving "wva-external-scaler\.\$\{WVA_NS\}\.svc"`, Replace: `serving "llmsm-external-scaler.$${LLMSM_NS}.svc"`, Paths: scalerTLS},
		{Name: "tls-common-name-cert-manager", Class: "tls", Pattern: `commonName: \$\{dns1\}`, Replace: "commonName: llmsm-external-scaler.$${LLMSM_NS}.svc", Paths: scalerTLS},

		// An article in front of a name whose first sound changes. Only the
		// article is claimed; the name goes on to its own row. wva- and
		// workload-variant become scaling-manager, a consonant; wva_ and WVAName
		// become llmsm_ and LLMSMName, said "el".
		{Name: "article-before-consonant", Class: "prose", Pattern: `(?:^|[\s(*"'])(?P<m>[Aa]n) \x60?(?:wva-|workload-variant)`, Fn: articleA},
		{Name: "article-before-vowel-sound", Class: "prose", Pattern: `(?:^|[\s(*"'])(?P<m>[Aa]) \x60?(?:wva_|WVA[A-Z_])`, Fn: articleAn},

		// The in-memory struct that was a CRD, in comments, strings and text.
		{Name: "keep-harness-variant-key", Class: "external", Pattern: `\bvariantAutoscaling\b`, Action: Keep,
			Paths: []string{"hack/benchmark/scenarios/**"}, Why: "a key of llm-d-benchmark's scenario schema"},
		{Name: "variant-old-crd", Class: "variant", Pattern: `\b(?P<art>[Tt]he |[Aa] )?VariantAutoscaling (?P<k>CRD|CR|reconciler|controller)\b`, Fn: oldCRD},
		{Name: "variant-kind-qualified", Class: "variant", Pattern: `\.(?P<m>VariantAutoscaling(?:ConfigSpec|Spec|Status|List))\b`, Fn: variantBare},
		{Name: "variant-kind-spec", Class: "variant", Pattern: `(?:^|[^.\w])(?P<m>VariantAutoscaling(?:ConfigSpec|Spec|Status|List))\b`, Fn: variantDotted},
		{Name: "variant-word-by-model", Class: "variant", Pattern: `VariantAutoscaling(ByModel)\b`, Replace: "Variants$1"},
		{Name: "variant-word-group", Class: "variant", Pattern: `GroupVariantAutoscaling`, Replace: "GroupVariants"},
		{Name: "variant-word-active", Class: "variant", Pattern: `\b((?:In)?[Aa]ctive)VariantAutoscaling\b`, Replace: "${1}Variants"},
		{Name: "variant-word-plural", Class: "variant", Pattern: `VariantAutoscalings`, Replace: "Variants"},
		{Name: "variant-word", Class: "variant", Pattern: `VariantAutoscaling`, Replace: "Variant"},
		{Name: "variant-lower-plural", Class: "variant", Pattern: `\bvariantAutoscalings\b`, Replace: "variants"},
		{Name: "variant-lower", Class: "variant", Pattern: `\bvariantAutoscaling\b`, Replace: "variantObj"},
		{Name: "variant-lowercase-kind", Class: "variant", Pattern: `\bvariantautoscalings?\b`, Replace: "variants"},

		// A controller regex that must not reach the warm-pool Deployments.
		{Name: "dashboard-controller-regex", Class: "dashboard", Pattern: `\.\*workload-variant-autoscaler\.\*\|wva-controller-manager`, Replace: kubeName + "-controller-manager"},

		// Names with a length budget. KEDA refuses an HPA name over 63, and an HPA
		// is keda-hpa-<ScaledObject>, so what is added to a workload's name comes
		// off the workload's. A tenant's controller namespace goes into the
		// scaler's X.509 common name, which holds 64.
		{Name: "budget-warm-pool", Class: "k8s-object", Pattern: `\bwva-warm-pool\b`, Replace: "llmsm-warm-pool"},
		{Name: "budget-tenant-namespace", Class: "k8s-object", Pattern: `\bwva-<(?P<n>ns|namespace|tenant)>`, Replace: "llmsm-<${n}>"},

		// Metrics and alert names.
		{Name: "metric-name", Class: "metric", Pattern: metricPattern(metrics), Replace: metricPre + "${s}"},
		{Name: "alert-anchor", Class: "alert", Pattern: `#wva([a-z]{4,})`, Replace: "#llmsm$1", Contexts: []string{CtxText, CtxComment, CtxString}},
		{Name: "alert-name", Class: "alert", Pattern: `\bWVA([A-Z]\w*)`, Replace: "LLMSM$1"},
		{Name: "camel-embedded", Class: "word", Pattern: `([a-z])WVA([A-Z]\w*|\b)`, Replace: "${1}ScalingManager$2"},

		// Environment variables.
		{Name: "env-prefix", Class: "env", Pattern: `\bWVA_`, Replace: envPre},
		{Name: "env-after-escape", Class: "env", Pattern: `\\[a-z"](?P<m>WVA_)`, Replace: envPre},
		{Name: "env-inner", Class: "env", Pattern: `_WVA_`, Replace: "_LLMSM_"},
		{Name: "env-suffix", Class: "env", Pattern: `_WVA\b`, Replace: "_SCALING_MANAGER"},

		// The project's names.
		{Name: "project-name-title", Class: "project", Pattern: `Workload[- ]Variant[- ]Autoscaler(?: \(WVA\))?`, Replace: newName},
		{Name: "project-name-bare", Class: "project", Pattern: `llm-scaling-manager`, Replace: newName},
		pathToo(Rule{Name: "kube-long-name", Class: "k8s-object", Pattern: `workload-variant-autoscaler`, Replace: kubeName}),
		{Name: "key-domain", Class: "k8s-object", Pattern: `wva(?P<a>\\?\.)llm(?P<b>-?)d(?P<c>\\?\.)ai`, Replace: kubeName + "${a}llm${b}d${c}ai"},
		{Name: "go-style-word", Class: "word", Pattern: `\bwva([A-Z])`, Replace: "scalingManager$1"},
		{Name: "pascal-word", Class: "word", Pattern: `\bWva([A-Z])`, Replace: "ScalingManager$1"},
		{Name: "fixture-word", Class: "testdata", Pattern: `\bwvax\b`, Replace: "smx"},
		{Name: "e2e-word", Class: "word", Pattern: `e2ewva`, Replace: "e2e-scaling-manager"},
		pathToo(Rule{Name: "underscore-word-file", Class: "file-ref", Pattern: `infra_wva|dump_wva_|tail_wva_|wva_threshold|wva_saturation_v2_config`, Fn: renameFileWord}),
		{Name: "underscore-prefix", Class: "word", Pattern: `\bwva_`, Replace: metricPre},
		{Name: "underscore-then-object", Class: "k8s-object", Pattern: `_wva-`, Replace: "_" + kubeName + "-"},
		{Name: "underscore-inner", Class: "word", Pattern: `_wva_`, Replace: "_scaling_manager_"},
		pathToo(Rule{Name: "underscore-suffix", Class: "word", Pattern: `_wva\b`, Replace: "_scaling_manager"}),
		pathToo(Rule{Name: "kube-prefix", Class: "k8s-object", Pattern: `\bwva-`, Replace: kubeName + "-"}),
		pathToo(Rule{Name: "kube-suffix-word", Class: "k8s-object", Pattern: `(?P<w>deploy|two-variant|vs|external-scaler|manager)-wva\b`, Replace: "${w}-" + kubeName}),
		pathToo(Rule{Name: "kube-suffix", Class: "k8s-object", Pattern: `-wva\b`, Replace: "-llmsm"}),
		{Name: "dir-word", Class: "k8s-object", Pattern: `/wva\b`, Replace: "/" + kubeName},
		pathToo(Rule{Name: "dot-word", Class: "word", Pattern: `\bwva\.`, Replace: kubeName + "."}),
		{Name: "bare-word", Class: "word", Pattern: `\bwva\b`, Replace: kubeName},

		// Prose. The first rows see the word before WVA so an article is not
		// doubled, and the last row decides on a capital from what stands
		// before it on the line.
		{Name: "prose-py-ident", Class: "word", Pattern: `\bWVA(?P<x>\s*=\s*re\.|\.(?:match|search|sub)\()`, Replace: "LLMSM${x}", Paths: []string{"**/*.py"}},
		{Name: "prose-dashboard-title", Class: "prose", Pattern: `\bWVA Operational Dashboard`, Replace: "Scaling Manager Operational Dashboard"},
		{Name: "prose-hyphen", Class: "prose", Pattern: `\bWVA-`, Replace: "scaling-manager-"},
		{Name: "alert-prefix-literal", Class: "alert", Pattern: `"WVA"|\bWVA\.\*|\bWVA\[`, Fn: alertPrefix},
		{Name: "prose-adjective", Class: "prose", Pattern: `\b([Tt]he|[Aa]n?) ([a-z][a-z-]*) WVA\b`, Replace: "$1 $2 scaling manager"},
		{Name: "prose-the", Class: "prose", Pattern: `\b([Tt])he WVA\b`, Replace: "${1}he scaling manager"},
		{Name: "prose-a", Class: "prose", Pattern: `\b([Aa])n? WVA\b`, Replace: "${1} scaling manager"},
		{Name: "prose-determiner", Class: "prose", Pattern: `\b(?i:every|each|any|this|that|our|your|no|one|another|per|missing|secondary|new|old|other|second|own|its|their|whose|both|all|several|two|three|many|most|some|multiple) WVAs?\b`, Fn: proseDeterminer},
		{Name: "prose-plural", Class: "prose", Pattern: `\bWVAs\b`, Replace: "scaling managers"},
		{Name: "prose-hyphen-after", Class: "prose", Pattern: `-WVA\b`, Replace: "-scaling-manager"},
		{Name: "prose-word", Class: "prose", Pattern: `\bWVA\b`, Fn: proseWVA},
		{Name: "prose-title", Class: "prose", Pattern: `\bWva\b`, Replace: "Scaling manager"},
	}
	return rules
}

// identRules map Go identifiers. First match wins.
//
// VariantAutoscaling was a CRD and is now an in-memory struct in internal/variant,
// so its names lose the suffix, and its Spec, Status and List types lose the
// stutter: variant.Spec, not variant.VariantSpec. The import aliases carry the old
// kind and an API version the package does not have.
var identRules = []IdentRule{
	{Name: "ident-import-alias", Class: "go-ident", Pattern: `^(?:wvav1alpha1|llmdVariantAutoscalingV1alpha1|llmdOptv1alpha1|llmdv1alpha1|vav1alpha1)$`, Replace: "llmdvariant"},
	{Name: "ident-prometheus-alias", Class: "go-ident", Pattern: `^wvaprom$`, Replace: "prometheusutil"},
	{Name: "ident-variant-spec", Class: "go-variant", Pattern: `^VariantAutoscaling(ConfigSpec|Spec|Status|List)$`, Replace: "$1"},
	{Name: "ident-variant-by-model", Class: "go-variant", Pattern: `VariantAutoscaling(ByModel)$`, Replace: "Variants$1"},
	{Name: "ident-variant-group", Class: "go-variant", Pattern: `^(Test)?GroupVariantAutoscaling`, Replace: "${1}GroupVariants"},
	{Name: "ident-variant-active", Class: "go-variant", Pattern: `^((?:In)?[Aa]ctive)VariantAutoscaling$`, Replace: "${1}Variants"},
	{Name: "ident-variant-plural", Class: "go-variant", Pattern: `VariantAutoscalings`, Replace: "Variants"},
	{Name: "ident-variant-lower-plural", Class: "go-variant", Pattern: `^variantAutoscalings$`, Replace: "variants"},
	{Name: "ident-variant-lower", Class: "go-variant", Pattern: `^variantAutoscaling$`, Replace: "variantObj"},
	{Name: "ident-variant", Class: "go-variant", Pattern: `VariantAutoscaling`, Replace: "Variant"},
	{Name: "ident-leading-upper", Class: "go-ident", Pattern: `^WVA`, Replace: "LLMSM"},
	{Name: "ident-leading-lower", Class: "go-ident", Pattern: `^wva`, Replace: "scalingManager"},
	{Name: "ident-embedded-upper", Class: "go-ident", Pattern: `WVA`, Replace: "ScalingManager"},
	{Name: "ident-embedded-pascal", Class: "go-ident", Pattern: `Wva`, Replace: "ScalingManager"},
}

// metricPattern is an alternation of the metric suffixes, longest first so a
// name is not cut at a shorter one, which also takes _bucket and friends.
func metricPattern(metrics []string) string {
	seen := map[string]bool{}
	var alts []string
	for _, m := range metrics {
		s := strings.TrimPrefix(strings.TrimPrefix(m, "wva_"), "llmsm_")
		if !seen[s] {
			seen[s] = true
			alts = append(alts, regexp.QuoteMeta(s))
		}
	}
	if len(alts) == 0 {
		return `\bwva_(?P<s>\A\z)` // a pattern that cannot match
	}
	return `\bwva_(?P<s>(?:` + strings.Join(alts, "|") + `)\w*)`
}

// renameFileWord is the new form of an old word inside a file name.
func renameFileWord(claimed string, _ FnContext) string {
	r := strings.NewReplacer("infra_wva", "infra_scaling_manager", "wva_threshold", "scaling_manager_threshold",
		"wva_saturation_v2_config", "saturation_v2_config", "dump_wva_", "dump_scaling_manager_",
		"tail_wva_", "tail_scaling_manager_", "wva", kubeName)
	return r.Replace(claimed)
}

// variantDotted and variantBare name a type of internal/variant in text: outside
// Go code it needs its package to read, and after a dot it already has one.
func variantDotted(claimed string, c FnContext) string {
	bare := strings.TrimPrefix(claimed, "VariantAutoscaling")
	if c.StartsGoDoc() {
		return bare
	}
	return "variant." + bare
}

func variantBare(claimed string, _ FnContext) string {
	return strings.TrimPrefix(claimed, "VariantAutoscaling")
}

// proseDeterminer keeps the word before WVA and drops the article the bare word
// would take.
func proseDeterminer(claimed string, _ FnContext) string {
	head, _, _ := strings.Cut(claimed, " ")
	if strings.HasSuffix(claimed, "s") {
		return head + " scaling managers"
	}
	return head + " scaling manager"
}

// alertPrefix is WVA as the prefix of alert names in a query: code, so it takes
// the prefix they now have, not the prose form.
func alertPrefix(claimed string, _ FnContext) string {
	return strings.Replace(claimed, "WVA", "LLMSM", 1)
}

// oldCRD names what no longer exists without inventing a Variant CRD.
func oldCRD(claimed string, c FnContext) string {
	head, kind := "the", claimed[strings.LastIndex(claimed, " ")+1:]
	if strings.HasPrefix(claimed, "The") {
		head = "The"
	}
	if articleAbove(c) {
		return "old " + kind
	}
	return head + " old " + kind
}

func articleA(claimed string, _ FnContext) string  { return claimed[:1] }
func articleAn(claimed string, _ FnContext) string { return claimed + "n" }

// moduleSwap keeps what follows the module path and swaps the path itself.
func moduleSwap(target string) func(string, FnContext) string {
	return func(claimed string, _ FnContext) string {
		for _, old := range oldModules {
			if rest, ok := strings.CutPrefix(claimed, old); ok {
				return target + rest
			}
		}
		return claimed
	}
}

// proseWVA is "the scaling manager", capitalised where a sentence starts. A word
// at the start of a line starts a sentence only if the line above ended one.
func proseWVA(_ string, c FnContext) string {
	if c.Heading() {
		return "Scaling manager"
	}
	trimmed := strings.TrimRight(c.Before, " \t*_`\"'>([")
	if trimmed == "" || isCommentLead(trimmed) {
		if articleAbove(c) {
			return "scaling manager"
		}
		if c.Prev != "" && !endsSentence(c.Prev) {
			return "the scaling manager"
		}
		return "The scaling manager"
	}
	switch {
	case strings.HasSuffix(trimmed, "|"), strings.HasSuffix(trimmed, "-"), endsSentence(trimmed):
		return "The scaling manager"
	}
	return "the scaling manager"
}

// articleAbove reports whether the text opens a line and the line above ends in
// an article, so it must not bring one of its own.
func articleAbove(c FnContext) bool {
	words := strings.Fields(c.Prev)
	if len(words) == 0 || !isCommentLead(strings.TrimSpace(c.Before)) {
		return false
	}
	switch strings.ToLower(words[len(words)-1]) {
	case "the", "a", "an":
		return true
	}
	return false
}

func isCommentLead(s string) bool {
	return strings.Trim(s, " \t/#*") == ""
}

func endsSentence(s string) bool {
	return strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, ":")
}
