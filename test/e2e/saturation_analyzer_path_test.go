package e2e

import (
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// NOTE: V2 is the sole analysis path — the V1 analyzer was removed, so
// analyzerName no longer selects between engines. The arcs that leave
// analyzerName unset now assert that an unset value keeps processing on V2
// rather than stalling the model. This suite remains self-guarded: it snapshots
// the base ConfigMap and writes its own explicit config for each arc, so a
// change to the shipped default does not affect it.

// Saturation calibration via the simulator's --fake-metrics flag.
//
// kv-cache-usage=0.3 and waiting-requests=2 are chosen so both threshold arcs
// are deterministically exercisable by config alone — no load required:
//
// V2 does not compare the usage against the threshold as a boolean the way V1
// did; kvCacheThreshold is a utilization *target* that sizes per-replica
// capacity, and the engine scales on demand vs that capacity:
//
//   - Scale-up path (kvCache=0.80, queue=1): the tighter target sizes capacity
//     so demand ≈ supply → utilization ≈ 1.0 > scaleUpThreshold 0.85 → scale-up.
//   - No-scale path (kvCache=1.00, queue=100): the looser target sizes ~25% more
//     capacity per replica → utilization drops below 0.85 → no scale-up.
//
// Both arcs keep the target above the faked 0.30 usage. Setting it below would
// put utilization over 1.0 from a standing start, which pins the no-scale arc to
// a scale-up and makes the pair meaningless — the arcs must differ by where the
// ceiling sits relative to the same occupancy, not by saturating both.
//
// --fake-metrics replaces simulator runtime emission entirely; service traffic
// has no effect on the values the analyzer reads.
const fakeMetricsJSON = `{"kv-cache-usage":0.3,"waiting-requests":2,"running-requests":1}`

const (
	// No limiters, matching the shipped ConfigMap: this entry REPLACES the
	// cluster's "default" saturation entry, so it must carry the same limiter
	// policy. A suite that needs one declares it for itself — see the
	// scale-from-zero capacity suite.
	saturationConfigTemplate = `
model_id: ""
namespace: ""
kvCacheThreshold: %.2f
queueLengthThreshold: %d
scaleUpThreshold: %.2f
scaleDownBoundary: %.2f
analyzerName: %q
`

	// Scale-up arc. kvCacheThreshold is V2's KV utilization *ceiling*: capacity is
	// the KV budget scaled by it, so a tighter ceiling means less capacity per
	// replica and higher utilization for the same occupancy. Utilization is
	// KvCacheUsage/kvCacheThreshold, so 0.30 occupancy against a 0.80 ceiling is
	// 0.375 — which, with the queue term the fixture's waiting requests add on
	// top, clears scaleUpThreshold.
	saturationKVCacheThreshold     = 0.80
	saturationQueueLengthThreshold = 1
	saturationScaleUpThreshold     = 0.85
	saturationScaleDownBoundary    = 0.70

	// No-scale arc. The looser 1.00 ceiling sizes 25% more capacity per replica
	// than the scale-up arc (utilization 0.30 rather than 0.375) and the raised
	// queue threshold removes the queue term, dropping the total below
	// scaleUpThreshold so the engine sees no shortfall.
	saturationNoScaleKVCacheThreshold     = 1.00
	saturationNoScaleQueueLengthThreshold = 100
)

// buildSaturationConfigYAML builds a valid saturation config entry at the default
// thresholds. The analyzer name is fixed: every caller wants "saturation", and the
// alternative analyzer paths this used to be parameterised for are gone. Callers
// that still need to vary it (including the empty name, which exercises the
// default) go through buildSaturationConfigYAMLWithThresholds.
func buildSaturationConfigYAML() string {
	return fmt.Sprintf(saturationConfigTemplate, 0.80, 1, 0.85, 0.70, "saturation")
}

// buildSaturationConfigYAMLWithThresholds builds a valid saturation config entry with explicit thresholds.
// buildSaturationConfigYAMLWithLimiter is buildSaturationConfigYAMLWithThresholds
// plus a declared limiter, for the one suite that asserts limiting actually
// happens. Everything else must NOT declare one — see the template's comment.
func buildSaturationConfigYAMLWithLimiter(
	analyzerName string,
	kvCacheThreshold float64, queueLengthThreshold int,
	scaleUpThreshold, scaleDownBoundary float64,
	limiterType string,
) string {
	return buildSaturationConfigYAMLWithThresholds(
		analyzerName, kvCacheThreshold, queueLengthThreshold, scaleUpThreshold, scaleDownBoundary,
	) + "limiters:\n  - type: " + limiterType + "\n"
}

func buildSaturationConfigYAMLWithThresholds(analyzerName string, kvCacheThreshold float64, queueLengthThreshold int, scaleUpThreshold float64, scaleDownBoundary float64) string {
	return fmt.Sprintf(
		saturationConfigTemplate,
		kvCacheThreshold,
		queueLengthThreshold,
		scaleUpThreshold,
		scaleDownBoundary,
		analyzerName,
	)
}

// buildSaturationConfigYAMLWithModel is buildSaturationConfigYAMLWithThresholds
// plus the identity that makes the entry a PER-MODEL OVERRIDE. Identity lives in
// the body, not the key: a ConfigMap data key admits only [-._a-zA-Z0-9], so a
// namespaced model ID cannot appear in one.
// kvCacheThreshold is kept even though every caller passes 0.80 today. That is a
// coincidence, not a constant: the tier suite passes tierKvCacheThreshold, which
// merely happens to equal the default. Collapsing the parameter would silently pin
// that suite to 0.80 the moment the constant changed, and the suite exists to prove
// a tier's own thresholds are applied.
//
//nolint:unparam // deliberate; see above
func buildSaturationConfigYAMLWithModel(
	kvCacheThreshold float64, queueLengthThreshold int,
	scaleUpThreshold, scaleDownBoundary float64,
	modelID, namespace string,
) string {
	entry := buildSaturationConfigYAMLWithThresholds(
		"saturation", kvCacheThreshold, queueLengthThreshold, scaleUpThreshold, scaleDownBoundary,
	)
	entry = strings.Replace(entry, `model_id: ""`, fmt.Sprintf("model_id: %q", modelID), 1)
	return strings.Replace(entry, `namespace: ""`, fmt.Sprintf("namespace: %q", namespace), 1)
}

// scalingPolicyConfigMapName resolves the active saturation ConfigMap name from controller runtime env.
func scalingPolicyConfigMapName() string {
	// Match the controller's runtime config map name; discover by label first
	// since the deployment name can vary across overlays.
	deps, err := k8sClient.AppsV1().Deployments(cfg.WVANamespace).List(ctx, metav1.ListOptions{
		LabelSelector: "control-plane=controller-manager",
	})
	if err != nil || len(deps.Items) == 0 {
		return config.ScalingPolicyConfigMapName()
	}
	return saturationConfigMapNameFromDeployment(&deps.Items[0])
}

// saturationConfigMapNameFromDeployment extracts SATURATION_CONFIG_MAP_NAME from manager container env.
func saturationConfigMapNameFromDeployment(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name != "manager" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "SATURATION_CONFIG_MAP_NAME" && e.Value != "" {
				return e.Value
			}
		}
	}
	return config.ScalingPolicyConfigMapName()
}

// expectAnalyzerPathLog is a Ginkgo helper: it Eventually-waits until WVA
// controller-manager logs show the model being processed on the V2 path. V2 is
// the sole analysis path since the V1 analyzer was removed, so there is no mode
// to select — the helper asserts the model is being processed at all.
// It uses testutils.PodLogsLabelSelectorContain for log collection.
func expectAnalyzerPathLog(modelID string) {
	GinkgoHelper()
	const controllerManagerLabel = "control-plane=controller-manager"
	const pattern = "Processing model (V2)"
	Eventually(func(g Gomega) {
		ok, logs, logErr := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace, controllerManagerLabel, pattern, 120)
		g.Expect(logErr).NotTo(HaveOccurred())
		g.Expect(ok && strings.Contains(logs, modelID)).To(BeTrue())
	}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
}

// This suite covers the saturation analyzer's decision reaching a real workload
// through the KEDA **external scaler** — KEDA fetches the decision from WVA's
// gRPC service rather than reading the wva_desired_replicas gauge out of
// Prometheus. The Prometheus transport is covered by smoke_keda_test.go and
// saturation_test.go; this suite is the saturation-decision half of the
// external-scaler path, so both transports stay exercised.
var _ = Describe("Saturation-driven scaling through the KEDA external scaler", Label("full"), Ordered, func() {
	const (
		poolName     = "saturation-path-pool"
		modelSvcName = "saturation-path-ms"
		// modelDecodeDeployment is the Deployment name fixtures.CreateModelService creates
		// (name + "-decode"), matching llm-d model-service decode pods / labels.
		modelDecodeDeployment = modelSvcName + "-decode"
		serviceName           = modelSvcName + "-service"
		smName                = modelSvcName + "-monitor"
		// scalerBaseName is the annotated scaler's logical base; the KEDA ScaledObject
		// object name is base+"-so". The decode pods must carry
		// llm-d.ai/variant=<scaler object name> for metric attribution, so variantName
		// is derived from it.
		scalerBaseName = "saturation-path"
		soObjectName   = scalerBaseName + "-so"
	)

	var (
		modelID         string
		cmName          string
		cmOriginal      *corev1.ConfigMap
		cmExistedBefore bool
		cmKey           string
		cmNamespace     string
		// variantName is the variant_name — the scaler's object name — stamped as the
		// decode pods' llm-d.ai/variant label so the collector attributes their
		// metrics to the variant.
		variantName string
		// scalerAddress is WVA's external-scaler gRPC Service, which KEDA's external
		// trigger dials for this suite's decisions.
		scalerAddress string
	)

	BeforeAll(func() {
		if !cfg.UseSimulator {
			Skip("This suite needs the simulator runtime: set USE_SIMULATOR=true. " +
				"The suite uses llm-d-inference-sim's --fake-metrics flag, which real vLLM rejects.")
		}

		variantName = soObjectName
		// Matches config/base/manager/external-scaler-service.yaml under the deploy
		// namePrefix (wva-).
		scalerAddress = "wva-external-scaler." + cfg.WVANamespace + ".svc.cluster.local:9090"

		modelID = cfg.ModelID
		cmName = scalingPolicyConfigMapName()
		// Use global saturation config for deterministic engine-path selection.
		// Namespace-local ConfigMap watch is opt-in/tracked and can race in e2e.
		cmNamespace = cfg.WVANamespace
		cmKey = defaultConfigKey

		// Snapshot existing saturation config so the test can restore it.
		cm, err := k8sClient.CoreV1().ConfigMaps(cmNamespace).Get(ctx, cmName, metav1.GetOptions{})
		if err == nil {
			cmExistedBefore = true
			cmOriginal = cm.DeepCopy()
		} else if !errors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred(), "failed reading existing saturation configmap")
		}

		By("Creating model service + service + ServiceMonitor for saturation path test")
		_ = fixtures.DeleteModelService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName)
		err = fixtures.CreateModelServiceWithExtraArgs(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, poolName, modelID,
			cfg.UseSimulator, cfg.MaxNumSeqs, []string{"--fake-metrics", fakeMetricsJSON})

		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureService(ctx, k8sClient, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment, 8000)
		Expect(err).NotTo(HaveOccurred())
		err = fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, cfg.LLMDNamespace, modelSvcName, modelDecodeDeployment)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for model service to be ready")
		Eventually(func(g Gomega) {
			dep, depErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(depErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">=", 1))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering the saturation-path deployment with WVA via an annotated ScaledObject using an EXTERNAL trigger")
		// The llm-d.ai/managed annotation drives WVA discovery and the saturation
		// decision; the external trigger makes KEDA fetch that decision from WVA's
		// external-scaler gRPC service rather than from Prometheus, so these specs
		// exercise the analyzer and the external-scaler transport together.
		// The ScaledObject's variantName matches the model service's variantName so the
		// decode pods' llm-d.ai/variant label lines up for metric attribution.
		// The 30 s scale-down stabilization window overrides the HPA default (300 s) so
		// the "does not scale up" It can wait for minReplicas within EventuallyLongSec.
		err = fixtures.EnsureScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName, modelDecodeDeployment, variantName, 1, 10, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelID, "30.0"),
			fixtures.WithExternalScalerTrigger(scalerAddress),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(30))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = fixtures.DeleteScaledObject(ctx, crClient, cfg.LLMDNamespace, scalerBaseName) })
	})

	AfterAll(func() {
		By("Restoring saturation ConfigMap state")
		if cmExistedBefore && cmOriginal != nil {
			// Replace the object in two steps (delete + create) instead of updating in place.
			// That avoids resourceVersion conflict retries; a brief gap without the ConfigMap
			// during suite teardown is acceptable for e2e.
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

		By("Cleaning up saturation analyzer path resources")
		_ = crClient.Delete(ctx, &promoperator.ServiceMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: smName, Namespace: cfg.MonitoringNS},
		})
		_ = k8sClient.CoreV1().Services(cfg.LLMDNamespace).Delete(ctx, serviceName, metav1.DeleteOptions{})
		_ = k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Delete(ctx, modelDecodeDeployment, metav1.DeleteOptions{})
	})

	// Two specs were deleted here rather than adapted. They asserted that
	// analyzerName=saturation selected "the V2 path" and that an unset
	// analyzerName selected "the V1 path", by grepping controller logs for a
	// "Processing model (Vn)" marker. V1 is gone and V2 is the sole analysis
	// path, so there is no selection left to observe — after the removal both
	// specs could only assert that the model was processed at all, which every
	// spec below already requires in order to reach an actuation assertion.

	It("delivers the saturation decision to KEDA through the external scaler", func() {
		// WVA does not write VA .status; the decision leaves the controller either
		// as the wva_desired_replicas gauge or, as here, over the external-scaler
		// gRPC API. KEDA populates the managed HPA's CurrentMetrics only after a
		// scaler returns a value, so a populated CurrentMetrics on a ScaledObject
		// whose only trigger is the external one proves KEDA reached WVA's gRPC
		// service and got the saturation analyzer's decision back.
		By("Writing the model's saturation config")
		Expect(upsertSaturationConfigEntry(ctx, cmNamespace, cmName, cmKey,
			buildSaturationConfigYAML())).To(Succeed())

		By("Verifying the KEDA-managed HPA has CurrentMetrics populated from the external scaler")
		Eventually(func(g Gomega) {
			hpaList, err := k8sClient.AutoscalingV2().HorizontalPodAutoscalers(cfg.LLMDNamespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			var kedaHPA *autoscalingv2.HorizontalPodAutoscaler
			for i := range hpaList.Items {
				if hpaList.Items[i].Spec.ScaleTargetRef.Name == modelDecodeDeployment {
					kedaHPA = &hpaList.Items[i]
					break
				}
			}
			g.Expect(kedaHPA).NotTo(BeNil(), "KEDA should have created an HPA for the saturation-path deployment")
			g.Expect(kedaHPA.Status.CurrentMetrics).NotTo(BeEmpty(),
				"KEDA HPA should have CurrentMetrics populated from WVA's external scaler")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("holds at minReplicas through the external scaler for below-threshold traffic", func() {
		By("Configuring conservative saturation thresholds to avoid scale-up")
		// Set thresholds before capturing the baseline so WVA has time to reconcile
		// while we wait for KEDA to settle: the prior It may have left a scale-up
		// decision that KEDA is still serving from the external scaler. We must wait
		// for WVA to re-evaluate and KEDA to poll the new value before the stability
		// window starts — otherwise the HPA would fire a scale-up mid-assertion.
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationNoScaleKVCacheThreshold,
				saturationNoScaleQueueLengthThreshold,
				saturationScaleUpThreshold,
				saturationScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the pipeline to converge to a sustained minReplicas (drain any in-flight scale-up)")
		// A single Spec.Replicas <= 1 reading is NOT proof of convergence: the deployment
		// starts at minReplicas, so that check passes on the pre-existing state while a
		// scale-up decision left in flight by the prior It is still working through
		// WVA (≤15 s reconcile) → external scaler → KEDA (5 s poll) → HPA. That stale
		// decision actuates a scale-up mid-assertion unless it has fully drained.
		//
		// Require the deployment to HOLD at <=1 continuously for stableWindow before
		// trusting it: any bump resets the stability clock, and stableWindow outlasts the
		// full pipeline latency (reconcile + poll + the 30 s scale-down stabilization set
		// on this ScaledObject) so the old recommendation is guaranteed drained.
		const stableWindow = 45 * time.Second
		var stableSince *time.Time
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			now := time.Now()
			if *dep.Spec.Replicas > 1 {
				stableSince = nil // reset the stability clock on any in-flight scale-up
				GinkgoWriter.Printf("  Convergence (%s): replicas=%d (>1) — resetting stability clock\n", modelDecodeDeployment, *dep.Spec.Replicas)
				g.Expect(*dep.Spec.Replicas).To(BeNumerically("<=", int32(1)),
					"waiting for the in-flight scale-up recommendation to drain")
				return
			}
			if stableSince == nil {
				stableSince = &now
			}
			held := now.Sub(*stableSince)
			GinkgoWriter.Printf("  Convergence (%s): replicas<=1 held for %s (need %s)\n", modelDecodeDeployment, held.Round(time.Second), stableWindow)
			g.Expect(held).To(BeNumerically(">=", stableWindow),
				"waiting for minReplicas to hold long enough to confirm the pipeline converged")
		}, time.Duration(cfg.EventuallyExtendedSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Capturing baseline target deployment replicas before steady-state assertion")
		var baseline int32
		dep, err := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(dep.Spec.Replicas).NotTo(BeNil())
		baseline = *dep.Spec.Replicas
		GinkgoWriter.Printf("  Negative-path baseline (%s): replicas=%d\n", modelDecodeDeployment, baseline)

		By("Verifying the target deployment does not scale above baseline")
		Consistently(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			current := int32(0)
			if dep.Spec.Replicas != nil {
				current = *dep.Spec.Replicas
			}
			GinkgoWriter.Printf("  Negative-path progress (%s): replicas=%d baseline=%d\n", modelDecodeDeployment, current, baseline)
			g.Expect(current).To(BeNumerically("<=", baseline),
				"bounded below-threshold traffic should not scale the target deployment above baseline")
		}, time.Duration(cfg.EventuallyMediumSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

	It("scales up through the external scaler once saturation crosses the threshold", func() {
		var baseline int32

		By("Capturing baseline target deployment replicas before scale-up trigger")
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Spec.Replicas).NotTo(BeNil())
			baseline = *dep.Spec.Replicas
			GinkgoWriter.Printf("  Scale-up baseline (%s): replicas=%d\n", modelDecodeDeployment, baseline)
		}, time.Duration(cfg.EventuallyLongSec)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Configuring aggressive saturation thresholds and unsetting analyzerName")
		err := upsertSaturationConfigEntry(
			ctx,
			cmNamespace,
			cmName,
			cmKey,
			buildSaturationConfigYAMLWithThresholds(
				"",
				saturationKVCacheThreshold,
				saturationQueueLengthThreshold,
				saturationScaleUpThreshold,
				saturationScaleDownBoundary,
			),
		)
		Expect(err).NotTo(HaveOccurred())

		By("Asserting KEDA actuates scale-up above baseline")
		// A 0.80 KV target against faked kv=0.30 sizes per-replica capacity so that
		// demand ≈ supply — utilization ≈ 1.0, past scaleUpThreshold=0.85 — which
		// deterministically drives a scale-up. KEDA pulls that decision from WVA's
		// external scaler and drives the Deployment above its baseline. Assert the
		// observable Deployment replica count — the ground truth for the whole
		// analyzer → external-scaler → KEDA → HPA chain — rather than the HPA
		// CurrentMetrics surface, which only proves the decision was fetched.
		Eventually(func(g Gomega) {
			dep, getErr := k8sClient.AppsV1().Deployments(cfg.LLMDNamespace).Get(ctx, modelDecodeDeployment, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(dep.Status.ReadyReplicas).To(BeNumerically(">", baseline),
				"above-threshold traffic should scale the target Deployment above baseline")
		}, time.Duration(cfg.ScaleUpTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
	})

})

// upsertSaturationConfigEntry creates or updates a saturation ConfigMap data entry.
func upsertSaturationConfigEntry(ctx context.Context, cmNamespace, cmName, key, value string) error {
	cmClient := k8sClient.CoreV1().ConfigMaps(cmNamespace)
	cm, err := cmClient.Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			newCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      cmName,
					Namespace: cmNamespace,
				},
				Data: map[string]string{key: value},
			}
			_, createErr := cmClient.Create(ctx, newCM, metav1.CreateOptions{})
			return createErr
		}
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[key] = value
	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// deleteSaturationConfigEntry removes one entry from the scaling policy ConfigMap,
// leaving the rest intact. A missing ConfigMap or a missing key is success: the
// caller wants the entry gone, and it is.
func deleteSaturationConfigEntry(ctx context.Context, cmNamespace, cmName, key string) error {
	cmClient := k8sClient.CoreV1().ConfigMaps(cmNamespace)
	cm, err := cmClient.Get(ctx, cmName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if _, ok := cm.Data[key]; !ok {
		return nil
	}
	delete(cm.Data, key)
	_, err = cmClient.Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// saturationConfigMapForRecreate returns a copy of orig suitable for Create after Delete,
// with apiserver-owned fields cleared so admission succeeds.
func saturationConfigMapForRecreate(orig *corev1.ConfigMap) *corev1.ConfigMap {
	cm := orig.DeepCopy()
	cm.ResourceVersion = ""
	cm.UID = ""
	cm.Generation = 0
	cm.CreationTimestamp = metav1.Time{}
	cm.DeletionTimestamp = nil
	cm.DeletionGracePeriodSeconds = nil
	cm.ManagedFields = nil
	cm.Finalizers = nil
	return cm
}
