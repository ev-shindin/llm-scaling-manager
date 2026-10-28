package capacity

import (
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/inferenceengine"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Store", func() {

	var store *Store

	BeforeEach(func() {
		store = NewStore()
	})

	Describe("Store and retrieve", func() {
		It("should store and retrieve a capacity record by namespace/model/variant", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				NumGpuBlocks:          1000,
				BlockSize:             16,
				TotalKvCapacityTokens: 16000,
				EffectiveCapacity:     14000,
				LearnedFrom:           "live",
			}, time.Now())

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(16000)))
			Expect(got.AcceleratorName).To(Equal("H100"))
			Expect(got.GpuCount).To(Equal(1))
			Expect(got.LearnedFrom).To(Equal("live"))
		})

		It("should keep records separate across namespaces", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{
				TotalKvCapacityTokens: 16000,
				LearnedFrom:           "live",
			}, time.Now())
			store.Update("ns-2", "model-a", "variant-h100", Record{
				TotalKvCapacityTokens: 32000,
				LearnedFrom:           "live",
			}, time.Now())

			Expect(store.Get("ns-1", "model-a", "variant-h100").TotalKvCapacityTokens).To(Equal(int64(16000)))
			Expect(store.Get("ns-2", "model-a", "variant-h100").TotalKvCapacityTokens).To(Equal(int64(32000)))
		})
	})

	Describe("Get missing key", func() {
		It("should return nil for a key that does not exist", func() {
			Expect(store.Get("ns-1", "nonexistent", "variant")).To(BeNil())
		})
	})

	Describe("LoadFromScaleTarget", func() {
		It("should populate EngineParams from deployment args", func() {
			deploy := makeTestDeployment("--gpu-memory-utilization=0.85", "--max-num-batched-tokens=4096")
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-a100", "A100", 2, scaletarget.NewDeploymentAccessor(deploy), time.Now())

			got := store.Get("ns-1", "model-a", "variant-a100")
			Expect(got).NotTo(BeNil())
			Expect(got.EngineParams).NotTo(BeNil())
			Expect(got.EngineParams.GpuMemoryUtilization).To(Equal(0.85))
			Expect(got.EngineParams.MaxNumBatchedTokens).To(Equal(int64(4096)))
			Expect(got.AcceleratorName).To(Equal("A100"))
			Expect(got.GpuCount).To(Equal(2))
			Expect(got.LearnedFrom).To(Equal("deployment"))
		})

		It("should populate SGLang EngineParams and TotalKvCapacityTokens from --max-total-tokens", func() {
			// SGLang is detected from the launch command; --max-total-tokens maps to
			// TotalKvTokensOverride and is used as the total KV capacity directly
			// (vLLM's num_gpu_blocks path must not be taken).
			deploy := &appsv1.Deployment{
				Spec: appsv1.DeploymentSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:    "sglang",
								Image:   "lmsysorg/sglang:latest",
								Command: []string{"python3", "-m", "sglang.launch_server"},
								Args:    []string{"--mem-fraction-static=0.85", "--max-total-tokens=100000"},
							}},
						},
					},
				},
			}
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-sglang", "H100", 1, scaletarget.NewDeploymentAccessor(deploy), time.Now())

			got := store.Get("ns-1", "model-a", "variant-sglang")
			Expect(got).NotTo(BeNil())
			Expect(got.EngineParams).NotTo(BeNil())
			Expect(got.EngineParams.Engine).To(Equal(inferenceengine.EngineSGLang))
			Expect(got.EngineParams.TotalKvTokensOverride).To(Equal(int64(100000)))
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(100000)))
			Expect(got.NumGpuBlocks).To(Equal(int64(0)))
		})

		It("should not overwrite live data with deployment data", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{
				TotalKvCapacityTokens: 16000,
				LearnedFrom:           "live",
			}, time.Now())

			deploy := makeTestDeployment("--gpu-memory-utilization=0.85")
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, scaletarget.NewDeploymentAccessor(deploy), time.Now())

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got.LearnedFrom).To(Equal("live"))
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(16000)))
		})

		Describe("a live record and a rollout that changes the engine's arguments", func() {
			// The live write carries the deployment-parsed EngineParams
			// forward, as the analyzer does after pricing a replica.
			goLive := func() {
				loaded := store.Get("ns-1", "model-a", "variant-h100")
				Expect(loaded).NotTo(BeNil())
				store.Update("ns-1", "model-a", "variant-h100", Record{
					AcceleratorName:       "H100",
					GpuCount:              1,
					TotalKvCapacityTokens: 16000,
					EffectiveCapacity:     12800,
					EngineParams:          loaded.EngineParams,
					LearnedFrom:           LearnedFromLive,
				}, time.Now())
			}
			load := func(args ...string) {
				store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1,
					scaletarget.NewDeploymentAccessor(makeTestDeployment(args...)), time.Now())
			}

			It("keeps the live record while the arguments are the ones it was learned under", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=256")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).To(Equal(LearnedFromLive))
				Expect(got.EffectiveCapacity).To(Equal(int64(12800)))
			})

			It("replaces it once they change, so neither the parameters nor the capacity describe the old engine", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=64")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.EngineParams.MaxNumSeqs).To(Equal(int64(64)),
					"the parameters stayed frozen on the configuration that was replaced")
				Expect(got.LearnedFrom).NotTo(Equal(LearnedFromLive),
					"a capacity measured under --max-num-seqs=256 kept pricing an engine running 64")
			})

			loadImage := func(image string, args ...string) {
				d := makeTestDeployment(args...)
				d.Spec.Template.Spec.Containers[0].Image = image
				store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1,
					scaletarget.NewDeploymentAccessor(d), time.Now())
			}

			It("keeps the live record across a rollout that changes only the image", func() {
				// Replaced, a variant parked at zero was priced from the
				// derived figure: 320,000 tokens at the defaults against the
				// 12,800 measured.
				loadImage("registry.example/vllm:v0.11.0", "--max-num-seqs=256")
				goLive()
				before := store.Get("ns-1", "model-a", "variant-h100").EngineParams.Fingerprint()
				loadImage("registry.example/vllm:v0.11.1", "--max-num-seqs=256")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).To(Equal(LearnedFromLive), "a routine image bump discarded the measured capacity")
				Expect(got.EffectiveCapacity).To(Equal(int64(12800)))
				// The parameters still move to the new release: left on the
				// old one, the fingerprint and the snapshot's tag named it,
				// and the next restart restored nothing.
				Expect(got.EngineParams.Image).To(Equal("registry.example/vllm:v0.11.1"),
					"the live record kept naming the release it replaced")
				Expect(got.EngineParams.Fingerprint()).NotTo(Equal(before))
			})

			It("keeps what it carried on the next cycle's re-read", func() {
				// LoadFromScaleTarget runs every cycle and writes a record
				// that is not live afresh; the carry held for one cycle.
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=64")
				load("--max-num-seqs=64")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.TotalKvCapacityTokens).To(Equal(int64(16000)), "the carried k1 was lost a cycle later")
				Expect(got.PriorLiveCapacity).To(Equal(int64(12800)), "the ceiling was lost a cycle later")
			})

			It("keeps the last measured capacity as a ceiling when the cache changed shape", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=256", "--gpu-memory-utilization=0.85")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).NotTo(Equal(LearnedFromLive))
				Expect(got.PriorLiveCapacity).To(Equal(int64(12800)),
					"a parked variant was left with no bound but the derived figure")
			})

			It("does not carry the KV total to a larger per-step budget", func() {
				// A larger profiling peak leaves the cache smaller; the old
				// total would over-state it.
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=512")
				Expect(store.Get("ns-1", "model-a", "variant-h100").TotalKvCapacityTokens).To(BeZero(),
					"a KV total measured at --max-num-seqs=256 bounded an engine capturing graphs for 512")

				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=256", "--max-num-batched-tokens=65536")
				Expect(store.Get("ns-1", "model-a", "variant-h100").TotalKvCapacityTokens).To(BeZero(),
					"a KV total measured at the default budget bounded an engine profiled at 65536")
			})

			loadOn := func(accelerator string, gpus int) {
				store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", accelerator, gpus,
					scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seqs=256")), time.Now())
			}

			It("replaces it when the variant moves to other hardware, with no KV total carried", func() {
				load("--max-num-seqs=256")
				goLive()
				loadOn("H200", 1)
				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).NotTo(Equal(LearnedFromLive), "an H100 measurement kept pricing an H200 engine")
				Expect(got.TotalKvCapacityTokens).To(BeZero())
				Expect(got.PriorLiveCapacity).To(BeZero(), "an H100 measurement capped an H200 engine's estimate")

				load("--max-num-seqs=256")
				goLive()
				loadOn("H100", 2)
				got = store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).NotTo(Equal(LearnedFromLive),
					"a one-GPU measurement kept pricing a two-GPU engine")
				Expect(got.PriorLiveCapacity).To(BeZero(), "a one-GPU measurement capped a two-GPU engine's estimate")
			})

			It("drops the ceiling when a parked variant moves to other hardware later", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=64")
				Expect(store.Get("ns-1", "model-a", "variant-h100").PriorLiveCapacity).To(Equal(int64(12800)))

				store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H200", 1,
					scaletarget.NewDeploymentAccessor(makeTestDeployment("--max-num-seqs=64")), time.Now())
				Expect(store.Get("ns-1", "model-a", "variant-h100").PriorLiveCapacity).To(BeZero(),
					"the ceiling outlived the hardware it was measured on")
			})

			It("ages the ceiling out on the measurement's clock, not the record's", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=64")
				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.PriorLiveAt).NotTo(BeZero())

				// Rewritten every cycle, the record's own LearnedAt is always
				// fresh; the measurement is not.
				aged := *got
				aged.PriorLiveAt = time.Now().Add(-EvictionTimeout - time.Hour)
				store.records[storeKey("ns-1", "model-a", "variant-h100")] = &aged
				load("--max-num-seqs=64")
				got = store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.PriorLiveCapacity).To(BeZero(), "a measurement older than the eviction horizon still capped the estimate")
				Expect(got.PriorLiveAt).To(BeZero())
			})

			It("does not take an unresolved accelerator for a move", func() {
				load("--max-num-seqs=256")
				goLive()
				loadOn("", 1)
				Expect(store.Get("ns-1", "model-a", "variant-h100").LearnedFrom).To(Equal(LearnedFromLive))
			})

			It("carries the measured KV capacity as k1 across a scheduler-flag change", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=64")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).NotTo(Equal(LearnedFromLive))
				Expect(got.TotalKvCapacityTokens).To(Equal(int64(16000)),
					"the KV cache is the same memory; without it a parked variant's derived figure has no k1 bound")
			})

			It("does not carry it when what sizes the KV cache changed", func() {
				load("--max-num-seqs=256")
				goLive()
				load("--max-num-seqs=256", "--gpu-memory-utilization=0.5")

				got := store.Get("ns-1", "model-a", "variant-h100")
				Expect(got.LearnedFrom).NotTo(Equal(LearnedFromLive))
				Expect(got.TotalKvCapacityTokens).To(BeZero(),
					"a KV total measured at 0.9 of the memory bounded an engine given 0.5")
			})
		})

		It("should handle nil scale target gracefully", func() {
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, nil, time.Now())
			Expect(store.Get("ns-1", "model-a", "variant-h100")).To(BeNil())
		})

		It("should set conservative EffectiveCapacity from EffectiveMaxBatchedTokens", func() {
			// Default vLLM V1 deployment (no overrides)
			deploy := makeTestDeployment()
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, scaletarget.NewDeploymentAccessor(deploy), time.Now())

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
			// V1 engine default EffectiveMaxBatchedTokens = 8192
			Expect(got.EffectiveCapacity).To(Equal(int64(8192)))
			Expect(got.LearnedFrom).To(Equal("deployment"))
		})

		It("should use num_gpu_blocks_override for k1 without overriding EffectiveCapacity", func() {
			deploy := makeTestDeployment("--num-gpu-blocks-override=5000", "--block-size=16")
			store.LoadFromScaleTarget("ns-1", "model-a", "variant-h100", "H100", 1, scaletarget.NewDeploymentAccessor(deploy), time.Now())

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
			Expect(got.TotalKvCapacityTokens).To(Equal(int64(80000))) // 5000 * 16
			// EffectiveCapacity should NOT be overwritten since TotalKvCapacityTokens is set
			// but LoadFromScaleTarget doesn't compute k1 from TotalKvCapacityTokens directly,
			// it only sets EffectiveCapacity from EffectiveMaxBatchedTokens as a fallback
			// Since num_gpu_blocks_override doesn't set EffectiveCapacity,
			// the fallback EffectiveMaxBatchedTokens (8192) is used
			Expect(got.EffectiveCapacity).To(Equal(int64(8192)))
		})
	})

	Describe("Staleness", func() {
		It("should report missing keys as stale", func() {
			Expect(store.IsStale("ns-1", "x", "y", time.Now())).To(BeTrue())
		})

		It("should report fresh records as not stale", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{LearnedFrom: "live"}, time.Now())
			Expect(store.IsStale("ns-1", "model-a", "variant-h100", time.Now())).To(BeFalse())
		})

		It("should report old records as stale", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{LearnedFrom: "live"}, time.Now())

			// Manually age the record
			store.mu.Lock()
			rec := store.records[storeKey("ns-1", "model-a", "variant-h100")]
			rec.LearnedAt = time.Now().Add(-StalenessTimeout - time.Minute)
			store.mu.Unlock()

			Expect(store.IsStale("ns-1", "model-a", "variant-h100", time.Now())).To(BeTrue())
		})
	})

	Describe("Concurrent access", func() {
		It("should handle concurrent reads and writes without panicking", func() {
			var wg sync.WaitGroup

			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					store.Update("ns-1", "model-a", "variant-h100", Record{
						TotalKvCapacityTokens: int64(i * 1000),
						LearnedFrom:           "live",
					}, time.Now())
				}(i)
			}

			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = store.Get("ns-1", "model-a", "variant-h100")
					_ = store.IsStale("ns-1", "model-a", "variant-h100", time.Now())
				}()
			}

			wg.Wait()

			got := store.Get("ns-1", "model-a", "variant-h100")
			Expect(got).NotTo(BeNil())
		})
	})

	Describe("Cross-variant lookup", func() {
		It("should keep separate records for the same model on different variants", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{
				AcceleratorName:       "H100",
				TotalKvCapacityTokens: 32000,
				LearnedFrom:           "live",
			}, time.Now())
			store.Update("ns-1", "model-a", "variant-a100", Record{
				AcceleratorName:       "A100",
				TotalKvCapacityTokens: 16000,
				LearnedFrom:           "live",
			}, time.Now())

			Expect(store.Get("ns-1", "model-a", "variant-h100").TotalKvCapacityTokens).To(Equal(int64(32000)))
			Expect(store.Get("ns-1", "model-a", "variant-a100").TotalKvCapacityTokens).To(Equal(int64(16000)))
		})
	})

	Describe("FindCompatible", func() {
		defaultParams := func() *EngineParams {
			p := defaultEngineParams()
			resolveEffectiveMaxBatchedTokens(&p)
			return &p
		}

		It("should find a compatible record from another variant", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-1", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			}, time.Now())

			// Search for a compatible record for a different H100 variant
			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).NotTo(BeNil())
			Expect(found.EffectiveCapacity).To(Equal(int64(28000)))
		})

		It("should not match across different accelerator types", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			}, time.Now())

			// Search for A100 — should not match H100 record
			found := store.FindCompatible("model-a", "A100", 1, params)
			Expect(found).To(BeNil())
		})

		It("should not match across different GPU counts", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-1gpu", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			}, time.Now())

			// Search for 2-GPU H100 — should not match 1-GPU record
			found := store.FindCompatible("model-a", "H100", 2, params)
			Expect(found).To(BeNil())
		})

		It("should not match across different engine parameters", func() {
			params1 := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-high-util", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params1,
				LearnedFrom:           "live",
			}, time.Now())

			// Search with different gpu_memory_utilization
			params2 := defaultParams()
			params2.GpuMemoryUtilization = 0.5
			found := store.FindCompatible("model-a", "H100", 1, params2)
			Expect(found).To(BeNil())
		})

		It("should match across different namespaces (capacity is hardware-dependent)", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			}, time.Now())

			// Record in ns-1 should be found when searching for model-a
			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).NotTo(BeNil())
			Expect(found.EffectiveCapacity).To(Equal(int64(28000)))
		})

		It("should not match across different models", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			}, time.Now())

			// Different model should not match
			found := store.FindCompatible("model-b", "H100", 1, params)
			Expect(found).To(BeNil())
		})

		It("should prefer live records over deployment-derived records", func() {
			params := defaultParams()

			store.Update("ns-1", "model-a", "variant-h100-deploy", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 30000,
				EffectiveCapacity:     25000,
				EngineParams:          params,
				LearnedFrom:           "deployment",
			}, time.Now())
			store.Update("ns-1", "model-a", "variant-h100-live", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          params,
				LearnedFrom:           "live",
			}, time.Now())

			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).NotTo(BeNil())
			Expect(found.LearnedFrom).To(Equal("live"))
			Expect(found.EffectiveCapacity).To(Equal(int64(28000)))
		})

		It("should skip records with no useful capacity data", func() {
			params := defaultParams()
			store.Update("ns-1", "model-a", "variant-h100-empty", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				EngineParams:          params,
				EffectiveCapacity:     0,
				TotalKvCapacityTokens: 0,
				LearnedFrom:           "deployment",
			}, time.Now())

			found := store.FindCompatible("model-a", "H100", 1, params)
			Expect(found).To(BeNil())
		})

		It("should return nil when nil params are provided", func() {
			store.Update("ns-1", "model-a", "variant-h100", Record{
				AcceleratorName:       "H100",
				GpuCount:              1,
				TotalKvCapacityTokens: 32000,
				EffectiveCapacity:     28000,
				EngineParams:          defaultParams(),
				LearnedFrom:           "live",
			}, time.Now())

			found := store.FindCompatible("model-a", "H100", 1, nil)
			Expect(found).To(BeNil())
		})
	})
})

// makeTestDeployment creates a minimal Deployment with the given vLLM args.
func makeTestDeployment(args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "vllm",
							Command: []string{"vllm", "serve", "model-name"},
							Args:    args,
						},
					},
				},
			},
		},
	}
}
