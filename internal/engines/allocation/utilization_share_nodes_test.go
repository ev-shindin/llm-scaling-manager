package allocation

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Node-aware donor sets (§6.5)", func() {
	// B, far short, grows by one replica. A and C hold surplus in Deployment
	// replicas of one 8-GPU pod, whose pods run on the nodes given.
	type pod struct{ name, node string }
	run := func(receiverPods []int, nodes map[string]ShareNode, a, c []pod, domainKey string) SharePlan {
		roles := []ShareRole{
			{Key: "A", Weight: 1, Need: 4, Ceiling: 64, ReplicaGPUs: 8},
			{Key: "B", Weight: 1, Need: 64, Ceiling: 64, ReplicaGPUs: 16},
			{Key: "C", Weight: 1, Need: 4, Ceiling: 64, ReplicaGPUs: 8},
		}
		units := func(ps []pod) []ShareUnit {
			out := make([]ShareUnit, 0, len(ps))
			for _, p := range ps {
				out = append(out, ShareUnit{Pods: []SharePod{{Name: p.name, Node: p.node, GPUs: 8}}})
			}
			return out
		}
		in := SharePlanInput{Roles: roles, Held: map[string]int{"A": 24, "B": 16, "C": 24},
			Thresholds: map[string]float64{"A": 0.8, "B": 0.8, "C": 0.8}, Budget: 64, Tolerance: 0.15,
			Give: map[string]ShareVariant{
				"A": {Name: "a", GPUs: 8, PodGPUs: []int{8}},
				"C": {Name: "c", GPUs: 8, PodGPUs: []int{8}},
			},
			Grow:       map[string]ShareVariant{"B": {Name: "b", GPUs: 16, PodGPUs: receiverPods}},
			Nodes:      nodes,
			DonorUnits: map[string][]ShareUnit{"A": units(a), "C": units(c)},
		}
		if domainKey != "" {
			in.DomainKey = map[string]string{"B": domainKey}
		}
		l := NewShareLedger()
		var p SharePlan
		for i := range 3 {
			if p = PlanShareTransfers(l, in, time.Unix(int64(30*i), 0), simTimings()); len(p.Started) > 0 {
				break
			}
		}
		return p
	}
	planned := func(p SharePlan) []string {
		out := make([]string, 0, 2*len(p.Started))
		for _, t := range p.Started {
			out = append(out, t.PlannedPods...)
		}
		return out
	}
	full := func(names ...string) map[string]ShareNode {
		m := map[string]ShareNode{}
		for _, n := range names {
			m[n] = ShareNode{}
		}
		return m
	}

	It("adds up two donor pods sharing a node into one hole for a larger receiver pod", func() {
		p := run([]int{16}, full("n1", "n2"),
			[]pod{{"ns/a-0", "n1"}, {"ns/a-1", "n2"}}, []pod{{"ns/c-0", "n1"}}, "")
		Expect(p.Started).To(HaveLen(2))
		Expect(p.Started[0].IsSetPrimary()).To(BeTrue())
		Expect(p.Started[1].SetID).To(Equal(p.Started[0].ID))
		Expect(planned(p)).To(ConsistOf("ns/a-0", "ns/c-0"), "the two pods on n1, not a-1 on n2")
	})

	It("finds nothing when the donor pods are on different nodes (control)", func() {
		p := run([]int{16}, full("n1", "n2"),
			[]pod{{"ns/a-0", "n1"}}, []pod{{"ns/c-0", "n2"}}, "")
		Expect(p.Started).To(BeEmpty())
		Expect(p.Unfunded).To(HaveKeyWithValue("B", ShareUnfundedNoCompatibleDonor))
	})

	It("never uses free GPUs in place of a donor's quota", func() {
		// n1 has 8 free and a-0: the hole fits, but one donor replica gives 8
		// of the 16 the receiver's replica costs.
		nodes := full("n1", "n2")
		nodes["n1"] = ShareNode{Free: 8}
		p := run([]int{16}, nodes, []pod{{"ns/a-0", "n1"}}, nil, "")
		Expect(p.Started).To(BeEmpty())
	})

	It("keeps every hole of an exclusive-topology receiver in one domain", func() {
		nodes := map[string]ShareNode{
			"n1": {Labels: map[string]string{"rack": "r1"}},
			"n2": {Labels: map[string]string{"rack": "r2"}},
			"n3": {Labels: map[string]string{"rack": "r1"}},
		}
		p := run([]int{8, 8}, nodes,
			[]pod{{"ns/a-0", "n1"}}, []pod{{"ns/c-0", "n2"}, {"ns/c-1", "n3"}}, "rack")
		Expect(p.Started).To(HaveLen(2))
		Expect(planned(p)).To(ConsistOf("ns/a-0", "ns/c-1"), "c-0 is on rack r2")
	})

	It("crosses domains for a receiver without one (control)", func() {
		nodes := map[string]ShareNode{
			"n1": {Labels: map[string]string{"rack": "r1"}},
			"n2": {Labels: map[string]string{"rack": "r2"}},
			"n3": {Labels: map[string]string{"rack": "r1"}},
		}
		p := run([]int{8, 8}, nodes,
			[]pod{{"ns/a-0", "n1"}}, []pod{{"ns/c-0", "n2"}, {"ns/c-1", "n3"}}, "")
		Expect(p.Started).To(HaveLen(2))
		Expect(planned(p)).To(ConsistOf("ns/a-0", "ns/c-0"), "first node in name order")
	})

	It("starts nothing for a domain receiver when no domain holds enough", func() {
		nodes := map[string]ShareNode{
			"n1": {Labels: map[string]string{"rack": "r1"}},
			"n2": {Labels: map[string]string{"rack": "r2"}},
		}
		p := run([]int{8, 8}, nodes, []pod{{"ns/a-0", "n1"}}, []pod{{"ns/c-0", "n2"}}, "rack")
		Expect(p.Started).To(BeEmpty(), "the node-blind search must not be used for a domain receiver")
	})
})

var _ = Describe("Which donor pod went (§6.5)", func() {
	tm := simTimings()
	t0 := time.Unix(0, 0)
	start := func(l *ShareLedger) ShareTransfer {
		return l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 8, DonorGPUs: 8, PlannedPods: []string{"ns/a-0"}},
			map[string]int{"A": 16}, t0, tm)
	}

	It("ends a planned transfer whose donor shrank while its planned pod still runs, raising nothing", func() {
		l := NewShareLedger()
		t := start(l)
		l.PlannedRunning(map[string]bool{t.ID: true})
		ends := l.Observe(map[string]int{"A": 8}, t0.Add(time.Minute), tm)
		Expect(ends).To(HaveLen(1))
		Expect(ends[0].Outcome).To(Equal(ShareOutcomeWrongPod))
		Expect(l.TakeReleased()).To(BeEmpty(), "the receiver must not be raised into a hole that did not open")
		Expect(l.Promised()).To(BeZero())
		Expect(l.GivingHeld("A", t0.Add(time.Minute), tm)).To(BeFalse(), "the donor released promptly: no back-off")
	})

	It("releases normally once the planned pod is the one that went (control)", func() {
		l := NewShareLedger()
		start(l)
		l.PlannedRunning(nil)
		Expect(l.Observe(map[string]int{"A": 8}, t0.Add(time.Minute), tm)).To(BeEmpty())
		Expect(l.TakeReleased()).To(HaveLen(1))
	})

	It("does nothing while the donor has not shrunk, planned pod running or not", func() {
		l := NewShareLedger()
		t := start(l)
		l.PlannedRunning(map[string]bool{t.ID: true})
		Expect(l.Observe(map[string]int{"A": 16}, t0.Add(time.Minute), tm)).To(BeEmpty())
		got, _ := l.Transfer(t.ID)
		Expect(got.State).To(Equal(ShareReleasing))
	})
})
