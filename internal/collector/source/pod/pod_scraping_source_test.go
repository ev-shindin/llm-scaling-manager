package pod

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sourcepkg "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/testutil"
)

var _ = Describe("PodScrapingSource", func() {
	var (
		ctx        context.Context
		fakeClient *fake.ClientBuilder
		scheme     *runtime.Scheme
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		fakeClient = fake.NewClientBuilder().WithScheme(scheme)
	})

	Describe("NewPodScrapingSource", func() {
		It("should create source with provided service name", func() {
			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).NotTo(HaveOccurred())
			Expect(source).NotTo(BeNil())
			Expect(source.config.ServiceName).To(Equal("test-pool-epp"))
		})

		It("should return error if ServiceName is empty", func() {
			config := PodScrapingSourceConfig{
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			_, err := NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ServiceName is required"))
		})

		It("should return error if ServiceNamespace is empty", func() {
			config := PodScrapingSourceConfig{
				ServiceName: "test-pool-epp",
				MetricsPort: 9090,
			}
			_, err := NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ServiceNamespace is required"))
		})

		It("should set defaults for missing config values", func() {
			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).NotTo(HaveOccurred())
			Expect(source.config.MetricsPath).To(Equal("/metrics"))
			Expect(source.config.MetricsScheme).To(Equal("http"))
			Expect(source.config.ScrapeTimeout).To(Equal(5 * time.Second))
			Expect(source.config.MaxConcurrentScrapes).To(Equal(10))
			Expect(source.config.DefaultTTL).To(Equal(30 * time.Second))
		})
	})

	Describe("service name validation", func() {
		It("should require service name to be provided", func() {
			config := PodScrapingSourceConfig{
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			_, err := NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("ServiceName is required"))
		})
	})

	Describe("isPodReady", func() {
		It("should return true for Ready pod", func() {
			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}
			Expect(isPodReady(pod)).To(BeTrue())
		})

		It("should return false for not Ready pod", func() {
			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			}
			Expect(isPodReady(pod)).To(BeFalse())
		})

		It("should return false for pod without Ready condition", func() {
			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodInitialized,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}
			Expect(isPodReady(pod)).To(BeFalse())
		})
	})

	Describe("discoverPods", func() {
		var (
			service *corev1.Service
			pod1    *corev1.Pod
			pod2    *corev1.Pod
			pod3    *corev1.Pod // Not ready
		)

		BeforeEach(func() {
			service = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool-epp",
					Namespace: "test-ns",
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
			}

			pod1 = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-1",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			pod2 = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-2",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.2",
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			pod3 = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-3",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.3",
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			}
		})

		It("should discover Ready pods only", func() {
			client := fakeClient.
				WithObjects(service, pod1, pod2, pod3).
				Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			pods, err := source.discoverPods(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(HaveLen(2))
			Expect(pods[0].Name).To(BeElementOf("epp-pod-1", "epp-pod-2"))
			Expect(pods[1].Name).To(BeElementOf("epp-pod-1", "epp-pod-2"))
		})

		It("should return error if service not found", func() {
			client := fakeClient.Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "nonexistent-service",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			_, err = source.discoverPods(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to get service"))
		})

		It("should return empty list if no pods match selector", func() {
			client := fakeClient.
				WithObjects(service).
				Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			pods, err := source.discoverPods(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty())
		})

		It("should return empty list if service has no selector (headless service)", func() {
			// Create a service without selector (headless service)
			headlessService := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "headless-service",
					Namespace: "test-ns",
				},
				Spec: corev1.ServiceSpec{
					Selector:  map[string]string{}, // Empty selector
					ClusterIP: "None",              // Headless service
				},
			}

			client := fakeClient.
				WithObjects(headlessService).
				Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "headless-service",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			pods, err := source.discoverPods(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty(), "Should return empty list for service without selector")
		})

		It("should return empty list if service selector is nil", func() {
			// Create a service with nil selector
			serviceWithNilSelector := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-selector-service",
					Namespace: "test-ns",
				},
				Spec: corev1.ServiceSpec{
					Selector: nil, // Nil selector
				},
			}

			client := fakeClient.
				WithObjects(serviceWithNilSelector).
				Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "nil-selector-service",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			pods, err := source.discoverPods(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pods).To(BeEmpty(), "Should return empty list for service with nil selector")
		})
	})

	Describe("getAuthToken", func() {
		It("should return the BearerToken when configured", func() {
			client := fakeClient.Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				BearerToken:      "explicit-token",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			token, useAuth := source.getAuthToken()
			Expect(useAuth).To(BeTrue())
			Expect(token).To(Equal("explicit-token"))
		})

		It("should skip authentication when no BearerToken is configured", func() {
			client := fakeClient.Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			token, useAuth := source.getAuthToken()
			Expect(useAuth).To(BeFalse())
			Expect(token).To(BeEmpty())
		})

		// A rotated token must reach the next scrape, not wait for the pool to
		// be registered again.
		It("should re-read BearerTokenFile on every call", func() {
			client := fakeClient.Build()
			path := filepath.Join(GinkgoT().TempDir(), "token")
			Expect(os.WriteFile(path, []byte("first "), 0o600)).To(Succeed())

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				BearerToken:      "registered",
				BearerTokenFile:  path,
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			token, useAuth := source.getAuthToken()
			Expect(useAuth).To(BeTrue())
			Expect(token).To(Equal("first"))

			Expect(os.WriteFile(path, []byte("rotated"), 0o600)).To(Succeed())
			token, useAuth = source.getAuthToken()
			Expect(useAuth).To(BeTrue())
			Expect(token).To(Equal("rotated"))
		})

		It("should fall back to BearerToken while BearerTokenFile is unreadable", func() {
			client := fakeClient.Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				BearerToken:      "registered",
				BearerTokenFile:  filepath.Join(GinkgoT().TempDir(), "missing"),
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			token, useAuth := source.getAuthToken()
			Expect(useAuth).To(BeTrue())
			Expect(token).To(Equal("registered"))
		})

		// An empty (or whitespace-only) file is a mis-mounted Secret more often
		// than a rotation in flight: fall back, and say so once at Info.
		DescribeTable("should fall back to BearerToken, logging once at Info, while BearerTokenFile is blank",
			func(content string) {
				path := filepath.Join(GinkgoT().TempDir(), "token")
				Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
				source, err := NewPodScrapingSource(ctx, fakeClient.Build(), PodScrapingSourceConfig{
					ServiceName:      "test-pool-epp",
					ServiceNamespace: "test-ns",
					BearerToken:      "registered",
					BearerTokenFile:  path,
					MetricsPort:      9090,
				})
				Expect(err).NotTo(HaveOccurred())
				var logged []string
				// Verbosity 0: only Info-level lines reach the sink, so a
				// V(DEBUG) line would not count.
				source.log = funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{Verbosity: 0})

				for range 3 {
					token, useAuth := source.getAuthToken()
					Expect(useAuth).To(BeTrue())
					Expect(token).To(Equal("registered"))
				}
				Expect(logged).To(HaveLen(1), "logged once, not once per scrape")
				Expect(logged[0]).To(ContainSubstring("empty"))

				// A token in the file re-arms the log for the next empty spell.
				Expect(os.WriteFile(path, []byte("rotated"), 0o600)).To(Succeed())
				token, _ := source.getAuthToken()
				Expect(token).To(Equal("rotated"))
				Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
				_, _ = source.getAuthToken()
				Expect(logged).To(HaveLen(2))
			},
			Entry("empty file", ""),
			Entry("whitespace-only file", " \n\t\n"),
		)
	})

	Describe("parsePrometheusMetrics", func() {
		var source *PodScrapingSource

		BeforeEach(func() {
			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			var err error
			source, err = NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should parse Prometheus text format", func() {
			metricsText := `# HELP vllm_kv_cache_usage_perc KV cache usage percentage
# TYPE vllm_kv_cache_usage_perc gauge
vllm_kv_cache_usage_perc{namespace="test-ns"} 0.75
# HELP vllm_num_requests_waiting Number of requests waiting
# TYPE vllm_num_requests_waiting gauge
vllm_num_requests_waiting{namespace="test-ns"} 5
`

			result, err := source.parsePrometheusMetrics(
				&mockReader{data: []byte(metricsText)},
				"test-pod",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Values).To(HaveLen(2))
			Expect(result.QueryName).To(Equal("all_metrics"))

			// Check metrics (order not guaranteed from map iteration)
			metricsByName := make(map[string]sourcepkg.MetricValue)
			for _, value := range result.Values {
				Expect(value.Labels["pod"]).To(Equal("test-pod"))
				metricsByName[value.Labels["__name__"]] = value
			}

			// Check first metric
			Expect(metricsByName).To(HaveKey("vllm_kv_cache_usage_perc"))
			Expect(metricsByName["vllm_kv_cache_usage_perc"].Value).To(Equal(0.75))

			// Check second metric
			Expect(metricsByName).To(HaveKey("vllm_num_requests_waiting"))
			Expect(metricsByName["vllm_num_requests_waiting"].Value).To(Equal(5.0))
		})

		It("should handle empty metrics", func() {
			result, err := source.parsePrometheusMetrics(
				&mockReader{data: []byte("")},
				"test-pod",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.Values).To(BeEmpty())
		})

		It("should return error for invalid Prometheus format", func() {
			_, err := source.parsePrometheusMetrics(
				&mockReader{data: []byte("invalid prometheus format!!!")},
				"test-pod",
			)
			Expect(err).To(HaveOccurred())
		})
	})

	Describe("Refresh", func() {
		var (
			service     *corev1.Service
			readyPod1   *corev1.Pod
			readyPod2   *corev1.Pod
			mockServer1 *httptest.Server
			mockServer2 *httptest.Server
			registry    *prometheus.Registry
		)

		BeforeEach(func() {
			// Initialize metrics for error recording
			registry = prometheus.NewRegistry()
			Expect(metrics.InitMetrics(registry)).To(Succeed())

			service = &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool-epp",
					Namespace: "test-ns",
				},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
			}

			// Create mock HTTP servers for pods
			mockServer1 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Verify Authorization header
				auth := r.Header.Get("Authorization")
				Expect(auth).To(Equal("Bearer test-token"))
				Expect(r.URL.Path).To(Equal("/metrics"))

				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `# HELP vllm_kv_cache_usage_perc KV cache usage
# TYPE vllm_kv_cache_usage_perc gauge
vllm_kv_cache_usage_perc{namespace="test-ns"} 0.75
# HELP vllm_num_requests_waiting Number of requests waiting
# TYPE vllm_num_requests_waiting gauge
vllm_num_requests_waiting{namespace="test-ns"} 5
`)
			}))

			mockServer2 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				Expect(auth).To(Equal("Bearer test-token"))
				Expect(r.URL.Path).To(Equal("/metrics"))

				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, `# HELP vllm_kv_cache_usage_perc KV cache usage
# TYPE vllm_kv_cache_usage_perc gauge
vllm_kv_cache_usage_perc{namespace="test-ns"} 0.50
# HELP vllm_num_requests_waiting Number of requests waiting
# TYPE vllm_num_requests_waiting gauge
vllm_num_requests_waiting{namespace="test-ns"} 3
`)
			}))

			// Extract host and port from mock server URLs
			// httptest.Server URL format: "http://127.0.0.1:PORT"
			// We'll use localhost IP and extract the port
			server1URL := mockServer1.URL
			server2URL := mockServer2.URL

			// Parse URLs to get host:port
			// For testing, we'll use 127.0.0.1 as pod IP and extract port
			readyPod1 = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-1",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "127.0.0.1", // Will be overridden with actual server address
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			readyPod2 = &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-2",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "127.0.0.1", // Will be overridden with actual server address
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			// Store server URLs for later use in tests
			_ = server1URL
			_ = server2URL
		})

		AfterEach(func() {
			if mockServer1 != nil {
				mockServer1.Close()
			}
			if mockServer2 != nil {
				mockServer2.Close()
			}
		})

		It("should scrape metrics from all Ready pods and aggregate results", func() {
			// Parse server URLs to extract ports
			server1URL := mockServer1.URL
			server2URL := mockServer2.URL

			// Extract port from URLs (format: http://127.0.0.1:PORT)
			var port1, port2 int32
			_, err := fmt.Sscanf(server1URL, "http://127.0.0.1:%d", &port1)
			Expect(err).NotTo(HaveOccurred())
			_, err = fmt.Sscanf(server2URL, "http://127.0.0.1:%d", &port2)
			Expect(err).NotTo(HaveOccurred())

			// Update pods with correct IPs
			readyPod1.Status.PodIP = "127.0.0.1"
			readyPod2.Status.PodIP = "127.0.0.1"

			// Create separate sources for each pod (since they use different ports)
			// In real scenario, all pods use same port but different IPs
			client1 := fakeClient.
				WithObjects(service, readyPod1).
				Build()

			config1 := PodScrapingSourceConfig{
				ServiceName:          "test-pool-epp",
				ServiceNamespace:     "test-ns",
				MetricsPort:          port1,
				MetricsPath:          "/metrics",
				MetricsScheme:        "http",
				ScrapeTimeout:        5 * time.Second,
				MaxConcurrentScrapes: 10,
				BearerToken:          "test-token",
			}
			source1, err := NewPodScrapingSource(ctx, client1, config1)
			Expect(err).NotTo(HaveOccurred())

			// Test scraping from first pod
			results1, err := source1.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).NotTo(HaveOccurred())
			Expect(results1).To(HaveKey("all_metrics"))
			Expect(results1["all_metrics"].Values).To(HaveLen(2)) // 2 metrics from pod1

			// Verify metrics have pod label
			for _, value := range results1["all_metrics"].Values {
				Expect(value.Labels["pod"]).To(Equal("epp-pod-1"))
			}

			// Verify per-metric keys are also present in the result map.
			Expect(results1).To(HaveKey("vllm_kv_cache_usage_perc"))
			Expect(results1).To(HaveKey("vllm_num_requests_waiting"))
			Expect(results1["vllm_kv_cache_usage_perc"].Values).To(HaveLen(1))
			Expect(results1["vllm_num_requests_waiting"].Values).To(HaveLen(1))

			// Verify Get returns per-metric values after Refresh.
			cachedKV := source1.Get("vllm_kv_cache_usage_perc", nil)
			Expect(cachedKV).NotTo(BeNil())
			Expect(cachedKV.Result.Values).To(HaveLen(1))
			Expect(cachedKV.Result.Values[0].Value).To(Equal(0.75))
		})

		It("should handle unreachable pods gracefully", func() {
			// Create pod with invalid IP
			unreachablePod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-unreachable",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "192.0.2.1", // Invalid/unreachable IP (TEST-NET-1)
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			client := fakeClient.
				WithObjects(service, unreachablePod).
				Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
				ScrapeTimeout:    1 * time.Second, // Short timeout
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			// Every Ready pod unreachable is a failure to look, not an empty
			// answer. This spec used to assert a nil error here, and that nil is
			// what kept the scale-from-zero engine off its Prometheus fallback:
			// it read the empty result as "the EPP exports no queue".
			results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("test-ns/test-pool-epp"))
			Expect(results).To(BeNil())
		})

		It("should handle authentication failures", func() {
			// Create server that requires auth but we'll provide wrong token
			authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				if auth != "Bearer test-token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer authServer.Close()

			var port int32
			if _, err := fmt.Sscanf(authServer.URL, "http://127.0.0.1:%d", &port); err != nil {
				Fail(fmt.Sprintf("failed to parse port from auth server URL %q: %v", authServer.URL, err))
			}

			authPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "epp-pod-auth",
					Namespace: "test-ns",
					Labels: map[string]string{
						"inferencepool": "test-pool-epp",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "127.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}

			client := fakeClient.
				WithObjects(service, authPod).
				Build()

			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      port,
				ScrapeTimeout:    1 * time.Second,
				BearerToken:      "wrong-token",
			}
			source, err := NewPodScrapingSource(ctx, client, config)
			Expect(err).NotTo(HaveOccurred())

			// A 401 from every pod is the first case the scale-from-zero
			// Prometheus fallback exists for (queue_fallback.go), so it must be
			// an error the engine can count. This spec used to assert nil.
			results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("401"))
			Expect(results).To(BeNil())
		})

		It("should return the pods that answered, and no error, when only some fail", func() {
			var port int32
			_, err := fmt.Sscanf(mockServer1.URL, "http://127.0.0.1:%d", &port)
			Expect(err).NotTo(HaveOccurred())
			noIP := readyPod2.DeepCopy()
			noIP.Status.PodIP = "" // scrapePodMetrics fails before any request

			source, err := NewPodScrapingSource(ctx, fakeClient.WithObjects(service, readyPod1, noIP).Build(),
				PodScrapingSourceConfig{
					ServiceName:      "test-pool-epp",
					ServiceNamespace: "test-ns",
					MetricsPort:      port,
					BearerToken:      "test-token",
				})
			Expect(err).NotTo(HaveOccurred())

			results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).NotTo(HaveOccurred(), "one pod answering is an answer")
			Expect(results["all_metrics"].Values).To(HaveLen(2))
		})

		It("should log a failing scrape once per change and keep the last good cache", func() {
			failing := atomic.Bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if failing.Load() {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = fmt.Fprint(w, "llm_d_epp_flow_control_queue_size{target_model_name=\"m\"} 2\n")
			}))
			defer server.Close()
			var port int32
			_, err := fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port)
			Expect(err).NotTo(HaveOccurred())

			source, err := NewPodScrapingSource(ctx, fakeClient.WithObjects(service, readyPod1).Build(),
				PodScrapingSourceConfig{
					ServiceName:      "test-pool-epp",
					ServiceNamespace: "test-ns",
					MetricsPort:      port,
				})
			Expect(err).NotTo(HaveOccurred())
			var logged []string
			// Verbosity 0: the per-pod V(VERBOSE) line does not reach the sink,
			// so what is counted is the once-per-change line alone.
			source.log = funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{Verbosity: 0})

			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).NotTo(HaveOccurred())

			failing.Store(true)
			for range 3 {
				_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
				Expect(err).To(HaveOccurred())
			}
			Expect(logged).To(HaveLen(1), "logged once on the change, not once per scrape")
			Expect(logged[0]).To(ContainSubstring(`"namespace"="test-ns"`))
			Expect(logged[0]).To(ContainSubstring("500"))

			cached := source.Get("all_metrics", nil)
			Expect(cached).NotTo(BeNil(), "a failed scrape must not overwrite the last good answer")
			Expect(cached.Result.Values).To(HaveLen(1))

			failing.Store(false)
			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).NotTo(HaveOccurred())
			Expect(logged).To(HaveLen(2))
			Expect(logged[1]).To(ContainSubstring("recovered"))
		})

		// No Ready pod is not an empty answer either. Refresh used to return an
		// empty all_metrics and a nil error here, which the scale-from-zero
		// engine read as "the EPP exports no queue", so a crash-looping or
		// scaled-to-zero EPP parked the model behind no-wake-signal.
		DescribeTable("should return ErrNoReadyPods when the service selects no Ready pod",
			func(objs func() []ctrlclient.Object) {
				source, err := NewPodScrapingSource(ctx, fakeClient.WithObjects(objs()...).Build(),
					PodScrapingSourceConfig{
						ServiceName:      "test-pool-epp",
						ServiceNamespace: "test-ns",
						MetricsPort:      9090,
					})
				Expect(err).NotTo(HaveOccurred())

				results, err := source.Refresh(ctx, sourcepkg.RefreshSpec{})
				Expect(err).To(MatchError(ErrNoReadyPods))
				Expect(err.Error()).To(ContainSubstring("test-ns/test-pool-epp"))
				Expect(results).To(BeNil())
			},
			Entry("every pod NotReady", func() []ctrlclient.Object {
				notReady := readyPod1.DeepCopy()
				notReady.Status.Conditions[0].Status = corev1.ConditionFalse
				return []ctrlclient.Object{service, notReady}
			}),
			Entry("no pod at all", func() []ctrlclient.Object { return []ctrlclient.Object{service} }),
		)

		It("should log a new all-failing spell after a spell with no Ready pod", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			var port int32
			_, err := fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port)
			Expect(err).NotTo(HaveOccurred())

			k8s := fakeClient.WithObjects(service, readyPod1.DeepCopy()).Build()
			source, err := NewPodScrapingSource(ctx, k8s, PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      port,
			})
			Expect(err).NotTo(HaveOccurred())
			var logged []string
			source.log = funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{Verbosity: 0})

			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).To(HaveOccurred())
			Expect(logged).To(HaveLen(1))

			Expect(k8s.Delete(ctx, readyPod1.DeepCopy())).To(Succeed())
			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).To(MatchError(ErrNoReadyPods))
			Expect(logged).To(HaveLen(1), "no pod to fail is not a line of its own")

			Expect(k8s.Create(ctx, readyPod1.DeepCopy())).To(Succeed())
			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).To(HaveOccurred())
			Expect(logged).To(HaveLen(2), "the pods came back and failed again: a new spell, a new line")
			Expect(logged[1]).To(ContainSubstring("failing on every pod"))
		})

		It("should not log a cancelled context as an outage", func() {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, "llm_d_epp_flow_control_queue_size{target_model_name=\"m\"} 2\n")
			}))
			defer server.Close()
			var port int32
			_, err := fmt.Sscanf(server.URL, "http://127.0.0.1:%d", &port)
			Expect(err).NotTo(HaveOccurred())

			source, err := NewPodScrapingSource(ctx, fakeClient.WithObjects(service, readyPod1).Build(),
				PodScrapingSourceConfig{
					ServiceName:      "test-pool-epp",
					ServiceNamespace: "test-ns",
					MetricsPort:      port,
				})
			Expect(err).NotTo(HaveOccurred())
			var logged []string
			source.log = funcr.New(func(_, args string) { logged = append(logged, args) }, funcr.Options{Verbosity: 0})

			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			_, err = source.Refresh(cancelled, sourcepkg.RefreshSpec{})
			Expect(err).To(MatchError(context.Canceled))
			Expect(err.Error()).To(ContainSubstring("scrape of every Ready pod"), "the scrape path was reached")
			Expect(logged).To(BeEmpty(), "shutdown is not an EPP outage")

			_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
			Expect(err).NotTo(HaveOccurred())
			Expect(logged).To(BeEmpty(), "nothing was failing, so nothing recovered")
		})
	})

	Describe("aggregateResults", func() {
		var source *PodScrapingSource

		BeforeEach(func() {
			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			var err error
			source, err = NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should combine metrics from all pods", func() {
			now := time.Now()
			results := map[string]*sourcepkg.MetricResult{
				"pod-1": {
					QueryName:   "all_metrics",
					Values:      []sourcepkg.MetricValue{{Value: 0.75, Labels: map[string]string{"pod": "pod-1"}}},
					CollectedAt: now.Add(-1 * time.Second),
				},
				"pod-2": {
					QueryName:   "all_metrics",
					Values:      []sourcepkg.MetricValue{{Value: 0.50, Labels: map[string]string{"pod": "pod-2"}}},
					CollectedAt: now,
				},
			}

			aggregated := source.aggregateResults(results)
			Expect(aggregated).NotTo(BeNil())
			Expect(aggregated.Values).To(HaveLen(2))
			Expect(aggregated.CollectedAt).To(Equal(now))
		})

		It("should handle empty results", func() {
			aggregated := source.aggregateResults(map[string]*sourcepkg.MetricResult{})
			Expect(aggregated).NotTo(BeNil())
			Expect(aggregated.Values).To(BeEmpty())
		})

		It("should skip nil results", func() {
			results := map[string]*sourcepkg.MetricResult{
				"pod-1": {
					QueryName:   "all_metrics",
					Values:      []sourcepkg.MetricValue{{Value: 0.75}},
					CollectedAt: time.Now(),
				},
				"pod-2": nil,
			}

			aggregated := source.aggregateResults(results)
			Expect(aggregated).NotTo(BeNil())
			Expect(aggregated.Values).To(HaveLen(1))
		})
	})

	Describe("splitByMetricName", func() {
		It("should group values by __name__ label", func() {
			now := time.Now()
			aggregated := &sourcepkg.MetricResult{
				QueryName:   "all_metrics",
				CollectedAt: now,
				Values: []sourcepkg.MetricValue{
					{Value: 0.75, Labels: map[string]string{"__name__": "vllm_kv_cache_usage_perc", "pod": "pod-1"}},
					{Value: 0.50, Labels: map[string]string{"__name__": "vllm_kv_cache_usage_perc", "pod": "pod-2"}},
					{Value: 5.0, Labels: map[string]string{"__name__": "vllm_num_requests_waiting", "pod": "pod-1"}},
				},
			}

			result := splitByMetricName(aggregated)
			Expect(result).To(HaveLen(2))

			kvCache := result["vllm_kv_cache_usage_perc"]
			Expect(kvCache).NotTo(BeNil())
			Expect(kvCache.QueryName).To(Equal("vllm_kv_cache_usage_perc"))
			Expect(kvCache.Values).To(HaveLen(2))
			Expect(kvCache.CollectedAt).To(Equal(now))

			waiting := result["vllm_num_requests_waiting"]
			Expect(waiting).NotTo(BeNil())
			Expect(waiting.QueryName).To(Equal("vllm_num_requests_waiting"))
			Expect(waiting.Values).To(HaveLen(1))
			Expect(waiting.Values[0].Value).To(Equal(5.0))
		})

		It("should skip values with no __name__ label", func() {
			aggregated := &sourcepkg.MetricResult{
				QueryName:   "all_metrics",
				CollectedAt: time.Now(),
				Values: []sourcepkg.MetricValue{
					{Value: 1.0, Labels: map[string]string{"pod": "pod-1"}},           // no __name__
					{Value: 2.0, Labels: map[string]string{"__name__": "vllm_queue"}}, // has __name__
				},
			}

			result := splitByMetricName(aggregated)
			Expect(result).To(HaveLen(1))
			Expect(result).To(HaveKey("vllm_queue"))
		})

		It("should skip values whose __name__ is the reserved 'all_metrics'", func() {
			aggregated := &sourcepkg.MetricResult{
				QueryName:   "all_metrics",
				CollectedAt: time.Now(),
				Values: []sourcepkg.MetricValue{
					{Value: 1.0, Labels: map[string]string{"__name__": "all_metrics"}}, // reserved — skip
					{Value: 2.0, Labels: map[string]string{"__name__": "vllm_queue"}},
				},
			}

			result := splitByMetricName(aggregated)
			Expect(result).To(HaveLen(1))
			Expect(result).To(HaveKey("vllm_queue"))
			Expect(result).NotTo(HaveKey("all_metrics"))
		})

		It("should return empty map for aggregated with no values", func() {
			aggregated := &sourcepkg.MetricResult{
				QueryName:   "all_metrics",
				CollectedAt: time.Now(),
				Values:      []sourcepkg.MetricValue{},
			}

			result := splitByMetricName(aggregated)
			Expect(result).To(BeEmpty())
		})
	})

	Describe("Get", func() {
		var source *PodScrapingSource

		BeforeEach(func() {
			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
				DefaultTTL:       1 * time.Hour, // Long TTL for testing
			}
			var err error
			source, err = NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should return nil for uncached query", func() {
			cached := source.Get("all_metrics", nil)
			Expect(cached).To(BeNil())
		})

		It("should return nil for uncached per-metric query", func() {
			cached := source.Get("vllm:queue_size", nil)
			Expect(cached).To(BeNil())
		})

		It("should return cached value if fresh", func() {
			// Manually set cache
			cacheKey := sourcepkg.BuildCacheKey("all_metrics", nil)
			result := sourcepkg.MetricResult{
				QueryName:   "all_metrics",
				Values:      []sourcepkg.MetricValue{{Value: 1.0}},
				CollectedAt: time.Now(),
			}
			source.cache.Set(cacheKey, result, 1*time.Hour)

			cached := source.Get("all_metrics", nil)
			Expect(cached).NotTo(BeNil())
			Expect(cached.Result.QueryName).To(Equal("all_metrics"))
			Expect(cached.Result.Values).To(HaveLen(1))
		})

		It("should return per-metric cached value for individual metric name", func() {
			// Simulate what Refresh does: store a per-metric cache entry.
			cacheKey := sourcepkg.BuildCacheKey("vllm:queue_size", nil)
			result := sourcepkg.MetricResult{
				QueryName: "vllm:queue_size",
				Values: []sourcepkg.MetricValue{
					{Value: 3.0, Labels: map[string]string{"__name__": "vllm:queue_size", "pod": "pod-1"}},
				},
				CollectedAt: time.Now(),
			}
			source.cache.Set(cacheKey, result, 1*time.Hour)

			cached := source.Get("vllm:queue_size", nil)
			Expect(cached).NotTo(BeNil())
			Expect(cached.Result.QueryName).To(Equal("vllm:queue_size"))
			Expect(cached.Result.Values).To(HaveLen(1))
			Expect(cached.Result.Values[0].Value).To(Equal(3.0))
		})

		It("should return nil for expired cache", func() {
			// Manually set cache with expired TTL
			cacheKey := sourcepkg.BuildCacheKey("all_metrics", nil)
			result := sourcepkg.MetricResult{
				QueryName:   "all_metrics",
				Values:      []sourcepkg.MetricValue{{Value: 1.0}},
				CollectedAt: time.Now().Add(-2 * time.Hour),
			}
			source.cache.Set(cacheKey, result, 1*time.Second)

			// Wait for expiration
			time.Sleep(2 * time.Second)

			cached := source.Get("all_metrics", nil)
			Expect(cached).To(BeNil())
		})
	})

	Describe("QueryList", func() {
		It("should return query registry", func() {
			config := PodScrapingSourceConfig{
				ServiceName:      "test-pool-epp",
				ServiceNamespace: "test-ns",
				MetricsPort:      9090,
			}
			source, err := NewPodScrapingSource(ctx, fakeClient.Build(), config)
			Expect(err).NotTo(HaveOccurred())

			registry := source.QueryList()
			Expect(registry).NotTo(BeNil())

			// Check that default query is registered
			query := registry.Get("all_metrics")
			Expect(query).NotTo(BeNil())
			Expect(query.Name).To(Equal("all_metrics"))
		})
	})
})

// mockReader is a simple io.Reader implementation for testing
type mockReader struct {
	data []byte
	pos  int
}

func (m *mockReader) Read(p []byte) (n int, err error) {
	if m.pos >= len(m.data) {
		return 0, io.EOF
	}
	n = copy(p, m.data[m.pos:])
	m.pos += n
	return n, nil
}

var _ = Describe("Error metrics recording", func() {
	var (
		ctx      context.Context
		registry *prometheus.Registry
	)

	BeforeEach(func() {
		ctx = context.Background()
		// Create fresh registry for each test
		registry = prometheus.NewRegistry()
		Expect(metrics.InitMetrics(registry)).To(Succeed())
	})

	It("should record error metric when pod scraping fails", func() {
		// Create service and pod but no HTTP server (scraping will fail)
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-pool-epp",
				Namespace: "test-ns",
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"inferencepool": "test-pool-epp",
				},
			},
		}

		readyPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "epp-pod-1",
				Namespace: "test-ns",
				Labels: map[string]string{
					"inferencepool": "test-pool-epp",
				},
			},
			Status: corev1.PodStatus{
				PodIP: "192.0.2.1", // Invalid/unreachable IP (TEST-NET-1)
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					},
				},
			},
		}

		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())

		client := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(service, readyPod).
			Build()

		config := PodScrapingSourceConfig{
			ServiceName:      "test-pool-epp",
			ServiceNamespace: "test-ns",
			MetricsPort:      9999, // Unreachable port
			ScrapeTimeout:    500 * time.Millisecond,
		}

		source, err := NewPodScrapingSource(ctx, client, config)
		Expect(err).NotTo(HaveOccurred())

		// The only pod fails, so Refresh reports it (it used to return nil
		// here, which hid the failure from the scale-from-zero fallback), and
		// the per-pod failure is still counted.
		_, err = source.Refresh(ctx, sourcepkg.RefreshSpec{})
		Expect(err).To(HaveOccurred())

		// Verify error metric was recorded
		count := testutil.GetErrorMetricValue(registry, constants.ComponentCollector, "Failed to scrape pod")
		Expect(count).To(BeNumerically(">", 0), "Error metric should be incremented when pod scraping fails")
	})
})
