package capacity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
)

// The flags that size or reshape what one replica holds, which no other field
// describes, reach MemoryLayout from both parsers and from the env, in a form
// flag order does not change.
func TestTheMemoryLayoutFlagsAreRead(t *testing.T) {
	p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "org/m",
		"--num-redundant-experts", "32",
		"--kv-cache-memory-bytes", "40G",
		"--enable-eplb",
		"--eplb-config", `{"window_size": 1000, "num_redundant_experts": 32}`,
		"--all2all-backend", "deepep_low_latency",
	}, corev1.EnvVar{Name: "VLLM_ALL2ALL_BACKEND", Value: "deepep_high_throughput"})))
	require.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
	assert.Empty(t, p.SharingUnresolved)
	assert.Equal(t, "all2all_backend=deepep_low_latency;enable_eplb=true;"+
		`env.VLLM_ALL2ALL_BACKEND=deepep_high_throughput;eplb_config={"num_redundant_experts":32,"window_size":1000};`+
		"kv_cache_memory_bytes=40G;num_redundant_experts=32", p.MemoryLayout)

	reordered := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "org/m",
		"--all2all-backend", "deepep_low_latency",
		"--eplb-config", `{"num_redundant_experts":32,"window_size":1000}`,
		"--enable-eplb",
		"--kv-cache-memory-bytes", "40G",
		"--num-redundant-experts", "32",
	}, corev1.EnvVar{Name: "VLLM_ALL2ALL_BACKEND", Value: "deepep_high_throughput"})))
	assert.Equal(t, p.MemoryLayout, reordered.MemoryLayout, "flag order and JSON spacing do not change it")
	assert.Equal(t, p.Fingerprint(), reordered.Fingerprint())

	off := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer([]string{
		"vllm", "serve", "org/m", "--enable-eplb", "--no-enable-eplb",
	})))
	assert.Empty(t, off.MemoryLayout, "the negative form takes the flag back, as argparse does")

	sg := ParseEngineArgs(inferenceengine.EngineSGLang, deploymentWithContainers(corev1.Container{
		Name: "sglang", Image: "lmsysorg/sglang:v0.5", Command: []string{
			"python", "-m", "sglang.launch_server", "--model-path", "org/m",
			"--enable-dp-attention", "--moe-a2a-backend", "deepep",
		}}))
	require.True(t, sg.Complete(), "unresolved: %v", sg.Unresolved)
	assert.Equal(t, "enable_dp_attention=true;moe_a2a_backend=deepep", sg.MemoryLayout)
}

// Each memory-layout flag splits the identity: two engines that differ only
// in it are different keys and different capacity configurations, and the KV
// total measured under one is not carried to the other.
func TestEachMemoryLayoutFlagSplitsTheIdentity(t *testing.T) {
	base := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m"})))
	require.Empty(t, base.MemoryLayout)
	for name, args := range map[string][]string{
		"kv cache bytes":    {"--kv-cache-memory-bytes", "40G"},
		"redundant experts": {"--num-redundant-experts", "32"},
		"eplb on":           {"--enable-eplb"},
		"eplb config":       {"--eplb-config", `{"num_redundant_experts":16}`},
		"all2all backend":   {"--all2all-backend", "deepep_low_latency"},
	} {
		t.Run(name, func(t *testing.T) {
			other := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
				append([]string{"vllm", "serve", "org/m"}, args...))))
			require.NotEmpty(t, other.MemoryLayout)
			assert.NotEqual(t, base.Fingerprint(), other.Fingerprint())
			assert.NotEqual(t, base.ArgsFingerprint(), other.ArgsFingerprint())
			assert.False(t, base.IsCapacityCompatible(&other))
			assert.False(t, kvTotalCarries(base, other))
			assert.True(t, base.IsCapacityClampCompatible(&other), "a ceiling is still a ceiling")
		})
	}

	env := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
		[]string{"vllm", "serve", "org/m"}, corev1.EnvVar{Name: "VLLM_ALL2ALL_BACKEND", Value: "pplx"})))
	assert.False(t, base.IsCapacityCompatible(&env), "the env var is read too")
}

// A memory-layout value that cannot be read withholds sharing, not the
// variant's own restore: it lands in SharingUnresolved, and the read stays
// complete.
func TestAnUnreadableMemoryLayoutWithholdsOnlySharing(t *testing.T) {
	for name, c := range map[string]corev1.Container{
		"flag from an unset variable": vllmContainer([]string{
			"vllm", "serve", "org/m", "--kv-cache-memory-bytes", "$KV_BYTES"}),
		"env from valueFrom": vllmContainer([]string{"vllm", "serve", "org/m"}, corev1.EnvVar{
			Name:      "VLLM_ALL2ALL_BACKEND",
			ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{Key: "backend"}},
		}),
		"env referencing an unset variable": vllmContainer([]string{"vllm", "serve", "org/m"},
			corev1.EnvVar{Name: "VLLM_ALL2ALL_BACKEND", Value: "$(BACKEND)"}),
	} {
		t.Run(name, func(t *testing.T) {
			p := ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(c))
			assert.True(t, p.Complete(), "unresolved: %v", p.Unresolved)
			require.Len(t, p.SharingUnresolved, 1)
			assert.False(t, p.IsCapacityCompatible(&p), "not known to be the same engine")
		})
	}

	sg := ParseEngineArgs(inferenceengine.EngineSGLang, deploymentWithContainers(corev1.Container{
		Name: "sglang", Image: "lmsysorg/sglang:v0.5", Command: []string{
			"python", "-m", "sglang.launch_server", "--model-path", "org/m", "--moe-a2a-backend", "$A2A",
		}}))
	assert.True(t, sg.Complete(), "unresolved: %v", sg.Unresolved)
	assert.Equal(t, []string{"moe_a2a_backend=$A2A"}, sg.SharingUnresolved)
}

// An unread identity is not an equality: a pair where either side has an
// unread checkpoint or an uninterpreted identity flag is refused for sharing
// capacity, and still found as a ceiling.
func TestAnUnreadIdentityIsNotCapacityCompatible(t *testing.T) {
	base := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&base)
	base.ModelPath = "org/m"

	unreadModel := base
	unreadModel.ModelPath = ""
	unreadModel.ModelUnresolved = true

	unreadDP := base
	unreadDP.SharingUnresolved = []string{"data_parallel_size=$((LWS_GROUP_SIZE * DP_SIZE_LOCAL))"}

	require.True(t, base.IsCapacityCompatible(&base), "the control: a fully-read engine matches itself")
	for name, pair := range map[string][2]EngineParams{
		"two unread checkpoints":          {unreadModel, unreadModel},
		"read against unread checkpoint":  {base, unreadModel},
		"the same computed parallel size": {unreadDP, unreadDP},
		"read against computed size":      {base, unreadDP},
	} {
		t.Run(name, func(t *testing.T) {
			a, b := pair[0], pair[1]
			assert.False(t, a.IsCapacityCompatible(&b))
			assert.False(t, b.IsCapacityCompatible(&a))
			assert.True(t, a.IsCapacityClampCompatible(&b), "the ceiling's equality does not read either field")
		})
	}
}

// sameArguments and ArgsFingerprint are one definition: a change the live
// path keeps a record across is a change a restart restores across, and the
// other way round.
func TestSameArgumentsIsTheArgumentsFingerprint(t *testing.T) {
	parse := func(args ...string) EngineParams {
		return ParseEngineArgs(inferenceengine.EngineVLLM, deploymentWithContainers(vllmContainer(
			append([]string{"vllm", "serve"}, args...))))
	}
	base := parse("org/m")
	for name, tc := range map[string]struct {
		other EngineParams
		same  bool
	}{
		// The DeepEqual it replaced compared MaxNumBatchedTokens, so writing
		// the default budget out replaced a live record a restart restored.
		"the default batch budget written out": {parse("org/m", "--max-num-batched-tokens", "8192"), true},
		"a different batch budget":             {parse("org/m", "--max-num-batched-tokens", "4096"), false},
		"the checkpoint path re-spelled":       {parse("/models/org/m"), false},
		"the offload resized":                  {parse("org/m", "--kv-offloading-size", "64"), false},
		"only the image":                       {func() EngineParams { p := base; p.Image = "vllm:other"; return p }(), true},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.same {
				require.Equal(t, base.EffectiveMaxBatchedTokens, tc.other.EffectiveMaxBatchedTokens,
					"precondition: a 'same' case must resolve to the same budget")
			}
			assert.Equal(t, tc.same, sameArguments(base, tc.other))
			assert.Equal(t, tc.same, base.ArgsFingerprint() == tc.other.ArgsFingerprint())
		})
	}
}

// The ceiling on a parked variant's estimate is the LOWEST live measurement
// among its clamp-compatible siblings, from its own namespace when any
// qualifies, and the same one every time.
func TestFindClampBoundTakesTheLowestLiveSiblingOwnNamespaceFirst(t *testing.T) {
	params := defaultEngineParams()
	resolveEffectiveMaxBatchedTokens(&params)
	const model, accel = "m", "H200"
	live := func(capacity int64) Record {
		return Record{AcceleratorName: accel, GpuCount: 8, EffectiveCapacity: capacity,
			EngineParams: &params, LearnedFrom: LearnedFromLive}
	}

	s := NewStore()
	s.Update("ns-a", model, "v1", live(9000), time.Now())
	s.Update("ns-a", model, "v2", live(7000), time.Now())
	s.Update("ns-a", model, "v3", live(8000), time.Now())
	s.Update("ns-b", model, "v1", live(3000), time.Now())
	dep := live(1000)
	dep.LearnedFrom = "deployment"
	s.Update("ns-a", model, "v4", dep, time.Now())

	for range 20 {
		got := s.FindClampBound("ns-a", model, accel, 8, &params)
		require.NotNil(t, got)
		assert.Equal(t, int64(7000), got.EffectiveCapacity,
			"the lowest LIVE figure in the variant's own namespace, every time")
	}
	got := s.FindClampBound("ns-c", model, accel, 8, &params)
	require.NotNil(t, got)
	assert.Equal(t, int64(3000), got.EffectiveCapacity, "with none of its own, the lowest across namespaces")

	other := params
	other.MaxNumSeqs = 64
	assert.Nil(t, s.FindClampBound("ns-a", model, accel, 8, &other), "only clamp-compatible records")
	assert.Nil(t, s.FindClampBound("ns-a", model, accel, 4, &params), "only on the same hardware")
}
