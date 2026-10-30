package learnreplay

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// recordedTypes is every type a recorded event can reach. What a Raw or an
// interface holds is not reachable by type, so the types encoded into them
// are listed too: the input's policy, the capacity records, the learned-state
// document. The export reads the Go field names from them to tell a struct
// from a map, and the tests walk them to hold the classification tables to
// every field.
var recordedTypes = []reflect.Type{
	reflect.TypeFor[learnrec.Start](),
	reflect.TypeFor[learnrec.Input](),
	reflect.TypeFor[*config.ScalingPolicy](),
	reflect.TypeFor[map[string]*capacity.Record](),
	reflect.TypeFor[saturation.LearnedState](),

	reflect.TypeFor[saturation.AnalyzeEvent](),
	reflect.TypeFor[saturation.PresentEvent](),
	reflect.TypeFor[saturation.Evicted](),
	reflect.TypeFor[saturation.ForgetVariantEvent](),
	reflect.TypeFor[saturation.ForgottenTokenEvent](),
	reflect.TypeFor[saturation.HoldEvent](),
	reflect.TypeFor[saturation.RestoreEvent](),
	reflect.TypeFor[saturation.RestoreReport](),
	reflect.TypeFor[saturation.AcceleratorEvent](),
	reflect.TypeFor[saturation.LineEvent](),

	reflect.TypeFor[slo.AnalyzeEvent](),
	reflect.TypeFor[slo.EvictStaleEvent](),
	reflect.TypeFor[slo.PresentEvent](),
	reflect.TypeFor[metrics.SLOModelSnapshot](),

	reflect.TypeFor[throughput.AnalyzeEvent](),
	reflect.TypeFor[throughput.ObserveEvent](),

	reflect.TypeFor[capacity.FileScaleTargetEvent](),
	reflect.TypeFor[capacity.EvictStaleEvent](),

	reflect.TypeFor[domain.AnalyzerResult](),
	reflect.TypeFor[domain.AnalyzerInput](),
}

// fieldNames is every Go field name of recordedTypes: an object whose keys
// are all field names is a struct, any other a map.
var fieldNames = func() map[string]bool {
	out := map[string]bool{}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if seen[t] {
			return
		}
		seen[t] = true
		if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface {
			if _, ok := t.MethodByName("RecordedValue"); ok {
				if set, ok := reflect.PointerTo(t).MethodByName("SetRecordedValue"); ok && set.Type.NumIn() == 2 {
					walk(set.Type.In(1))
				}
				return
			}
		}
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(t.Elem())
		case reflect.Map:
			walk(t.Elem())
		case reflect.Struct:
			for i := range t.NumField() {
				out[t.Field(i).Name] = true
				walk(t.Field(i).Type)
			}
		}
	}
	for _, t := range recordedTypes {
		walk(t)
	}
	// The outputs' summary row.
	for _, f := range []string{"Error", "Result"} {
		out[f] = true
	}
	return out
}()

// class says what the export does with a string, and what it accepts there
// afterwards.
type class int

const (
	// classRename: a name a cluster gives; it becomes a pseudonym of the
	// category the table names.
	classRename class = iota + 1
	// classVocab: a value the code defines; it must be in vocabulary.
	classVocab
	// classAccelerator: a hardware product name, kept.
	classAccelerator
	// classDuration: a Go duration, kept.
	classDuration
	// classFlag: an engine flag name, kept.
	classFlag
	// classTypeName: a Go type name, kept.
	classTypeName
	// classLabelKey: a label or taint key; kept under a well-known prefix,
	// renamed otherwise.
	classLabelKey
	// classResource: a resource name; kept when well known, renamed
	// otherwise.
	classResource
	// classComposite: a key built from names and figures, joined by '|' and
	// '/'; each part is renamed, and each must then be allowed.
	classComposite
	// classText: a message; names inside it are renamed, host names, IPs
	// and URLs are replaced, and each word must then be allowed.
	classText
	// classCommit: the commit a build was made from: hex, or "unknown".
	classCommit
	// classRedact: configuration text no analyzer reads; a non-empty value
	// is replaced by "redacted".
	classRedact
)

// rule is one table entry: a class, and for a rename its category.
type rule struct {
	class    class
	category string
}

func rename(category string) rule { return rule{classRename, category} }

// valueRules classify a string value by the Go field it sits under, or by
// "<parent>.<field>" where one field name means two things. A value inside
// a map is classified by the map's field. A string under a field in no
// table stops the export.
var valueRules = map[string]rule{
	"Namespace": rename("ns"), "DonorNamespace": rename("ns"), "ControllerNamespace": rename("ns"),
	"BorrowNamespaces": rename("ns"), "ClusterNamespaces": rename("ns"), "Exclude": rename("ns"),
	"ModelID": rename("model"), "ModelName": rename("model"), "ModelPath": rename("model"),
	// Variant and Donor hold a composite key in some events.
	"VariantName": rename("variant"), "Variants": rename("variant"),
	"Variant": {classComposite, "variant"}, "Donor": {classComposite, "variant"},
	"PodName": rename("pod"), "UnplacedPods": rename("pod"), "AffectedPods": rename("pod"),
	"Image":    rename("image"),
	"Revision": rename("revision"), "ModelRevision": rename("revision"),
	"Token":             rename("token"),
	"SpeculativeConfig": rename("arg"), "SharingUnresolved": rename("arg"),
	"NodeSelector": rename("label"), "Values": rename("label"), "Value": rename("label"),
	"PriorityClass": rename("priority"),
	"WeightClass":   rename("weightclass"),
	"Limiters.Name": rename("limiter"),

	"Role": {class: classVocab}, "Reason": {class: classVocab}, "K2Source": {class: classVocab},
	"RateSource": {class: classVocab}, "Source": {class: classVocab}, "Excluded": {class: classVocab},
	"Objective": {class: classVocab}, "Statistic": {class: classVocab}, "LearnedFrom": {class: classVocab},
	"FreshnessStatus": {class: classVocab}, "Quantity": {class: classVocab}, "Engine": {class: classVocab},
	"Scope": {class: classVocab}, "Type": {class: classVocab}, "AnalyzerName": {class: classVocab},
	"Analyzers.Name": {class: classVocab}, "DefaultPolicy": {class: classVocab}, "Operator": {class: classVocab},
	"Effect": {class: classVocab}, "KvCacheDtype": {class: classVocab}, "WeightDtype": {class: classVocab},
	"Quantization": {class: classVocab}, "KVConnector": {class: classVocab},
	"KVOffloadingBackend": {class: classVocab}, "MemoryLayout": {class: classVocab},

	"Accelerator": {class: classAccelerator}, "AcceleratorName": {class: classAccelerator},
	"Answer": {class: classAccelerator}, "Accelerators.Name": {class: classAccelerator},

	"BorrowCooldown": {class: classDuration}, "FlappingWindow": {class: classDuration},
	"LenderDebounce": {class: classDuration}, "PlacementTimeout": {class: classDuration},
	"RefreshInterval": {class: classDuration}, "RetentionPeriod": {class: classDuration},
	"WaitBeforeReverseMove": {class: classDuration},

	"Unresolved": {class: classFlag},
	"ConfigType": {class: classTypeName},
	"Commit":     {class: classCommit},
	"Key":        {classLabelKey, "labelkey"}, "TopologyKey": {classLabelKey, "labelkey"},
	"Resources": {classResource, "resource"},

	"Args": {class: classComposite}, "Config": {class: classComposite}, "From": {class: classComposite},
	"FromVariant": {class: classComposite}, "PhysicsKey": {class: classComposite},
	"K2Args": {class: classComposite}, "ThroughputArgs": {class: classComposite},

	"Error": {class: classText}, "Issues": {class: classText},

	"Query": {class: classRedact}, "DecodeErr": {class: classRedact}, "Err": {class: classRedact},
	"Threshold": {class: classRedact},
}

// keyRules classify a map's keys by the map's field, with "{}" for each map
// level inside it ("NamespaceQuotas{}" is the inner map's keys), or by the
// event kind for a map that is the event's whole data. A map in no table
// may hold only keys that are allowed as they are (allowedAtom).
var keyRules = map[string]rule{
	"Records": rename("variant"), "VariantBusiestQueueLoad": rename("variant"),
	"GatedAt": rename("pod"), "UnplacedAt": rename("pod"),
	"NamespaceQuotas": rename("ns"), "Namespaces": rename("ns"),
	"WeightClasses":   rename("weightclass"),
	"NodeSelector":    {classLabelKey, "labelkey"},
	"ReplicaRequests": {classResource, "resource"}, "ClusterQuotas": {classResource, "resource"},
	"NamespaceQuotas{}": {classResource, "resource"},

	"Accelerators": {class: classComposite}, "Args": {class: classComposite}, "Config": {class: classComposite},
	"Forgotten": {class: classComposite}, "ITLBaseline": {class: classComposite},
	"ITLBaselineStash": {class: classComposite}, "ITLLines": {class: classComposite},
	"ITLSlopes": {class: classComposite}, "K2": {class: classComposite}, "K2Args": {class: classComposite},
	"K2BorrowedSince": {class: classComposite}, "K2Stash": {class: classComposite},
	"LowerBounds": {class: classComposite}, "PriorK2": {class: classComposite},
	"StartSeconds": {class: classComposite}, "Throughput": {class: classComposite},
	"ThroughputArgs": {class: classComposite}, "ThroughputStash": {class: classComposite},
	"VariantSeenAt": {class: classComposite}, "Reports": {class: classComposite},

	"HeldRoleDemand": {class: classVocab}, "HotRankDemand": {class: classVocab},
	"RoleDemand": {class: classVocab}, "RoleStartSeconds": {class: classVocab},
	"AnalyzerDefinitions": {class: classVocab}, "Engines": {class: classVocab},
	"Parameters":                       {class: classVocab},
	saturation.KindTakeRestoreOutcomes: {class: classVocab},
}

// vocabulary is every value the code defines that a kept field or map key
// may hold. A value not here stops the export, naming the field: add the
// word here when it is the code's own, or classify the field as a rename
// when it is a name someone chose.
var vocabulary = setOf(
	// Roles and analyzers.
	"both", "decode", "prefill", "saturation", "slo", "throughput", "saturation-token-based",
	"queueing-model", "default",
	// Capacity sources and reasons.
	"P0-store", "P1-obs", "P1-obs-downstream", "P1-obs-invalid", "P1-obs-rollout", "P1-obs-unadmitted",
	"P1-obs-unsettled", "P2-borrow", "P2-hist", "P2-prior", "P3-k2", "P4-k1",
	"T1-ols", "T2-default", "T2-failed", "T2-pinned",
	"no-change", "no-data", "no-itl", "no-line", "no-rate", "no-separators", "no-seqs", "no-shape",
	"slo-model", "slo-own", "slo-pinned", "slo-tpot", "slo-transient", "slo-ttft", "slo-variant",
	"own", "pinned", "borrowed", "borrowed-line", "derived", "measured", "sibling", "stale", "served",
	"itl-line", "k2", "mu", "deployment", "live", "store", "unobserved", "whole", "partial", "none",
	"tpot", "ttft", "mean", "p50", "p90", "p95", "p99",
	// Freshness, limiter scopes and types, toleration operators and effects.
	"fresh", "stale-ok", "unavailable", "namespace", "cluster", "quota", "gpu-inventory", "kueue",
	"utilization-share", "Exists", "Equal", "In", "NotIn", "DoesNotExist", "Gt", "Lt",
	"NoSchedule", "PreferNoSchedule", "NoExecute",
	// Engines, dtypes, quantization, KV connectors and layouts.
	"vllm", "sglang", "auto", "half", "float16", "bfloat16", "float", "float32", "fp8", "fp8_e4m3",
	"fp8_e5m2", "int8", "awq", "gptq", "marlin", "awq_marlin", "gptq_marlin", "compressed-tensors",
	"modelopt", "bitsandbytes", "NixlConnector", "LMCacheConnectorV1", "OffloadingConnector",
	"MultiConnector", "P2pNcclConnector", "native", "lmcache", "cpu",
	// The redaction marker.
	"redacted",
)

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// wellKnownLabelDomains are the label and resource prefixes kept as they
// are: Kubernetes' own and the GPU vendors'.
var wellKnownLabelDomains = []string{"kubernetes.io", "k8s.io", "nvidia.com", "amd.com", "intel.com",
	"habana.ai", "cloud.google.com", "amazonaws.com", "azure.com", "karpenter.sh", "openshift.io",
	"x-k8s.io", "llm-d.ai"}

// wellKnownLabel reports a label key, or resource name, of Kubernetes or a
// GPU vendor: "nvidia.com/gpu.product", "topology.kubernetes.io/zone",
// "cpu".
func wellKnownLabel(k string) bool {
	switch k {
	case "cpu", "memory", "ephemeral-storage", "pods":
		return true
	}
	domain, _, ok := strings.Cut(k, "/")
	if !ok {
		return false
	}
	for _, d := range wellKnownLabelDomains {
		if domain == d || strings.HasSuffix(domain, "."+d) {
			return true
		}
	}
	return false
}

var (
	acceleratorShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	flagShape        = regexp.MustCompile(`^--?[a-z0-9][a-z0-9_-]*$`)
	typeNameShape    = regexp.MustCompile(`^\*?[a-z][a-z0-9]*\.[A-Z][A-Za-z0-9]*$`)
	commitShape      = regexp.MustCompile(`^([0-9a-f]{7,40}|unknown)$`)
)

// isNumber reports a string that is a number, or a float the recorder
// writes as a string.
func isNumber(s string) bool {
	switch s {
	case "NaN", "+Inf", "-Inf":
		return true
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// isTime reports a recorded time.
func isTime(s string) bool {
	if len(s) < 20 || s[4] != '-' || s[7] != '-' || s[10] != 'T' {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// isHex16 reports a fingerprint: 16 lowercase hex digits.
func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
