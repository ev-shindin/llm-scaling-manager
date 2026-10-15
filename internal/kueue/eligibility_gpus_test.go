package kueue

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// sizedWorkloadFor is a reserved Workload named name, owned by owner, with one
// pod set per entry of counts, each pod of which requests gpus GPUs: the
// shape Kueue writes for a LeaderWorkerSet group (a leader set and a worker
// set).
func sizedWorkloadFor(owner client.Object, name, gpus string, counts ...int64) client.Object {
	w := workloadFor(owner)
	w.SetName(name)
	podSets := make([]interface{}, 0, len(counts))
	for i, n := range counts {
		podSets = append(podSets, map[string]interface{}{
			"name": []string{"leader", "workers"}[min(i, 1)], "count": n,
			"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []interface{}{
				map[string]interface{}{"name": "c", "resources": map[string]interface{}{
					"requests": map[string]interface{}{"nvidia.com/gpu": gpus}}},
			}}},
		})
	}
	w.Object["spec"] = map[string]interface{}{"podSets": podSets}
	return w
}

// Review finding (security): the managed check asked only that a reserved
// Workload request some GPU. A tenant with create on Workloads made a 1-GPU
// one owned by its LeaderWorkerSet, Kueue admitted it, and every pod of a
// 16-GPU-per-group set passed. The reserved Workloads of an owner must cover
// the GPUs its pods request.
var _ = Describe("Eligibility: the charge covers the pods' GPUs", func() {
	lws := owned("leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", "serve", "lws-uid", "lq", nil)
	sts := owned("apps/v1", "StatefulSet", "serve-0", "sts-uid", "", lws)
	leader := owned("v1", "Pod", "serve-0", "leader-uid", "", sts)
	workerSts := owned("apps/v1", "StatefulSet", "serve-0-workers", "wsts-uid", "", leader)
	// Two 2x8 groups: a leader and a worker each, 32 GPUs.
	pods := []client.Object{
		gpuPod("g0-leader", 8, sts, labelled), gpuPod("g0-worker", 8, workerSts, labelled),
		gpuPod("g1-leader", 8, sts, labelled), gpuPod("g1-worker", 8, workerSts, labelled),
	}
	set := []client.Object{lws, sts, leader, workerSts}

	DescribeTable("the managed check",
		func(want Reason, objs ...client.Object) {
			all := append(append(append([]client.Object{}, set...), pods...), objs...)
			Expect(evaluate(eligibilityClient(nil, all...)).Reason).To(Equal(want))
		},
		Entry("one reserved Workload per group, 16 GPUs each, covers the two groups", Reason(""),
			sizedWorkloadFor(lws, "wl-g0", "8", 1, 1), sizedWorkloadFor(lws, "wl-g1", "8", 1, 1)),
		Entry("a forged 1-GPU Workload owned by the LeaderWorkerSet covers none of its 32 GPUs", ReasonNotKueueManaged,
			sizedWorkloadFor(lws, "wl-forged", "1", 1)),
		Entry("one group's Workload does not cover two groups", ReasonNotKueueManaged,
			sizedWorkloadFor(lws, "wl-g0", "8", 1, 1)),
	)

	It("keeps the pod integration exact per pod", func() {
		p := gpuPod("p", 8, nil, labelled)
		Expect(evaluate(eligibilityClient(nil, p, sizedWorkloadFor(p, "wl-p", "8", 1))).Reason).To(Equal(Reason("")))
		Expect(evaluate(eligibilityClient(nil, p, sizedWorkloadFor(p, "wl-p", "1", 1))).Reason).To(Equal(ReasonNotKueueManaged),
			"a 1-GPU Workload owned by an 8-GPU pod")
	})

	// Review round 4 (security): each Workload's GPUs were credited in full to
	// every owner it names, so one 8-GPU Workload naming ten 8-GPU pods, or
	// several sets, covered them all. A Workload now counts once, for the pods
	// of all its owners together.
	Describe("a Workload naming several owners", func() {
		ownedByAll := func(w client.Object, owners ...client.Object) client.Object {
			refs := make([]metav1.OwnerReference, 0, len(owners))
			for _, o := range owners {
				apiVersion, kind := "v1", "Pod"
				if u, ok := o.(*unstructured.Unstructured); ok {
					apiVersion, kind = u.GetAPIVersion(), u.GetKind()
				}
				refs = append(refs, metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: o.GetName(), UID: o.GetUID()})
			}
			w.SetOwnerReferences(refs)
			return w
		}
		group := func(n int) []client.Object {
			out := make([]client.Object, 0, n)
			for i := range n {
				out = append(out, gpuPod(fmt.Sprintf("g-%d", i), 8, nil, labelled))
			}
			return out
		}

		It("does not let one 8-GPU Workload cover ten 8-GPU pods", func() {
			pods := group(10)
			objs := append(pods, ownedByAll(sizedWorkloadFor(pods[0], "wl-forged", "8", 1), pods...))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(ReasonNotKueueManaged))
		})

		It("keeps a genuine pod group managed: its Workload names every pod and covers them all", func() {
			pods := group(4)
			objs := append(pods, ownedByAll(sizedWorkloadFor(pods[0], "wl-group", "8", 4), pods...))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(Reason("")))
		})

		It("does not let one Workload cover two LeaderWorkerSets", func() {
			lwsB := owned("leaderworkerset.x-k8s.io/v1", "LeaderWorkerSet", "other", "lws-b-uid", "lq", nil)
			stsB := owned("apps/v1", "StatefulSet", "other-0", "sts-b-uid", "", lwsB)
			objs := append(append(append([]client.Object{}, set...), pods...), lwsB, stsB,
				gpuPod("b0-leader", 8, stsB, labelled), gpuPod("b1-leader", 8, stsB, labelled),
				// 32 GPUs, enough for either set alone (32 and 16), not for both (48).
				ownedByAll(sizedWorkloadFor(lws, "wl-both", "8", 1, 3), lws, lwsB))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(ReasonNotKueueManaged))
		})

		It("keeps a genuine LeaderWorkerSet managed: one Workload per group, all owned by the set", func() {
			objs := append(append(append([]client.Object{}, set...), pods...),
				sizedWorkloadFor(lws, "wl-g0", "8", 1, 1), sizedWorkloadFor(lws, "wl-g1", "8", 1, 1))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(Reason("")))
		})
	})

	// Kueue review m8: with partial admission Kueue admits, and charges, fewer
	// pods than the spec's count; the admitted count is what covers the pods.
	Describe("the admitted pod count", func() {
		job := owned("batch/v1", "Job", "train", "job-uid", "lq", nil)
		admitted := func(w client.Object, count int64) client.Object {
			u := w.(*unstructured.Unstructured)
			u.Object["status"] = map[string]interface{}{
				"admission": map[string]interface{}{"clusterQueue": "cq", "podSetAssignments": []interface{}{
					map[string]interface{}{"name": "leader", "count": count},
				}},
			}
			return u
		}
		jobPods := func(n int) []client.Object {
			out := make([]client.Object, 0, n)
			for i := range n {
				out = append(out, gpuPod(fmt.Sprintf("j-%d", i), 8, job, labelled))
			}
			return out
		}

		It("charges the admitted count, not the spec's", func() {
			objs := append(jobPods(4), job, admitted(sizedWorkloadFor(job, "wl-job", "8", 4), 2))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(ReasonNotKueueManaged),
				"4 pods of 8 GPUs, 2 admitted")
			objs = append(jobPods(2), job, admitted(sizedWorkloadFor(job, "wl-job", "8", 4), 2))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(Reason("")), "the 2 admitted pods")
		})

		It("falls back to the spec's count without podSetAssignments", func() {
			objs := append(jobPods(4), job, sizedWorkloadFor(job, "wl-job", "8", 4))
			Expect(evaluate(eligibilityClient(nil, objs...)).Reason).To(Equal(Reason("")))
		})
	})

	It("counts a pod at the admission gate against nothing", func() {
		p := gpuPod("p", 8, nil, labelled, gated)
		Expect(evaluate(eligibilityClient(nil, p, sizedWorkloadFor(p, "wl-p", "1", 1))).Reason).To(Equal(Reason("")),
			"a gated pod holds no GPU")
	})
})
