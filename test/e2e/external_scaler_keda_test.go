package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// External-scaler smoke: same deterministic operating point as the V2 suite
// (kv-cache-usage=0.3 with scaleUpThreshold=0.30 makes WVA's V2 optimizer decide
// desiredReplicas=2), but KEDA receives that decision over WVA's external-scaler
// gRPC service (internal/scaler) rather than by querying the wva_desired_replicas
// gauge in Prometheus. This exercises the external-scaler transport end-to-end:
// WVA (decide) → in-memory decision store → external scaler (gRPC) → KEDA → HPA →
// Deployment.
//
// --fake-metrics is simulator-only; gate the suite on cfg.UseSimulator.
const extScalerFakeMetricsJSON = `{"kv-cache-usage":0.3,"running-requests":1,"waiting-requests":0}`

// Saturation config knobs; the V1 fields are inert on the V2 path and carry safe
// defaults. scaleUpThreshold=0.30 drives desiredReplicas=2 at kv-cache-usage=0.3.
const (
	extScalerKvCacheThreshold     = 0.80
	extScalerQueueLengthThreshold = 50

	extScalerScaleUpThreshold  = 0.30
	extScalerScaleDownBoundary = 0.20
)

var _ = Describe("KEDA external scaler", Label("smoke", "full"), Ordered, func() {
	const (
		poolName              = "ext-scaler-pool"
		modelSvcName          = "ext-scaler-ms"
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"

		// scalerBaseName is the logical base for the annotated scaler; the KEDA
		// ScaledObject name is scalerBaseName+"-so".
		scalerBaseName = "ext-scaler"
	)

	var (
		modelID         string
		cmName          string
		cmNamespace     string
		cmKey           string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		variantName     string
		scalerAddress   string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"It uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey
		variantName = scalerBaseName + "-so"
		// WVA's external scaler runs on the controller and is exposed by the
		// external-scaler Service (config/base/manager/external-scaler-service.yaml),
		// which with the deploy namePrefix (wva-) resolves to:
		scalerAddress = "wva-external-scaler." + cfg.WVANamespace + ".svc.cluster.local:9090"

		By("Snapshotting existing saturation ConfigMap for restore in AfterAll")
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating the model service with --fake-metrics so WVA decides deterministically")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		Expect(fixtures.CreateModelServiceWithExtraArgs(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID,
			cfg.UseSimulator, cfg.MaxNumSeqs,
			[]string{"--fake-metrics", extScalerFakeMetricsJSON})).To(Succeed())
		Expect(fixtures.EnsureService(
			ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000,
		)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(
			ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment,
		)).To(Succeed())

		By("Waiting for the external-scaler smoke model deployment to be ready")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())

		By("Registering the deployment with WVA via an annotated ScaledObject using an EXTERNAL trigger")
		// The llm-d.ai/managed annotation drives WVA discovery + decision; the
		// external trigger makes KEDA fetch that decision from WVA's external-scaler
		// gRPC service instead of Prometheus. min=1, max=10.
		Expect(fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithExternalScalerTrigger(scalerAddress),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))).To(Succeed())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })

		By("Installing saturation config (scaleUpThreshold=0.30) so WVA decides desiredReplicas=2")
		cfgYAML := buildSaturationConfigYAMLWithThresholds(
			"saturation",
			extScalerKvCacheThreshold, extScalerQueueLengthThreshold,
			extScalerScaleUpThreshold, extScalerScaleDownBoundary,
		)
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey, cfgYAML)).To(Succeed())
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			propagation := metav1.DeletePropagationBackground
			if err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			}); err != nil && !errors.IsNotFound(err) {
				GinkgoWriter.Printf("Warning: failed to delete saturation configmap %s before restore: %v\n", cmName, err)
			}
			toCreate := saturationConfigMapForRecreate(cmOriginal)
			if _, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Create(ctx, toCreate, metav1.CreateOptions{}); err != nil {
				GinkgoWriter.Printf("Warning: failed to recreate saturation configmap %s: %v\n", cmName, err)
			}
		} else {
			_ = k8sClient.CoreV1().ConfigMaps(cmNamespace).Delete(ctx, cmName, metav1.DeleteOptions{})
		}

		By("Cleaning up external-scaler smoke resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// Proves KEDA reached WVA's external scaler: the KEDA-managed HPA only gets
	// CurrentMetrics populated once a scaler returns a value.
	It("has KEDA consume WVA's decision through the external scaler", func() {
		By("Verifying KEDA created an HPA with CurrentMetrics populated from the external scaler")
		// This spec used to assert only that Status.CurrentMetrics was not
		// empty, and a CPU-defaulted HPA reports exactly one entry whose every
		// field is nil -- so the check passed on an HPA carrying no external
		// metric at all. Observed in CI: this spec went green in 25 s against
		// an HPA with CurrentMetrics [one all-nil entry] and
		// ScalingActive=False FailedGetResourceMetric x41, and the next spec
		// then waited the full 600 s for a Deployment that could never move.
		// The spec whose whole subject is "KEDA consumes WVA's decision" has to
		// assert the EXTERNAL metric, which is what these two helpers do.
		Eventually(func(g Gomega) {
			reportIfHPAOnCPUDefault(cfg.LLMDNamespace, modelDecodeDeployment)
			expectKEDAExternalMetricWired(g, cfg.LLMDNamespace, modelDecodeDeployment)
			expectWVADesiredReplicasConsumed(g, cfg.LLMDNamespace, modelDecodeDeployment)
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})

	// Full external-scaler pipeline: WVA decides 2, delivered over gRPC to KEDA,
	// which drives the managed Deployment to 2 ready replicas.
	It("scales up via the external scaler when WVA decides more replicas", func() {
		By("Asserting KEDA actuates scale-up to >= 2 replicas from the external scaler's value")
		Eventually(func(g Gomega) {
			dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 2),
				"Deployment should scale up to >= 2 replicas: WVA decides 2, delivered via the external scaler")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).
			Should(Succeed())
	})
})
