package e2e

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// The utilization-share optimizer's node-aware donor sets
// (docs/proposals/utilization-share-optimizer.md, section 6.5), on GPU nodes
// with no free GPU left. One namespace quota of 6 GPUs, spent:
//
//   - model A, idle: a Deployment of four 1-GPU replicas;
//   - model B, loaded: a Deployment of one 2-GPU replica.
//
// Every other GPU on the product's nodes is taken by a pause pod outside WVA.
// B grows by one 2-GPU pod. No A pod holds it on its own, so the node-blind
// search funds nothing; only the node-aware one can, by planning two A pods on
// one node as one hole. The controller must say it planned them ("planned":
// true), raise B only after both are gone, and B's new pod must run -- where
// the two A pods were, since nowhere else has room.
var _ = Describe("Utilization share optimizer on full GPU nodes", Label("full", "utilization-share"), Ordered, func() {
	const (
		quotaGPUs = 6
		msA       = "ushn-a"
		msB       = "ushn-b"
		modelA    = "ushn-model-a"
		modelB    = "ushn-model-b"
		loadJob   = "ushn-b-load"
		window    = int32(30)
	)
	var (
		ns, fillerNS, configName string
		key, product, gpuResName string
	)
	soA, soB := msA+"-so", msB+"-so"
	depA, depB := msA+"-decode", msB+"-decode"

	BeforeAll(func() {
		var ok bool
		key, product, gpuResName, ok = fixtures.DiscoverAccelerator(ctx, k8sClient)
		if !ok {
			Skip("no GPU product label on any schedulable node")
		}
		// A's pods all go to the largest schedulable node of the product: the
		// spec needs two of them on one node, and that must not be left to the
		// scheduler.
		pinNode, n := fixtures.LargestNodeForProduct(ctx, k8sClient, product)
		if n < quotaGPUs {
			Skip(fmt.Sprintf("needs a node with %d %s GPUs, the largest has %d", quotaGPUs, product, n))
		}

		for _, prefix := range []string{"ushn-", "ushn-filler-"} {
			nsObj, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{GenerateName: prefix},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred())
			name := nsObj.Name
			DeferCleanup(func() {
				_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
			})
			if prefix == "ushn-" {
				ns = name
			} else {
				fillerNS = name
			}
		}
		By(fmt.Sprintf("Using namespace %s with a %d-GPU quota on %s", ns, quotaGPUs, product))

		configName = scalingPolicyConfigMapName()
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.WVANamespace, configName, ns, product, quotaGPUs,
			fixtures.WithOptimizer(map[string]any{"type": "utilizationShare"}))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Creating idle model A at four 1-GPU replicas, and model B at one 2-GPU replica")
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msA, msA+"-pool", modelA, cfg.UseSimulator, 2,
			func(d *appsv1.Deployment) {
				d.Spec.Replicas = ptr.To(int32(4))
				if d.Spec.Template.Spec.NodeSelector == nil {
					d.Spec.Template.Spec.NodeSelector = map[string]string{}
				}
				d.Spec.Template.Spec.NodeSelector[corev1.LabelHostname] = pinNode
			})).To(Succeed())
		Expect(fixtures.EnsureModelService(ctx, k8sClient, ns, msB, msB+"-pool", modelB, cfg.UseSimulator, 2,
			func(d *appsv1.Deployment) { d.Spec.Replicas = ptr.To(int32(1)) },
			fixtures.WithGPUs(gpuResName, 2))).To(Succeed())
		for _, ms := range []string{msA, msB} {
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
			b, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depB, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(b.Status.ReadyReplicas).To(Equal(int32(1)))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

		// The spec needs two of A's pods on one node; they are pinned to one.
		pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + depA})
		Expect(err).NotTo(HaveOccurred())
		perNode := map[string]int{}
		for _, p := range pods.Items {
			perNode[p.Spec.NodeName]++
		}
		shared := false
		for _, n := range perNode {
			shared = shared || n >= 2
		}
		Expect(shared).To(BeTrue(), "no node holds two of A's pods (%v), although they are pinned to %s", perNode, pinNode)

		By("Waiting for earlier specs' pods to finish terminating on the product's nodes")
		// The scheduler counts a terminating pod's GPUs as used, so a fill
		// taken while one remains leaves GPUs that come free later -- and
		// B's pod would then fit free GPUs, which is not this spec's case.
		Eventually(func(g Gomega) {
			n, err := fixtures.TerminatingPodsOnNodes(ctx, k8sClient, key, product)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(BeZero(), "%d pods still terminating", n)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		By("Taking every other free GPU on the product's nodes with pause pods outside WVA")
		// Fill until a pass finds nothing left to take.
		var fillers []string
		Eventually(func(g Gomega) {
			names, took, err := fixtures.FillGPUNodes(ctx, k8sClient, fillerNS, key, product, gpuResName)
			g.Expect(err).NotTo(HaveOccurred())
			fillers = append(fillers, names...)
			g.Expect(took).To(BeZero(), "took %d more GPUs; filling again", took)
		}, 3*time.Minute, 15*time.Second).Should(Succeed())
		Expect(fillers).NotTo(BeEmpty(), "the product's nodes had no free GPU to take: nothing makes this spec need the node search")
		Eventually(func(g Gomega) {
			pods, err := k8sClient.CoreV1().Pods(fillerNS).List(ctx, metav1.ListOptions{LabelSelector: "app=gpu-filler"})
			g.Expect(err).NotTo(HaveOccurred())
			for _, p := range pods.Items {
				g.Expect(p.Status.Phase).To(Equal(corev1.PodRunning), p.Name)
			}
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("Registering both with WVA; A's floor is 4 until the optimizer has started")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 4, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soB, depB, depB, 1, 3, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelB, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())
		DeferCleanup(func() {
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, soA)
			_ = fixtures.DeleteScaledObject(context.Background(), crClient, ns, soB)
		})

		By("Loading model B well past one replica's capacity")
		job := buildThroughputSustainedLoadJob(ns, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", msB),
			modelB, 8, int64(1800))
		_, err = k8sClient.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("funds B's 2-GPU pod from two 1-GPU A pods it planned on one full node", func() {
		start := time.Now()
		Eventually(func(g Gomega) {
			g.Expect(controllerLogsSince(start.Add(-5 * time.Minute))).To(ContainSubstring("scope\": \"" + ns))
		}, 5*time.Minute, 10*time.Second).Should(Succeed(), "the optimizer never evaluated the namespace's group")

		By("Lowering A's floor to 1, so it has replicas to give")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, ns, soA, depA, depA, 1, 4, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())

		By("Waiting for a planned donor set of two A pods funding B")
		started := regexp.MustCompile(`"scope": "` + ns + `".*"set": "([^"]+)", "donor": "[^"]*/` + modelA +
			`/both", "receiver": "([^"]*)".*"planned": true`)
		var setID string
		Eventually(func(g Gomega) {
			members, receivers := map[string]int{}, map[string]string{}
			for _, line := range strings.Split(controllerLogsSince(start), "\n") {
				if !strings.Contains(line, "transfer started") {
					continue
				}
				if m := started.FindStringSubmatch(line); m != nil {
					members[m[1]]++
					if m[2] != "" {
						receivers[m[1]] = m[2]
					}
				}
			}
			for id, n := range members {
				if n == 2 && strings.HasSuffix(receivers[id], "/"+modelB+"/both") {
					setID = id
				}
			}
			g.Expect(setID).NotTo(BeEmpty(), "no planned set of two A pods funding B has started: %v", members)
		}, 12*time.Minute, 5*time.Second).Should(Succeed())
		GinkgoWriter.Printf("planned donor set: %s\n", setID)

		By("Waiting for B's second pod to run and the set's transfer to complete")
		Eventually(func(g Gomega) {
			b, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, depB, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(b.Status.ReadyReplicas).To(BeNumerically(">=", 2))
			logs := controllerLogsSince(start)
			expectSetCompleted(g, logs, setID)
			g.Expect(logs).NotTo(MatchRegexp(`"scope": "`+ns+`".*"outcome": "wrong-pod"`), "a donor lost a pod it was not asked for")
		}, 8*time.Minute, 10*time.Second).Should(Succeed())
	})
})
