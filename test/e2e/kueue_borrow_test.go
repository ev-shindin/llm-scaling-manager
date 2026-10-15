package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
	testutils "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
)

// Cohort borrowing as the scaling manager READS it, checked against a live
// Kueue controller. Kueue admits, borrows and reclaims; the spec asserts each
// state through ClusterQueue status, then asserts that the figures the
// controller logs for it -- the read-only borrowing line, "Kueue borrowing read
// and NOT applied" -- agree. Nothing here is applied: the quota entry still
// caps at nominal, so no model is involved.
//
// Two ClusterQueues of 8 GPUs share a flat cohort. Queue a reclaims (Any);
// queue b starts as a lender that cannot (Never). Each figure below is
// ComputeBorrowing (internal/kueue/borrow.go) evaluated by hand on the
// reservation Kueue reports, with no lending or borrowing limits:
//
//	state               a: res/borrowed  b: res/pending  a: G/B/kueueBorrowed  b: G/B, reason
//	a borrows 4          12 / 4           0 / 0           8 / 8 / 4             0 / 4, nominal-lent-out
//	b waits (Never)      12 / 4           4 / 1           8 / 4 / 4             4 / 0, nominal-lent-out
//	b reclaims (Any)      8 / 0           8 / 0           8 / 0 / 0             8 / 0
//
// The pods pin to a GPU product no node carries, so they are admitted but never
// scheduled. Kueue's quota accounting does not depend on placement, and the
// spec takes no emulated GPU another spec might be waiting for.
//
// Needs Kueue's controller, not only its CRDs: on kind,
// `make test-e2e-full-with-setup` installs it (KUEUE_CONTROLLER=true, the
// default) through deploy/ci-pr-checks/install-kueue.sh, and its absence there
// FAILS the spec. On any other cluster the spec runs where a Kueue controller is
// available and skips, saying so, where it is not.
var _ = Describe("Kueue cohort borrowing - read against a live Kueue", Label("full"), Label("kueue-borrow"), Ordered, Serial, func() {
	const (
		cohortName  = "e2e-kueue-borrow"
		flavorName  = "e2e-kueue-borrow"
		heldAccel   = "KUEUEBORROW"
		product     = "NVIDIA-" + heldAccel + "-80GB"
		gpuResource = "nvidia.com/gpu"
		queueA      = "e2e-kueue-borrow-a"
		queueB      = "e2e-kueue-borrow-b"
		nsA         = "e2e-kueue-borrow-a"
		nsB         = "e2e-kueue-borrow-b"
		deployA     = "kueue-borrow-a"
		deployB     = "kueue-borrow-b"
		nominal     = 8
		gpusPerPod  = 4
		refresh     = "5s"
		// Kueue settles in under a second on an idle machine; a CI runner
		// running the rest of the suite is not idle.
		kueueSettle = 2 * time.Minute
		// The controller re-reads Kueue every refresh and logs on the next
		// engine cycle that sees a change.
		logSettle = 3 * time.Minute
	)

	var (
		ctx     context.Context
		started time.Time
		cohort  fixtures.KueueCohort
	)

	queueStatus := func(g Gomega, cq string) fixtures.KueueQueueStatus {
		st, err := fixtures.ReadKueueQueueStatus(ctx, crClient, cq, flavorName, gpuResource)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(st.HasStatus).To(BeTrue(), "ClusterQueue %s has no reservation status for %s; is Kueue running?", cq, gpuResource)
		return st
	}

	// borrowView is the newest read-only borrowing line logged since the spec
	// started, keyed by namespace.
	borrowView := func(g Gomega) map[string][]borrowRow {
		since := int64(time.Since(started).Seconds()) + 60
		_, logs, err := testutils.PodLogsLabelSelectorContain(ctx, k8sClient, cfg.WVANamespace,
			"control-plane=controller-manager", "", since)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(logs).NotTo(ContainSubstring("external quota source unreadable"),
			"the reader must be able to list the Kueue objects -- a Forbidden here is an RBAC defect")
		view, found, err := latestBorrowView(logs)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(found).To(BeTrue(), "no %q line yet; countBorrowable is left unset, which must mean on", borrowLogMessage)
		return view
	}

	// rowFor is the one row of a namespace for the cohort's GPU resource.
	rowFor := func(g Gomega, view map[string][]borrowRow, ns string) borrowRow {
		var rows []borrowRow
		for _, r := range view[ns] {
			if r.Flavor == flavorName && r.Resource == gpuResource {
				rows = append(rows, r)
			}
		}
		g.Expect(rows).To(HaveLen(1), "namespace %s rows: %+v", ns, view[ns])
		return rows[0]
	}

	expectRow := func(g Gomega, r borrowRow, queue string, guaranteed, borrowable, kueueBorrowed int, reason string) {
		g.Expect(r.Queue).To(Equal(queue))
		g.Expect(r.Cohort).To(Equal(cohortName))
		// The reader passes the flavor's product label through as the accelerator.
		g.Expect(r.Accelerator).To(Equal(product))
		g.Expect(r.Nominal).To(Equal(nominal))
		g.Expect(r.Guaranteed).To(Equal(guaranteed), "guaranteed of %s", queue)
		g.Expect(r.Borrowable).To(Equal(borrowable), "borrowable of %s", queue)
		g.Expect(r.KueueBorrowed).To(Equal(kueueBorrowed), "kueueBorrowed of %s", queue)
		g.Expect(r.Reason).To(Equal(reason), "reason of %s", queue)
	}

	podsIn := func(g Gomega, ns, app string) []corev1.Pod {
		pods, err := k8sClient.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "app=" + app})
		g.Expect(err).NotTo(HaveOccurred())
		return pods.Items
	}

	BeforeAll(func() {
		ctx = context.Background()
		started = time.Now()

		if !fixtures.KueueInstalled(crClient) {
			if cfg.Environment == envKindEmulator {
				Fail("the Kueue CRDs are not installed on this kind cluster; test-e2e-full-with-setup installs " +
					"Kueue via deploy/ci-pr-checks/install-kueue.sh (KUEUE_CONTROLLER=true)")
			}
			Skip("Kueue is not installed on this cluster; the live-Kueue borrowing spec needs it and does not install it outside kind")
		}
		running, what, err := fixtures.KueueControllerRunning(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		if !running {
			if cfg.Environment == envKindEmulator {
				Fail("the Kueue CRDs are served but no Kueue controller is running (" + what + "); " +
					"install-kueue-crds.sh installs only the CRDs -- run test-e2e-full-with-setup with " +
					"KUEUE_CONTROLLER=true (the default) or deploy/ci-pr-checks/install-kueue.sh")
			}
			Skip("no Kueue controller is running on this cluster (" + what + "); the borrowing spec needs one")
		}
		GinkgoWriter.Printf("Kueue controller: %s\n", what)

		By("Creating the two namespaces, labelled for Kueue's webhooks")
		for _, ns := range []string{nsA, nsB} {
			// A namespace from an earlier run may still be terminating; nothing
			// can be created in it until it is gone.
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
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		}
		DeferCleanup(func() {
			for _, ns := range []string{nsA, nsB} {
				_ = k8sClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
			}
		})

		By("Creating a flat cohort: queue a reclaims, queue b lends and cannot reclaim")
		cohort = fixtures.KueueCohort{
			Cohort:       cohortName,
			Flavor:       flavorName,
			ProductLabel: "nvidia.com/gpu.product",
			Product:      product,
			Resource:     gpuResource,
			Queues: []fixtures.KueueCohortQueue{
				{Name: queueA, Namespace: nsA, GPUs: nominal, Reclaim: fixtures.KueueReclaimAny},
				{Name: queueB, Namespace: nsB, GPUs: nominal, Reclaim: fixtures.KueueReclaimNever},
			},
		}
		Expect(fixtures.CreateKueueCohort(ctx, crClient, cohort)).To(Succeed())
		DeferCleanup(func() {
			// Workloads first: Kueue holds a ClusterQueue while anything
			// reserves in it.
			bg := context.Background()
			Expect(fixtures.WaitDeploymentGone(bg, k8sClient, nsA, deployA, "app="+deployA, 2*time.Minute)).To(Succeed())
			Expect(fixtures.WaitDeploymentGone(bg, k8sClient, nsB, deployB, "app="+deployB, 2*time.Minute)).To(Succeed())
			Expect(fixtures.DeleteKueueCohort(bg, crClient, cohort)).To(Succeed())
		})

		By("Bounding namespace a by Kueue, with countBorrowable left unset")
		// The static figure is above anything Kueue grants, so the effective
		// entry is Kueue's nominal; the borrowing figures are logged beside it.
		restoreConfig, err := fixtures.SetNamespaceQuota(ctx, k8sClient, cfg.WVANamespace,
			scalingPolicyConfigMapName(), nsA, heldAccel, 4*nominal, fixtures.WithKueue(refresh))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = restoreConfig(context.Background()) })
	})

	It("Kueue admits queue a past its nominal quota, borrowing 4 from b", func() {
		Expect(fixtures.CreateKueueGPUDeployment(ctx, k8sClient, nsA, deployA, queueA, gpuResource, 3, gpusPerPod)).To(Succeed())

		Eventually(func(g Gomega) {
			a := queueStatus(g, queueA)
			g.Expect(a.Reserved).To(Equal(3*gpusPerPod), "queue a reserves all three pods")
			g.Expect(a.Borrowed).To(Equal(3*gpusPerPod-nominal), "the part past nominal is borrowed")
			g.Expect(a.Pending).To(BeZero())
			b := queueStatus(g, queueB)
			g.Expect(b.Reserved).To(BeZero())
		}, kueueSettle, time.Second).Should(Succeed())

		By("Every pod is Kueue-managed and admitted")
		Eventually(func(g Gomega) {
			pods := podsIn(g, nsA, deployA)
			g.Expect(pods).To(HaveLen(3))
			for _, p := range pods {
				g.Expect(p.Labels).To(HaveKeyWithValue(fixtures.KueueManagedLabel, "true"), "pod %s", p.Name)
				g.Expect(gateNames(p)).NotTo(ContainElement(fixtures.KueueAdmissionGate), "pod %s", p.Name)
			}
		}, kueueSettle, time.Second).Should(Succeed())
	})

	It("logs figures for both namespaces that agree with Kueue's status", func() {
		Eventually(func(g Gomega) {
			a := queueStatus(g, queueA)
			view := borrowView(g)
			ra := rowFor(g, view, nsA)
			g.Expect(ra.KueueBorrowed).To(Equal(a.Borrowed), "the logged cross-check is Kueue's own figure")
			// Free in the cohort: 16 lent - 12 used = 4. Queue a reclaims, so
			// it is guaranteed its nominal and could reach 12 + 4 = 16.
			expectRow(g, ra, queueA, nominal, nominal, 4, "")
			// Queue b cannot reclaim and holds nothing: its whole nominal is
			// lent out, and what it could still take is the cohort's free 4.
			expectRow(g, rowFor(g, view, nsB), queueB, 0, 4, 0, "nominal-lent-out")
		}, logSettle, 5*time.Second).Should(Succeed())
	})

	It("a Never lender with work pending waits, and the figures shrink to what is left", func() {
		Expect(fixtures.CreateKueueGPUDeployment(ctx, k8sClient, nsB, deployB, queueB, gpuResource, 2, gpusPerPod)).To(Succeed())

		Eventually(func(g Gomega) {
			b := queueStatus(g, queueB)
			g.Expect(b.Reserved).To(Equal(gpusPerPod), "the first pod fits in the cohort's free 4")
			g.Expect(b.Pending).To(Equal(1), "the second has no quota left and cannot reclaim")
		}, kueueSettle, time.Second).Should(Succeed())

		By("Kueue does not reclaim for a Never queue: a keeps what it borrowed")
		Consistently(func(g Gomega) {
			a := queueStatus(g, queueA)
			g.Expect(a.Borrowed).To(Equal(3*gpusPerPod - nominal))
			g.Expect(queueStatus(g, queueB).Pending).To(Equal(1))
		}, 15*time.Second, time.Second).Should(Succeed())

		By("The lender-waiting predicate holds on the status the reader reads: pending, below nominal")
		Eventually(func(g Gomega) {
			b := queueStatus(g, queueB)
			g.Expect(b.Pending).To(BeNumerically(">", 0))
			g.Expect(b.Reserved).To(BeNumerically("<", nominal))
		}, kueueSettle, time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			view := borrowView(g)
			// The cohort is full (12 + 4 = 16): a can reach only what it holds.
			expectRow(g, rowFor(g, view, nsA), queueA, nominal, 4, 4, "")
			// b is guaranteed only what it reserves and can borrow nothing.
			expectRow(g, rowFor(g, view, nsB), queueB, gpusPerPod, 0, 0, "nominal-lent-out")
		}, logSettle, 5*time.Second).Should(Succeed())
	})

	It("once b may reclaim, Kueue preempts a borrowed pod of a and gates its replacement", func() {
		var before []string
		Eventually(func(g Gomega) {
			before = podNames(podsIn(g, nsA, deployA))
			g.Expect(before).To(HaveLen(3))
		}, kueueSettle, time.Second).Should(Succeed())

		By("Switching queue b to reclaimWithinCohort: Any")
		Expect(fixtures.SetKueueReclaim(ctx, crClient, queueB, fixtures.KueueReclaimAny)).To(Succeed())

		Eventually(func(g Gomega) {
			a := queueStatus(g, queueA)
			g.Expect(a.Borrowed).To(BeZero(), "queue a no longer borrows")
			g.Expect(a.Reserved).To(Equal(nominal))
			g.Expect(a.Pending).To(Equal(1), "the preempted pod's replacement waits")
			b := queueStatus(g, queueB)
			g.Expect(b.Reserved).To(Equal(nominal))
			g.Expect(b.Pending).To(BeZero())
		}, kueueSettle, time.Second).Should(Succeed())

		By("Exactly one of a's pods was deleted, and its replacement is gated")
		Eventually(func(g Gomega) {
			pods := podsIn(g, nsA, deployA)
			live := map[string]bool{}
			var gated []string
			for _, p := range pods {
				if p.DeletionTimestamp == nil {
					live[p.Name] = true
				}
				if slices.Contains(gateNames(p), fixtures.KueueAdmissionGate) {
					gated = append(gated, p.Name)
				}
			}
			var gone []string
			for _, name := range before {
				if !live[name] {
					gone = append(gone, name)
				}
			}
			g.Expect(gone).To(HaveLen(1), "one borrowed pod is preempted; pods now: %v", podNames(pods))
			g.Expect(gated).To(HaveLen(1), "the ReplicaSet's replacement waits at Kueue's admission gate")
			g.Expect(before).NotTo(ContainElement(gated[0]), "the gated pod is the new one")
		}, kueueSettle, time.Second).Should(Succeed())

		Eventually(func(g Gomega) {
			view := borrowView(g)
			expectRow(g, rowFor(g, view, nsA), queueA, nominal, 0, 0, "")
			expectRow(g, rowFor(g, view, nsB), queueB, nominal, 0, 0, "")
		}, logSettle, 5*time.Second).Should(Succeed())
	})
})

// borrowLogMessage is the read-only borrowing line the quota limiter logs on
// every change (internal/engines/allocation/quota_inventory.go).
const borrowLogMessage = "Kueue borrowing read and NOT applied; the quota entry still caps at nominal"

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
