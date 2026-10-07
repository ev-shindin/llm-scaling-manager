package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// The utilization-share optimizer on the shape it was built for: a P/D model
// served by LeaderWorkerSets (docs/proposals/utilization-share-optimizer.md,
// sections 5.6 and 6.5). One namespace quota of 7 GPUs, spent:
//
//   - model A, idle: a Deployment of four 1-GPU replicas;
//   - model B, loaded, disaggregated: a decode LWS of one group of two 1-GPU
//     pods, and a prefill LWS of one 1-GPU pod.
//
// B's decode grows by a replica of two pods, and no donor replica -- one pod
// each -- can host it. Two of A's replicas are taken as one donor set, one pod
// each (section 6.5). The decode LWS is raised only once both are released.
// This exercises what the Deployment spec does not: P/D roles, LWS pod shapes
// read from the templates, an LWS receiver's held count by group, and a
// donor set on a real ReplicaSet.
//
// It needs a node with 7 GPUs of one product, which the kind emulator gives
// (CLUSTER_GPUS, 16 per node by default); on a cluster without one it skips,
// saying so.
var _ = Describe("Utilization share optimizer on a P/D LeaderWorkerSet model", Label("full", "utilization-share"), Ordered, func() {
	const (
		quotaGPUs = 7
		msA       = "ushpd-a"
		msDecode  = "ushpd-d"
		msPrefill = "ushpd-p"
		modelA    = "ushpd-model-a"
		modelB    = "ushpd-model-b"
		loadJob   = "ushpd-b-load"
		window    = int32(30)
	)
	var (
		ns, configName           string
		key, product, gpuResName string
	)
	depA := msA + "-decode"
	lwsDecode, lwsPrefill := msDecode+"-decode", msPrefill+"-decode"
	soA, soD, soP := msA+"-so", msDecode+"-so", msPrefill+"-so"

	BeforeAll(func() {
		var ok bool
		key, product, gpuResName, ok = fixtures.DiscoverAccelerator(ctx, k8sClient)
		if !ok {
			Skip("no GPU product label on any schedulable node")
		}
		if n := fixtures.AllocatableGPUsForProduct(ctx, k8sClient, product); n < quotaGPUs {
			Skip(fmt.Sprintf("needs a node with %d %s GPUs, the largest has %d", quotaGPUs, product, n))
		}

		nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "ushpd-"},
		}, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
		ns = nsObj.Name
		By(fmt.Sprintf("Using namespace %s with a %d-GPU quota on %s", ns, quotaGPUs, product))
		DeferCleanup(func() {
			_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		})

		configName = scalingPolicyConfigMapName()
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.WVANamespace, configName, ns, product, quotaGPUs,
			fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Creating idle model A at four 1-GPU replicas, and P/D model B on two LeaderWorkerSets")
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msA, msA+"-pool", modelA, cfg.UseSimulator, 2,
			func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(int32(4)) })).To(Succeed())
		Expect(fixtures.EnsureModelServiceLWS(ctx, crClient, ns, msDecode, msDecode+"-pool", modelB, cfg.UseSimulator, 2, 2,
			fixtures.WithLWSRole(domain.RoleDecode), fixtures.WithLWSGPUs(key, product, gpuResName, 1))).To(Succeed())
		Expect(fixtures.EnsureModelServiceLWS(ctx, crClient, ns, msPrefill, msPrefill+"-pool", modelB, cfg.UseSimulator, 2, 1,
			fixtures.WithLWSRole(domain.RolePrefill), fixtures.WithLWSGPUs(key, product, gpuResName, 1))).To(Succeed())
		for _, ms := range []string{msA, msDecode, msPrefill} {
			Expect(fixtures.EnsureService(ctx, k8sClient, ns, ms, ms+"-decode", 8000)).To(Succeed())
			Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, ns, ms, ms+"-decode")).To(Succeed())
			monitor := ms + "-monitor"
			DeferCleanup(func() {
				_ = crClient.Delete(context.Background(), &promoperator.ServiceMonitor{
					ObjectMeta: metav1.ObjectMeta{Name: monitor, Namespace: cfg.MonitoringNS},
				})
			})
		}
		Eventually(func(g Gomega) {
			a, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depA, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(a.Status.ReadyReplicas).To(Equal(int32(4)))
			for _, name := range []string{lwsDecode, lwsPrefill} {
				var l lwsv1.LeaderWorkerSet
				g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &l)).To(Succeed())
				g.Expect(l.Status.ReadyReplicas).To(Equal(int32(1)), name)
			}
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		By("Registering all three with WVA; A's floor is 4 until the optimizer has started")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 4, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		for _, so := range []struct{ name, target string }{{soD, lwsDecode}, {soP, lwsPrefill}} {
			Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, so.name, so.target, so.target, 1, 3, cfg.MonitoringNS,
				fixtures.WithScaledObjectScaleTargetKind("LeaderWorkerSet"),
				fixtures.WithWVATriggerMetadata(modelB, "10.0"),
				fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		}
		DeferCleanup(func() {
			for _, so := range []string{soA, soD, soP} {
				_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, so)
			}
		})

		By("Loading B's decode well past one group's capacity")
		job := buildThroughputSustainedLoadJob(ns, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", msDecode),
			modelB, 8, int64(1800))
		_, err = k8sClient.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("grows B's decode by one group, funded by two of A's replicas as one donor set", func() {
		start := time.Now()
		Eventually(func(g Gomega) {
			g.Expect(controllerLogsSince(start.Add(-5 * time.Minute))).To(ContainSubstring("scope\": \"" + ns))
		}, 5*time.Minute, 10*time.Second).Should(Succeed(), "the optimizer never evaluated the namespace's group")

		By("Lowering A's floor to 1, so it has replicas to give")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 1, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())

		By("Waiting for two A pods to be marked as one donor set")
		var marked []corev1.Pod
		Eventually(func(g Gomega) {
			marked = markedSharePods(ns)
			g.Expect(marked).To(HaveLen(2))
		}, 12*time.Minute, 5*time.Second).Should(Succeed())
		sets := map[string]bool{}
		for _, p := range marked {
			Expect(p.Labels["app"]).To(Equal(depA), "only A has replicas to give")
			var m struct {
				SetID string `json:"setID"`
			}
			Expect(json.Unmarshal([]byte(p.Annotations["llm-d.ai/utilization-share-transfer"]), &m)).To(Succeed())
			Expect(m.SetID).NotTo(BeEmpty(), "pod %s is not marked as part of a set", p.Name)
			sets[m.SetID] = true
		}
		Expect(sets).To(HaveLen(1), "both donors must belong to one set")

		By("Checking the decode LWS is not raised until BOTH donor pods have gone")
		gone := func(name string) bool {
			p, err := k8sClient.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			return err != nil || p.DeletionTimestamp != nil
		}
		var raisedEarly bool
		Eventually(func(g Gomega) {
			var l lwsv1.LeaderWorkerSet
			g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: lwsDecode}, &l)).To(Succeed())
			bothGone := gone(marked[0].Name) && gone(marked[1].Name)
			if *l.Spec.Replicas >= 2 && !bothGone {
				raisedEarly = true
			}
			g.Expect(*l.Spec.Replicas).To(BeNumerically(">=", 2))
		}, 12*time.Minute, time.Second).Should(Succeed())
		Expect(raisedEarly).To(BeFalse(), "the decode LWS was raised while a donor pod still held its GPU")

		By("Waiting for the new decode group to be ready and the set's transfer to complete")
		Eventually(func(g Gomega) {
			var l lwsv1.LeaderWorkerSet
			g.Expect(crClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: lwsDecode}, &l)).To(Succeed())
			g.Expect(l.Status.ReadyReplicas).To(BeNumerically(">=", 2))
			logs := controllerLogsSince(start)
			g.Expect(logs).To(MatchRegexp(`transfer ended.*\bdone\b`))
			g.Expect(strings.Count(logs, "Utilization share: transfer started")).To(BeNumerically(">=", 2))
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
	})
})
