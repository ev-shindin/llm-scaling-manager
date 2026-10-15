package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	promoperator "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// Cohort borrowing APPLIED, against a live Kueue controller: the scaling
// manager orders a managed model's replicas into capacity its ClusterQueue
// borrows from the cohort, and gives it back when a lender needs it.
//
// Two ClusterQueues of 2 GPUs share a flat cohort. Queue a (reclaimWithinCohort
// Any) admits model A in namespace a, one GPU per replica, on the GPU product
// the suite pins every workload to, so its pods really schedule. Queue b starts
// as a lender that cannot reclaim (Never); its workload is a Deployment of
// 1-GPU pause pods. A is loaded far past what four replicas serve and its
// ScaledObject allows six, so demand is never what stops it.
//
// The policy declares the physical limiter and a namespace quota bounded by
// Kueue, with countBorrowable left unset (on by default) and short borrowing
// timings. In order:
//
//  1. A GPU pod in namespace a that Kueue does not manage withholds borrowing
//     (B1, not-kueue-managed): A holds at queue a's nominal 2.
//  2. That pod goes: A grows to 4 -- 2 borrowed, every pod placed -- and no
//     further, because the cohort has no more.
//  3. Queue b's workload waits (Never): after the debounce the scaling manager
//     releases exactly one replica of A, and Kueue admits b's pod into it.
//  4. Queue b may reclaim (Any) and asks for its second GPU: Kueue preempts A's
//     borrowed pod, the scaling manager lowers A's target so the gated
//     replacement goes, and A borrows nothing during the cooldown even after
//     b's GPUs are free again -- then grows back once it ends.
//
// Every step logs measured timings (MEASURE lines), for the borrowCooldown and
// placementTimeout defaults.
//
// Needs Kueue's controller and an emulated GPU product with at least 6 free
// GPUs. On kind, `make test-e2e-full-with-setup` installs Kueue
// (KUEUE_CONTROLLER=true, the default, via deploy/ci-pr-checks/install-kueue.sh)
// and its absence FAILS the spec. On any other cluster the spec skips, saying
// why, where Kueue or the GPUs are not there.
var _ = Describe("Kueue cohort borrowing - applied against a live Kueue", Label("full"), Label("kueue-borrow"), Ordered, Serial, func() {
	const (
		cohortName = "e2e-kueue-borrow"
		flavorName = "e2e-kueue-borrow"
		queueA     = "e2e-kueue-borrow-a"
		queueB     = "e2e-kueue-borrow-b"
		nsA        = "e2e-kueue-borrow-a"
		nsB        = "e2e-kueue-borrow-b"
		msA        = "kb-model-a"
		modelA     = "kb-model-a"
		soA        = msA + "-so"
		depA       = msA + "-decode"
		loadJob    = "kb-model-a-load"
		lender     = "kb-lender-b"
		outsider   = "kb-not-kueue-managed"
		nominal    = 2 // each ClusterQueue's nominal GPUs
		reach      = 2 * nominal
		maxA       = 6 // the ScaledObject allows more than the cohort has
		window     = int32(30)

		refresh          = "5s"
		placementTimeout = 30 * time.Second
		borrowCooldown   = 4 * time.Minute
		lenderDebounce   = 10 * time.Second

		// Kueue settles in under a second on an idle machine; a CI runner
		// running the rest of the suite is not idle.
		kueueSettle = 2 * time.Minute
		// A replica decision: a few 15s engine cycles, the KEDA window, a
		// simulator pod starting.
		scaleSettle = 6 * time.Minute
		poll        = time.Second
	)

	var (
		started             time.Time
		key, product, gpuRN string
		cohort              fixtures.KueueCohort
	)

	queueStatus := func(g Gomega, cq string) fixtures.KueueQueueStatus {
		st, err := fixtures.ReadKueueQueueStatus(ctx, crClient, cq, flavorName, gpuRN)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(st.HasStatus).To(BeTrue(), "ClusterQueue %s has no reservation status for %s; is Kueue running?", cq, gpuRN)
		return st
	}

	deployment := func(g Gomega, ns, name string) *appsv1.Deployment {
		d, err := k8sClient.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		g.Expect(err).NotTo(HaveOccurred())
		return d
	}

	podsOf := func(g Gomega, ns, app string) []corev1.Pod {
		pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
		g.Expect(err).NotTo(HaveOccurred())
		return pods.Items
	}

	// gatedPods are A's live pods held at Kueue's admission gate.
	gatedPods := func(g Gomega) []string {
		var gated []string
		for _, p := range podsOf(g, nsA, depA) {
			if p.DeletionTimestamp == nil && slices.Contains(gateNames(p), fixtures.KueueAdmissionGate) {
				gated = append(gated, p.Name)
			}
		}
		return gated
	}

	// expectAllPlaced: A has want live pods, every one Kueue-managed, ungated,
	// on a node, Ready.
	expectAllPlaced := func(g Gomega, want int) {
		var live []corev1.Pod
		for _, p := range podsOf(g, nsA, depA) {
			if p.DeletionTimestamp == nil {
				live = append(live, p)
			}
		}
		g.Expect(live).To(HaveLen(want), "live pods of A: %v", podNames(live))
		for _, p := range live {
			g.Expect(p.Labels).To(HaveKeyWithValue(fixtures.KueueManagedLabel, "true"), "pod %s", p.Name)
			g.Expect(gateNames(p)).To(BeEmpty(), "pod %s", p.Name)
			g.Expect(p.Spec.NodeName).NotTo(BeEmpty(), "pod %s is not placed", p.Name)
			g.Expect(podReady(p)).To(BeTrue(), "pod %s is not Ready", p.Name)
		}
	}

	// borrowRowA is namespace a's row in the newest borrowing line logged
	// since the spec started.
	borrowRowA := func(g Gomega) borrowRow {
		view, found, err := latestBorrowView(controllerLogsSince(started))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "no %q line yet; countBorrowable is left unset, which must mean on", borrowLogMessage)
		var rows []borrowRow
		for _, r := range view[nsA] {
			if r.Flavor == flavorName && r.Resource == gpuRN {
				rows = append(rows, r)
			}
		}
		g.Expect(rows).To(HaveLen(1), "namespace %s rows: %+v", nsA, view[nsA])
		return rows[0]
	}

	measure := func(name string, d time.Duration) {
		GinkgoWriter.Printf("MEASURE %s: %s\n", name, d.Round(100*time.Millisecond))
		AddReportEntry("MEASURE "+name, d.Round(100*time.Millisecond).String())
	}

	BeforeAll(func() {
		started = time.Now()

		if !fixtures.KueueInstalled(crClient) {
			if cfg.Environment == envKindEmulator {
				Fail("the Kueue CRDs are not installed on this kind cluster; test-e2e-full-with-setup installs " +
					"Kueue via deploy/ci-pr-checks/install-kueue.sh (KUEUE_CONTROLLER=true)")
			}
			Skip("Kueue is not installed on this cluster; the borrowing spec needs it and does not install it outside kind")
		}
		running, what, err := fixtures.KueueControllerRunning(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		if !running {
			if cfg.Environment == envKindEmulator {
				Fail("the Kueue CRDs are served but no Kueue controller is running (" + what + "); " +
					"install-kueue-crds.sh installs only the CRDs -- run test-e2e-full-with-setup with " +
					"KUEUE_CONTROLLER=true (the default) or make install-kueue-e2e")
			}
			Skip("no Kueue controller is running on this cluster (" + what + "); the borrowing spec needs one")
		}
		GinkgoWriter.Printf("Kueue controller: %s\n", what)

		var ok bool
		key, product, gpuRN, ok = fixtures.DiscoverAccelerator(ctx, k8sClient)
		if !ok {
			if cfg.Environment == envKindEmulator {
				Fail("no GPU product label on any schedulable kind node; deploy/kind-emulator/setup.sh labels them")
			}
			Skip("no GPU product label on any schedulable node")
		}
		By(fmt.Sprintf("Using %s=%s (%s)", key, product, gpuRN))

		By("Creating the two namespaces, labelled for Kueue's webhooks")
		for _, ns := range []string{nsA, nsB} {
			createKueueNamespace(ns)
			name := ns
			DeferCleanup(func() {
				_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
			})
		}

		By(fmt.Sprintf("Checking the product's nodes have %d GPUs free", reach+2))
		Eventually(func(g Gomega) {
			n, err := fixtures.TerminatingPodsOnNodes(ctx, k8sClient, key, product)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(n).To(BeZero(), "%d pods still terminating on the product's nodes", n)
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
		free := freeGPUsOfProduct(key, product, gpuRN)
		if free < reach+2 {
			msg := fmt.Sprintf("the %s nodes have %d free GPUs; the spec needs %d", product, free, reach+2)
			if cfg.Environment == envKindEmulator {
				Fail(msg)
			}
			Skip(msg)
		}

		By("Creating a flat cohort: queue a reclaims, queue b lends and cannot reclaim")
		cohort = fixtures.KueueCohort{
			Cohort:       cohortName,
			Flavor:       flavorName,
			ProductLabel: key,
			Product:      product,
			Resource:     gpuRN,
			Queues: []fixtures.KueueCohortQueue{
				{Name: queueA, Namespace: nsA, GPUs: nominal, Reclaim: fixtures.KueueReclaimAny},
				{Name: queueB, Namespace: nsB, GPUs: nominal, Reclaim: fixtures.KueueReclaimNever},
			},
		}
		Expect(fixtures.CreateKueueCohort(ctx, crClient, cohort)).To(Succeed())
		DeferCleanup(func() {
			// The ScaledObject first, so KEDA stops scaling A up again; then
			// every workload, because Kueue holds a ClusterQueue while anything
			// reserves in it. The waits are best effort: an Expect here aborted
			// the cleanup before the cohort was deleted, and leaked it into the
			// next run. The cohort is always deleted (finalizers forced as a
			// last resort), and only then are the errors reported.
			bg := context.Background()
			_ = fixtures.DeleteScaledObject(bg, crClient, nsA, soA)
			_ = k8sClient.BatchV1().Jobs(nsA).Delete(bg, loadJob, metav1.DeleteOptions{
				PropagationPolicy: ptr.To(metav1.DeletePropagationBackground),
			})
			cleanupErrs := []error{
				fixtures.WaitDeploymentGone(bg, k8sClient, nsA, depA, "app="+depA, 3*time.Minute),
				fixtures.WaitDeploymentGone(bg, k8sClient, nsA, outsider, "app="+outsider, 2*time.Minute),
				fixtures.WaitDeploymentGone(bg, k8sClient, nsB, lender, "app="+lender, 2*time.Minute),
				fixtures.DeleteKueueCohort(bg, crClient, cohort),
			}
			Expect(errors.Join(cleanupErrs...)).NotTo(HaveOccurred(), "cleanup left Kueue state behind")
		})

		By("Declaring the physical limiter and a namespace quota bounded by Kueue, countBorrowable unset")
		// -1: the static entry names the type and lets Kueue supply the figure.
		restore, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.WVANamespace, scalingPolicyConfigMapName(),
			nsA, product, -1,
			fixtures.WithKueue(refresh),
			fixtures.WithKueueTimings(placementTimeout.String(), borrowCooldown.String(), lenderDebounce.String()),
			fixtures.WithPhysicalLimiter())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restore(context.Background()) })

		By("Placing a 1-GPU pod in namespace a that Kueue does not manage")
		Expect(fixtures.CreateKueueGPUDeployment(ctx, k8sClient, nsA, outsider, "", gpuRN, 1, 1)).To(Succeed())

		By("Creating model A on queue a: one GPU per replica, one replica")
		Expect(fixtures.EnsureModelService(ctx, k8sClient, nsA, msA, msA+"-pool", modelA, cfg.UseSimulator, 2,
			func(d *appsv1.Deployment) {
				d.Spec.Replicas = ptr.To(int32(1))
				if d.Labels == nil {
					d.Labels = map[string]string{}
				}
				d.Labels[fixtures.KueueQueueNameLabel] = queueA
			},
			fixtures.WithGPUs(gpuRN, 1))).To(Succeed())
		Expect(fixtures.EnsureService(ctx, k8sClient, nsA, msA, depA, 8000)).To(Succeed())
		Expect(fixtures.EnsureServiceMonitor(ctx, crClient, cfg.MonitoringNS, nsA, msA, depA)).To(Succeed())
		DeferCleanup(func() {
			_ = crClient.Delete(context.Background(), &promoperator.ServiceMonitor{
				ObjectMeta: metav1.ObjectMeta{Name: msA + "-monitor", Namespace: cfg.MonitoringNS},
			})
		})
		Eventually(func(g Gomega) {
			g.Expect(deployment(g, nsA, depA).Status.ReadyReplicas).To(Equal(int32(1)), unreadyPods(nsA))
		}, time.Duration(cfg.PodReadyTimeout)*time.Second, 2*time.Second).Should(Succeed())

		By("Registering A with the scaling manager, minReplicas 1, maxReplicas 6")
		Expect(fixtures.EnsureScaledObject(ctx, crClient, nsA, soA, depA, depA, 1, maxA, cfg.MonitoringNS,
			fixtures.WithWVATriggerMetadata(modelA, "10.0"),
			fixtures.WithScaledObjectScaleDownStabilizationWindow(window))).To(Succeed())

		By("Loading A far past what four replicas serve")
		job := buildThroughputSustainedLoadJob(nsA, loadJob, fmt.Sprintf("http://%s-service:8000/v1/completions", msA),
			modelA, 32, int64(3600))
		_, err = k8sClient.BatchV1().Jobs(nsA).Create(ctx, job, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("withholds borrowing while a GPU pod in the namespace is not Kueue-managed (B1)", func() {
		Eventually(func(g Gomega) {
			g.Expect(deployment(g, nsA, depA).Status.ReadyReplicas).To(Equal(int32(nominal)), unreadyPods(nsA))
		}, scaleSettle, 5*time.Second).Should(Succeed(), "A never reached its nominal %d under load", nominal)

		// The positive half first, so the hold below cannot pass vacuously: the
		// controller withholds for this reason, and queue a's status really
		// carries A's reservation (a status read that found nothing would read
		// Borrowed as zero too).
		Eventually(func(g Gomega) {
			g.Expect(borrowRowA(g).Reason).To(Equal("not-kueue-managed"))
			g.Expect(queueStatus(g, queueA).Reserved).To(Equal(nominal))
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("A holds at nominal; queue a borrows nothing")
		Consistently(func(g Gomega) {
			g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(BeNumerically("<=", nominal))
			g.Expect(queueStatus(g, queueA).Borrowed).To(BeZero())
		}, 75*time.Second, 5*time.Second).Should(Succeed())
	})

	It("orders A into borrowed capacity once every condition holds, and every pod is placed", func() {
		By("Removing the pod Kueue does not manage")
		Expect(fixtures.WaitDeploymentGone(ctx, k8sClient, nsA, outsider, "app="+outsider, 2*time.Minute)).To(Succeed())
		removed := time.Now()

		var ordered time.Time
		Eventually(func(g Gomega) {
			replicas := *deployment(g, nsA, depA).Spec.Replicas
			if ordered.IsZero() && replicas > nominal {
				ordered = time.Now()
			}
			g.Expect(replicas).To(Equal(int32(reach)))
		}, scaleSettle, poll).Should(Succeed(), "A was never ordered into the cohort's free %d", nominal)
		measure("first borrowed replica ordered after the B1 condition cleared", ordered.Sub(removed))

		Eventually(func(g Gomega) {
			expectAllPlaced(g, reach)
			a := queueStatus(g, queueA)
			g.Expect(a.Reserved).To(Equal(reach))
			g.Expect(a.Borrowed).To(Equal(reach - nominal))
		}, scaleSettle, poll).Should(Succeed())
		measure("borrowed replicas ordered to all Ready", time.Since(ordered))
		measureReplicaStart(nsA, depA, removed, measure)

		Eventually(func(g Gomega) {
			r := borrowRowA(g)
			g.Expect(r.Reason).To(BeEmpty())
			g.Expect(r.Guaranteed).To(Equal(nominal))
			g.Expect(r.KueueBorrowed).To(Equal(reach - nominal))
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("The cohort has no more: A stays at 4 although its ScaledObject allows 6")
		Consistently(func(g Gomega) {
			g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(BeNumerically("<=", reach))
			g.Expect(gatedPods(g)).To(BeEmpty())
		}, 45*time.Second, 5*time.Second).Should(Succeed())
	})

	It("releases exactly one replica to a lender that waits and cannot reclaim", func() {
		releaseLogged := time.Now()
		Expect(fixtures.CreateKueueGPUDeployment(ctx, k8sClient, nsB, lender, queueB, gpuRN, 1, 1)).To(Succeed())
		created := time.Now()

		Eventually(func(g Gomega) {
			b := queueStatus(g, queueB)
			g.Expect(b.Pending).To(Equal(1), "the cohort is full and b cannot reclaim")
		}, kueueSettle, poll).Should(Succeed())

		var lowered time.Time
		Eventually(func(g Gomega) {
			g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(Equal(int32(reach - 1)))
			lowered = time.Now()
		}, scaleSettle, poll).Should(Succeed(), "the scaling manager never released a replica to the waiting lender")
		measure("lender waiting to A's target lowered", lowered.Sub(created))

		Eventually(func(g Gomega) {
			b := queueStatus(g, queueB)
			g.Expect(b.Reserved).To(Equal(1), "the lender's own workload is admitted")
			g.Expect(b.Pending).To(BeZero())
			a := queueStatus(g, queueA)
			g.Expect(a.Reserved).To(Equal(reach - 1))
			g.Expect(gatedPods(g)).To(BeEmpty())
		}, scaleSettle, poll).Should(Succeed())
		measure("lender waiting to lender admitted (release cycle)", time.Since(created))

		Expect(controllerLogsSince(releaseLogged)).To(MatchRegexp(
			`Releasing borrowed capacity.*"cause": "lender-waiting".*"lenderClusterQueue": "` + queueB + `"`))
		Eventually(func(g Gomega) {
			g.Expect(releaseEvents(g, nsA, depA, corev1.EventTypeNormal)).To(
				ContainElement(ContainSubstring("ClusterQueue "+queueB)),
				"the Deployment carries a Normal %s Event naming the lender", borrowReleasedReason)
		}, time.Minute, 2*time.Second).Should(Succeed())

		By("One unit only: A neither drops further nor grows back")
		Consistently(func(g Gomega) {
			g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(Equal(int32(reach - 1)))
			g.Expect(gatedPods(g)).To(BeEmpty())
		}, 45*time.Second, 5*time.Second).Should(Succeed())
	})

	It("lowers A's target after Kueue preempts a borrowed pod, and borrows nothing during the cooldown", func() {
		By("Letting queue b reclaim, then asking for its second GPU")
		Expect(fixtures.SetKueueReclaim(ctx, crClient, queueB, fixtures.KueueReclaimAny)).To(Succeed())
		scaleDeployment(nsB, lender, nominal)
		triggered := time.Now()

		var preempted time.Time
		Eventually(func(g Gomega) {
			a := queueStatus(g, queueA)
			g.Expect(a.Borrowed).To(BeZero(), "Kueue preempts A's borrowed pod")
			g.Expect(queueStatus(g, queueB).Reserved).To(Equal(nominal))
			preempted = time.Now()
		}, kueueSettle, poll).Should(Succeed())
		measure("reclaim: lender's request to Kueue preempting", preempted.Sub(triggered))

		// The scaling manager must have SEEN the preemption: that is what
		// starts the cooldown checked below. Without this the spec passed with
		// preemption detection broken, because the placement timeout alone
		// also lowers A's target.
		reclaimLine := `Kueue preempted a pod of a borrowing model.*"model": ?"` +
			regexp.QuoteMeta(nsA+"/"+modelA) + `"`
		Eventually(func(g Gomega) {
			g.Expect(controllerLogsSince(triggered)).To(MatchRegexp(reclaimLine))
		}, scaleSettle, 5*time.Second).Should(Succeed(), "the scaling manager never recorded Kueue's preemption of A")

		var lowered time.Time
		Eventually(func(g Gomega) {
			g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(Equal(int32(nominal)),
				"gated pods of A now: %v", gatedPods(g))
			g.Expect(gatedPods(g)).To(BeEmpty(), "no gated replacement lingers")
			lowered = time.Now()
		}, placementTimeout+scaleSettle, poll).Should(Succeed(),
			"the gated replacement outlived placementTimeout %s and the KEDA window", placementTimeout)
		Eventually(func(g Gomega) {
			g.Expect(releaseEvents(g, nsA, depA, corev1.EventTypeWarning)).To(
				ContainElement(ContainSubstring("cause placement-timeout")))
		}, time.Minute, 2*time.Second).Should(Succeed())
		measure("reclaim: preemption to A's target lowered with no gated pod", lowered.Sub(preempted))
		measure("reclaim cycle: lender's request to A settled", lowered.Sub(triggered))
		Eventually(func(g Gomega) {
			expectAllPlaced(g, nominal)
		}, scaleSettle, poll).Should(Succeed())

		By("Freeing b's GPUs: A must still borrow nothing until the cooldown ends")
		scaleDeployment(nsB, lender, 0)
		Eventually(func(g Gomega) {
			g.Expect(queueStatus(g, queueB).Reserved).To(BeZero())
		}, kueueSettle, poll).Should(Succeed())
		// The cooldown starts when the engine sees the preemption, which is
		// after b asked for its GPU (triggered). So triggered + borrowCooldown
		// is the EARLIEST the cooldown can end; the check stops a margin before
		// that, and so never runs past the cooldown whenever the engine saw it.
		const margin, minQuiet = 20 * time.Second, 20 * time.Second
		cooldownStart := triggered
		quiet := time.Until(cooldownStart.Add(borrowCooldown - margin))
		skipped := ""
		if quiet < minQuiet {
			// Too slow a cluster to check the hold: say so, and still check
			// that A borrows again after the cooldown. Skipped at the end, so
			// the spec reports as skipped rather than as a pass.
			skipped = fmt.Sprintf("only %s of borrowCooldown %s left after b's GPUs were freed (cooldown started by %s); "+
				"the hold inside the cooldown was not checked", quiet.Round(time.Second), borrowCooldown,
				cooldownStart.Format(time.RFC3339))
			By("SKIPPING the in-cooldown hold: " + skipped)
		} else {
			// Positive precondition, asserted above: A settled at nominal with
			// every pod placed, and the engine logged the preemption.
			Consistently(func(g Gomega) {
				g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(BeNumerically("<=", nominal))
			}, quiet, 5*time.Second).Should(Succeed(), "A borrowed again inside the cooldown")
		}

		By("After the cooldown A borrows again")
		var regrown time.Time
		Eventually(func(g Gomega) {
			g.Expect(*deployment(g, nsA, depA).Spec.Replicas).To(BeNumerically(">", nominal))
			regrown = time.Now()
		}, borrowCooldown+scaleSettle, poll).Should(Succeed())
		measure("preemption to A borrowing again (cooldown "+borrowCooldown.String()+")", regrown.Sub(preempted))
		if skipped != "" {
			Skip(skipped)
		}
	})
})

// borrowLogMessage opens the borrowing line the quota limiter logs on every
// change (allocation.BorrowAppliedLogMessage), restated so the suite reads the
// log as an operator would.
const borrowLogMessage = "Kueue borrowing applied where every condition holds and withheld elsewhere"

// borrowRow mirrors config.ExternalBorrow's JSON, restated so the suite reads
// the log as an operator would rather than through the controller's types.
type borrowRow struct {
	Queue         string `json:"queue"`
	Cohort        string `json:"cohort"`
	Flavor        string `json:"flavor"`
	Resource      string `json:"resource"`
	Accelerator   string `json:"accelerator"`
	Nominal       int    `json:"nominal"`
	Guaranteed    int    `json:"guaranteed"`
	Borrowable    int    `json:"borrowable"`
	KueueBorrowed int    `json:"kueueBorrowed"`
	Reason        string `json:"reason"`
}

// latestBorrowView parses the newest borrowing line in logs. The controller
// logs with zap's console encoder (message, then the fields as one JSON
// object) or its JSON encoder (the whole line one object); both are read.
func latestBorrowView(logs string) (map[string][]borrowRow, bool, error) {
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		at := strings.Index(line, borrowLogMessage)
		if at < 0 {
			continue
		}
		fields := line
		if !strings.HasPrefix(line, "{") {
			open := strings.Index(line[at:], "{")
			if open < 0 {
				return nil, true, fmt.Errorf("borrowing line carries no fields: %s", line)
			}
			fields = line[at+open:]
		}
		var parsed struct {
			BorrowByNamespace map[string][]borrowRow `json:"borrowByNamespace"`
		}
		if err := json.Unmarshal([]byte(fields), &parsed); err != nil {
			return nil, true, fmt.Errorf("parse borrowing line: %w: %s", err, line)
		}
		return parsed.BorrowByNamespace, true, nil
	}
	return nil, false, nil
}

// createKueueNamespace creates ns with the label Kueue's narrowed webhooks
// select, waiting out a namespace of the same name still terminating from an
// earlier run.
func createKueueNamespace(ns string) {
	Eventually(func(g Gomega) {
		_, err := k8sClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: ns, Labels: map[string]string{fixtures.KueueE2ENamespaceLabel: "true"},
		}}, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			existing, getErr := k8sClient.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
			g.Expect(getErr).NotTo(HaveOccurred())
			g.Expect(existing.DeletionTimestamp).To(BeNil(), "namespace %s is still terminating", ns)
			g.Expect(existing.Labels).To(HaveKey(fixtures.KueueE2ENamespaceLabel),
				"namespace %s exists without the Kueue webhook label", ns)
			return
		}
		g.Expect(err).NotTo(HaveOccurred())
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

// freeGPUsOfProduct sums the free GPUs on the schedulable nodes labelled
// key=product.
func freeGPUsOfProduct(key, product, resourceName string) int {
	nodes, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: key + "=" + product})
	Expect(err).NotTo(HaveOccurred())
	total := 0
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable {
			continue
		}
		free, err := fixtures.FreeGPUsOnNode(ctx, k8sClient, n, resourceName)
		Expect(err).NotTo(HaveOccurred())
		total += max(free, 0)
	}
	return total
}

// measureReplicaStart reports, for each pod of the Deployment created at or
// after since, how long it took from creation to Ready.
func measureReplicaStart(ns, app string, since time.Time, measure func(string, time.Duration)) {
	pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
	Expect(err).NotTo(HaveOccurred())
	for _, p := range pods.Items {
		created := p.CreationTimestamp.Time
		if created.Before(since.Add(-time.Second)) {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				measure("replica start (created to Ready) "+p.Name, c.LastTransitionTime.Sub(created))
			}
		}
	}
}

// borrowReleasedReason is the Event reason the scaling manager records on a
// scale target it lowered to give borrowed capacity back
// (constants.K8SEventBorrowedCapacityReleased).
const borrowReleasedReason = "BorrowedCapacityReleased"

// releaseEvents are the messages of the release Events of eventType on the
// Deployment ns/name.
func releaseEvents(g Gomega, ns, name, eventType string) []string {
	events, err := k8sClient.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Deployment,involvedObject.name=" + name + ",reason=" + borrowReleasedReason,
	})
	g.Expect(err).NotTo(HaveOccurred())
	var msgs []string
	for _, e := range events.Items {
		if e.Type == eventType {
			msgs = append(msgs, e.Message)
		}
	}
	return msgs
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gateNames(p corev1.Pod) []string {
	names := make([]string, 0, len(p.Spec.SchedulingGates))
	for _, g := range p.Spec.SchedulingGates {
		names = append(names, g.Name)
	}
	return names
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	return names
}
