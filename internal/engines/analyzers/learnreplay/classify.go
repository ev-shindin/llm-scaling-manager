package learnreplay

import (
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
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

	reflect.TypeFor[enginerec.CompositeEvent](),
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
	// classAccelerator: a GPU product name; kept when the code knows it
	// (knownAccelerators), renamed otherwise.
	classAccelerator
	// classDuration: a Go duration, kept.
	classDuration
	// classFingerprint: an engine fingerprint (ArgsTag): 16 hex digits,
	// maybe behind capacity.IncompleteArgsPrefix.
	classFingerprint
	// classTypeName: the Go type of a recorded policy: one the replay can
	// decode (knownConfigTypes).
	classTypeName
	// classLabelKey: a label or taint key; kept when it is one of
	// knownLabels exactly,
	// renamed otherwise.
	classLabelKey
	// classResource: a resource name; kept when it is one of knownLabels,
	// renamed
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
	// The engine's composite events: analyzer names, and why a role is held.
	"Entries.Name": {class: classVocab}, "Grace.Name": {class: classVocab}, "Composite.Name": {class: classVocab},
	"Eligible": {class: classVocab}, "HoldReasons": {class: classVocab},
	"Effect": {class: classVocab}, "KvCacheDtype": {class: classVocab}, "WeightDtype": {class: classVocab},
	"Quantization": {class: classVocab}, "KVConnector": {class: classVocab},
	"KVOffloadingBackend": {class: classVocab}, "MemoryLayout": {class: classVocab},

	"Accelerator": {classAccelerator, "accelerator"}, "AcceleratorName": {classAccelerator, "accelerator"},
	"Answer": {classAccelerator, "accelerator"}, "Accelerators.Name": {classAccelerator, "accelerator"},

	"BorrowCooldown": {class: classDuration}, "FlappingWindow": {class: classDuration},
	"LenderDebounce": {class: classDuration}, "PlacementTimeout": {class: classDuration},
	"RefreshInterval": {class: classDuration}, "RetentionPeriod": {class: classDuration},
	"WaitBeforeReverseMove": {class: classDuration},

	"Unresolved": rename("arg"),
	"ConfigType": {class: classTypeName},
	"Commit":     {class: classCommit},
	"Key":        {classLabelKey, "labelkey"}, "TopologyKey": {classLabelKey, "labelkey"},
	"Resources": {classResource, "resource"},

	"Args": {class: classFingerprint}, "Config": {class: classFingerprint}, "From": {class: classComposite},
	"FromVariant": {class: classComposite}, "PhysicsKey": {class: classComposite},
	"K2Args": {class: classFingerprint}, "ThroughputArgs": {class: classFingerprint},

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
	"Parameters": {class: classVocab},
	// The composite's per-role maps, keyed by role.
	"Holds": {class: classVocab}, "HoldReasons": {class: classVocab}, "ReleaseBound": {class: classVocab},
	"OtherFrames": {class: classVocab}, "RoleCapacities": {class: classVocab}, "RoleSpare": {class: classVocab},
	"RoleReleasable":                   {class: classVocab},
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
	"k1-bound", "pinned-default", "slo-pinned-default", "no-output-length", "declined", "infeasible",
	"input-invalid",
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
	// The composite: its name, its decision paths, and why it holds a role
	// (steadystate's hold reasons; an analyzer's grace names the analyzer).
	"CompositeSignal", "C0-agree", "C1-single", "C4-no-signal",
	"no-listed-analyzer-sizes-role", "other-role-cannot-grow", "saturation-holds-role",
	"analyzer-in-grace:throughput",
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

// knownLabels are the label keys and resource names kept as they are: exact
// strings, Kubernetes' and the GPU vendors' own. Any other key is renamed,
// whatever its domain: a name after a well-known domain is still a name.
var knownLabels = setOf(
	"cpu", "memory", "ephemeral-storage", "pods",
	"nvidia.com/gpu", "nvidia.com/gpu.product", "nvidia.com/gpu.count", "nvidia.com/gpu.memory",
	"nvidia.com/gpu.family", "nvidia.com/gpu.machine", "nvidia.com/mig.config", "nvidia.com/gpu.present",
	"amd.com/gpu", "amd.com/gpu.product-name", "intel.com/gpu", "habana.ai/gaudi",
	"kubernetes.io/hostname", "kubernetes.io/arch", "kubernetes.io/os",
	"topology.kubernetes.io/zone", "topology.kubernetes.io/region", "node.kubernetes.io/instance-type",
	"node-role.kubernetes.io/worker", "node-role.kubernetes.io/control-plane",
	"node.kubernetes.io/not-ready", "node.kubernetes.io/unreachable", "node.kubernetes.io/unschedulable",
	"cloud.google.com/gke-accelerator", "cloud.google.com/gke-nodepool", "karpenter.sh/capacity-type",
	"karpenter.sh/nodepool", "kueue.x-k8s.io/queue-name", "kueue.x-k8s.io/priority-class",
	"leaderworkerset.sigs.k8s.io/name",
)

// knownAccelerators are the GPU product names the code and its docs name.
// Any other accelerator is renamed: a product name the code does not know
// is as likely a node pool someone named.
var knownAccelerators = setOf(
	"H100", "H200", "A100", "A10G", "L4", "L40S", "T4", "V100", "B200", "GB200", "MI300X", "MI250", "Gaudi-2",
	"NVIDIA-H100", "NVIDIA-H100-80GB-HBM3", "NVIDIA-H100-SXM5-80GB", "NVIDIA-H100-PCIE-80GB", "NVIDIA-H100-NVL",
	"NVIDIA-H200", "NVIDIA-H200-141GB-HBM3e", "NVIDIA-A100", "NVIDIA-A100-PCIE-80GB", "NVIDIA-A100-SXM4-80GB",
	"NVIDIA-A100-PCIE-40GB", "NVIDIA-A100-SXM4-40GB", "NVIDIA-A10G", "NVIDIA-L4", "NVIDIA-L40S",
	"NVIDIA-B200", "NVIDIA-GB200", "NVIDIA-V100-PCIE-16GB", "Tesla-T4", "Tesla-V100-SXM2-16GB",
	"AMD-MI300X", "AMD-MI300X-192G", "AMD-MI250-128G", "AMD_Instinct_MI300X_OAM", "Intel-Gaudi-2-96GB",
)

// knownConfigTypes are the policy types a replay decodes.
var knownConfigTypes = setOf("*config.ScalingPolicy")

var commitShape = regexp.MustCompile(`^([0-9a-f]{7,40}|unknown)$`)

// numberShape is a decimal number as the recorder or a message writes one:
// no hex floats, no "Inf" or "NaN" words (those are the recorder's exact
// "NaN", "+Inf" and "-Inf", checked apart).
var numberShape = regexp.MustCompile(`^[-+]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][-+]?[0-9]+)?$`)

// isNumber reports a string that is a number, or a float the recorder
// writes as a string.
func isNumber(s string) bool {
	switch s {
	case "NaN", "+Inf", "-Inf":
		return true
	}
	return numberShape.MatchString(s)
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

// isFingerprint reports an ArgsTag: a fingerprint, maybe marked incomplete.
func isFingerprint(s string) bool {
	return isHex16(strings.TrimPrefix(s, capacity.IncompleteArgsPrefix))
}

// structSites are the places, named as keyRules names a map ("<field>"
// plus "{}" per map level inside it), where some recorded type has a
// struct. A place in keyRules and not here is always a map, whatever its
// keys look like; a place in both (LearnedState.Config is a map,
// Input.Config a policy) is told apart by its keys (isStruct).
var structSites = func() map[string]bool {
	out := map[string]bool{}
	type visit struct {
		t     reflect.Type
		field string
		depth int
	}
	seen := map[visit]bool{}
	var walk func(t reflect.Type, field string, depth int)
	walk = func(t reflect.Type, field string, depth int) {
		v := visit{t, field, depth}
		if seen[v] {
			return
		}
		seen[v] = true
		if t.Kind() != reflect.Pointer && t.Kind() != reflect.Interface {
			if _, ok := t.MethodByName("RecordedValue"); ok {
				if set, ok := reflect.PointerTo(t).MethodByName("SetRecordedValue"); ok && set.Type.NumIn() == 2 {
					walk(set.Type.In(1), field, depth)
				}
				return
			}
		}
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(t.Elem(), field, depth)
		case reflect.Map:
			walk(t.Elem(), field, depth+1)
		case reflect.Struct:
			if t == reflect.TypeFor[time.Time]() {
				return
			}
			out[field+strings.Repeat("{}", depth)] = true
			for i := range t.NumField() {
				walk(t.Field(i).Type, t.Field(i).Name, 0)
			}
		}
	}
	for _, t := range recordedTypes {
		walk(t, "", 0)
	}
	// The policy and the input are written under these fields as Raw.
	out["Config"], out["Input"], out["State"] = true, true, true
	return out
}()
