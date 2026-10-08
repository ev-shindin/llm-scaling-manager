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
		l.PlannedRunning(map[string]bool{t.ID: true}, nil)
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
		l.PlannedRunning(nil, nil)
		Expect(l.Observe(map[string]int{"A": 8}, t0.Add(time.Minute), tm)).To(BeEmpty())
		Expect(l.TakeReleased()).To(HaveLen(1))
	})

	It("never releases a set's primary when a contributor's planned pod is the wrong one", func() {
		l := NewShareLedger()
		held := map[string]int{"A": 8, "C": 8}
		p := l.Start(ShareTransfer{Donor: "A", Receiver: "B", GPUs: 16, DonorGPUs: 8, SetID: "pending",
			PlannedPods: []string{"ns/a-0"}}, held, t0, tm)
		c := l.Start(ShareTransfer{Donor: "C", DonorGPUs: 8, SetID: "pending", PlannedPods: []string{"ns/c-0"}},
			held, t0, tm)
		l.LinkSet(p.ID, c.ID)
		// Both donors shrank; A lost its planned pod, C lost another one.
		l.PlannedRunning(map[string]bool{c.ID: true}, nil)
		ends := l.Observe(map[string]int{"A": 0, "C": 0}, t0.Add(time.Minute), tm)
		Expect(ends).To(HaveLen(1))
		Expect(ends[0].Transfer.ID).To(Equal(c.ID))
		Expect(ends[0].Outcome).To(Equal(ShareOutcomeWrongPod))
		Expect(l.TakeReleased()).To(BeEmpty(), "the receiver must not be raised on half a set")
		// The set is now broken: its primary is aborted, not released.
		l.PlannedRunning(nil, nil)
		ends = l.Observe(map[string]int{"A": 0, "C": 0}, t0.Add(2*time.Minute), tm)
		Expect(ends).To(HaveLen(1))
		Expect(ends[0].Transfer.ID).To(Equal(p.ID))
		Expect(ends[0].Outcome).To(Equal(ShareOutcomeAborted))
		Expect(l.TakeReleased()).To(BeEmpty())
	})

	It("holds a transfer whose planned pod could not be read: neither released nor wrong", func() {
		l := NewShareLedger()
		t := start(l)
		l.PlannedRunning(nil, map[string]bool{t.ID: true})
		Expect(l.Observe(map[string]int{"A": 8}, t0.Add(time.Minute), tm)).To(BeEmpty())
		Expect(l.TakeReleased()).To(BeEmpty())
		got, _ := l.Transfer(t.ID)
		Expect(got.State).To(Equal(ShareReleasing))
		l.PlannedRunning(nil, nil)
		Expect(l.Observe(map[string]int{"A": 8}, t0.Add(2*time.Minute), tm)).To(BeEmpty())
		Expect(l.TakeReleased()).To(HaveLen(1), "released once the pod reads as gone")
	})

	It("does nothing while the donor has not shrunk, planned pod running or not", func() {
		l := NewShareLedger()
		t := start(l)
		l.PlannedRunning(map[string]bool{t.ID: true}, nil)
		Expect(l.Observe(map[string]int{"A": 16}, t0.Add(time.Minute), tm)).To(BeEmpty())
		got, _ := l.Transfer(t.ID)
		Expect(got.State).To(Equal(ShareReleasing))
	})
})

var _ = Describe("Node-aware donor sets: guards and shapes (§6.5)", func() {
	// B grows by one replica; A and C give Deployment replicas of one 8-GPU
	// pod each, on the nodes given. mut adjusts the input before planning.
	pod8 := func(name, node string) ShareUnit {
		return ShareUnit{Pods: []SharePod{{Name: name, Node: node, GPUs: 8}}}
	}
	plan := func(receiverPods []int, nodes map[string]ShareNode, a, c []ShareUnit, mut func(*SharePlanInput)) SharePlan {
		g := 0
		for _, p := range receiverPods {
			g += p
		}
		in := SharePlanInput{
			Roles: []ShareRole{
				{Key: "A", Weight: 1, Need: 4, Ceiling: 64, ReplicaGPUs: 8},
				{Key: "B", Weight: 1, Need: 64, Ceiling: 64, ReplicaGPUs: g},
				{Key: "C", Weight: 1, Need: 4, Ceiling: 64, ReplicaGPUs: 8},
			},
			Held:       map[string]int{"A": 24, "B": 16, "C": 24},
			Thresholds: map[string]float64{"A": 0.8, "B": 0.8, "C": 0.8}, Budget: 64, Tolerance: 0.15,
			Give: map[string]ShareVariant{
				"A": {Name: "a", GPUs: 8, PodGPUs: []int{8}},
				"C": {Name: "c", GPUs: 8, PodGPUs: []int{8}},
			},
			Grow:       map[string]ShareVariant{"B": {Name: "b", GPUs: g, PodGPUs: receiverPods}},
			Nodes:      nodes,
			DonorUnits: map[string][]ShareUnit{"A": a, "C": c},
		}
		if mut != nil {
			mut(&in)
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
	full := func(names ...string) map[string]ShareNode {
		m := map[string]ShareNode{}
		for _, n := range names {
			m[n] = ShareNode{}
		}
		return m
	}
	planned := func(p SharePlan) []string {
		out := make([]string, 0, 2*len(p.Started))
		for _, t := range p.Started {
			out = append(out, t.PlannedPods...)
		}
		return out
	}

	It("never starts more transfers than the concurrency limit, whatever the donors", func() {
		// A 24-GPU pod needs three 8-GPU donor pods on one node: two from A
		// and one from C each pass their own donor's limits.
		p := plan([]int{24}, full("n1"),
			[]ShareUnit{pod8("ns/a-0", "n1"), pod8("ns/a-1", "n1")}, []ShareUnit{pod8("ns/c-0", "n1")}, nil)
		Expect(len(p.Started)).To(BeNumerically("<=", ShareMaxConcurrentTransfers))
		Expect(p.Started).To(BeEmpty(), "three units are needed, and only two may be in flight")
	})

	It("never takes a donor below its floor", func() {
		p := plan([]int{16}, full("n1"),
			[]ShareUnit{pod8("ns/a-0", "n1")}, []ShareUnit{pod8("ns/c-0", "n1")}, func(in *SharePlanInput) {
				in.Roles[0].Floor, in.Roles[2].Floor = 24, 24
			})
		Expect(p.Started).To(BeEmpty())
	})

	It("refuses a set that lowers the group's lowest score", func() {
		// A and C need every GPU they hold: giving makes them worse off than
		// B gains.
		p := plan([]int{16}, full("n1"),
			[]ShareUnit{pod8("ns/a-0", "n1")}, []ShareUnit{pod8("ns/c-0", "n1")}, func(in *SharePlanInput) {
				in.Roles[0].Need, in.Roles[2].Need, in.Roles[1].Need = 24, 24, 17
			})
		Expect(p.Started).To(BeEmpty())
	})

	It("funds a receiver pod from one LWS group whose smaller pods share its node, as an ordinary transfer", func() {
		// No 4-GPU pod holds the 8-GPU receiver pod pod-for-pod, so only the
		// node search funds it; one donor replica gives the whole replica.
		group := ShareUnit{Pods: []SharePod{{Name: "ns/a-0-0", Node: "n1", GPUs: 4}, {Name: "ns/a-0-1", Node: "n1", GPUs: 4}}}
		p := plan([]int{8}, full("n1", "n2"), []ShareUnit{group}, nil, func(in *SharePlanInput) {
			in.Give["A"] = ShareVariant{Name: "a", GPUs: 8, PodGPUs: []int{4, 4}}
			delete(in.Give, "C") // its 8-GPU pod would fund B pod-for-pod
		})
		Expect(p.Started).To(HaveLen(1))
		Expect(p.Started[0].SetID).To(BeEmpty(), "one donor replica is not a set")
		Expect(p.Started[0].Receiver).To(Equal("B"))
		Expect(p.Started[0].PlannedPods).To(ConsistOf("ns/a-0-0", "ns/a-0-1"))
	})

	It("credits a donor group spanning two nodes to each node's hole", func() {
		// Two 8-GPU receiver pods in one rack; one A group of two 4-GPU pods
		// per node on n1 and n2: neither node holds 8 alone without both of
		// its pods, which one group provides on each node only with the other.
		group := ShareUnit{Pods: []SharePod{
			{Name: "ns/a-0-0", Node: "n1", GPUs: 4}, {Name: "ns/a-0-1", Node: "n1", GPUs: 4},
			{Name: "ns/a-0-2", Node: "n2", GPUs: 4}, {Name: "ns/a-0-3", Node: "n2", GPUs: 4}}}
		p := plan([]int{8, 8}, full("n1", "n2"), []ShareUnit{group}, nil, func(in *SharePlanInput) {
			in.Give["A"] = ShareVariant{Name: "a", GPUs: 16, PodGPUs: []int{4, 4, 4, 4}}
			in.Roles[0].ReplicaGPUs, in.Roles[0].Need = 16, 4
			in.Held["A"], in.Budget = 32, 72
		})
		Expect(p.Started).To(HaveLen(1), "the second pod uses the hole the same group opened on n2")
		Expect(p.Started[0].PlannedPods).To(HaveLen(4))
	})

	It("tries every domain rather than the one its first pod would pick", func() {
		// r1 has one donor pod (on n1, first in name order); r2 has two.
		nodes := map[string]ShareNode{
			"n1": {Labels: map[string]string{"rack": "r1"}},
			"n2": {Labels: map[string]string{"rack": "r2"}},
			"n3": {Labels: map[string]string{"rack": "r2"}},
			"n4": {},
		}
		p := plan([]int{8, 8}, nodes,
			[]ShareUnit{pod8("ns/a-0", "n1"), pod8("ns/a-1", "n2"), pod8("ns/a-4", "n4")}, []ShareUnit{pod8("ns/c-0", "n3")},
			func(in *SharePlanInput) { in.DomainKey = map[string]string{"B": "rack"} })
		Expect(planned(p)).To(ConsistOf("ns/a-1", "ns/c-0"), "r2 holds both pods; n4 has no rack")
	})

	It("returns the node state its sets left, for the idle fill", func() {
		// B's two 4-GPU pods: one into n1's 4 free GPUs, one into the hole
		// a-0 opens on n2. n1's free GPUs are now B's.
		p := plan([]int{4, 4}, map[string]ShareNode{"n1": {Free: 4}, "n2": {}},
			[]ShareUnit{pod8("ns/a-0", "n2")}, nil, nil)
		Expect(planned(p)).To(ConsistOf("ns/a-0"))
		Expect(p.Nodes["n1"].Free).To(BeZero(), "the fill must not count n1's free GPUs again")
	})

	It("spends a started set's node state for the rest of the cycle", func() {
		nodes := map[string]ShareNode{"n1": {Free: 4}, "n2": {Free: 8}}
		units := map[string][]ShareUnit{"A": {pod8("ns/a-0", "n1"), pod8("ns/a-1", "n2")}}
		// The set took a-0, placing a 12-GPU pod on n1: 4 free + 8 donated.
		withdrawNodeSet(nodes, units, []shareDonor{{role: "A", planned: []string{"ns/a-0"}}}, map[string]int{"n1": 0, "n2": 8})
		Expect(nodes["n1"].Free).To(BeZero(), "the free GPUs went to this receiver")
		Expect(nodes["n2"].Free).To(Equal(8), "an untouched node keeps its free GPUs")
		Expect(units["A"]).To(HaveLen(1))
		Expect(units["A"][0].Pods[0].Name).To(Equal("ns/a-1"), "the set's donor pod is not offered again")
	})
})

var _ = Describe("ShareFitPods", func() {
	nodes := func() map[string]ShareNode {
		return map[string]ShareNode{
			"n1": {Free: 4, Labels: map[string]string{"rack": "r1"}},
			"n2": {Free: 2, Labels: map[string]string{"rack": "r2"}},
			"n3": {Free: 2, Labels: map[string]string{"rack": "r2"}},
		}
	}
	It("places largest first into the smallest node that holds each, and spends it", func() {
		n := nodes()
		Expect(ShareFitPods(n, []int{2, 4}, "")).To(BeTrue())
		Expect(n["n1"].Free + n["n2"].Free + n["n3"].Free).To(Equal(2))
		Expect(n["n1"].Free).To(BeZero(), "the 4-GPU pod takes the only node it fits")
	})
	It("spends nothing when a pod does not fit", func() {
		n := nodes()
		Expect(ShareFitPods(n, []int{4, 4}, "")).To(BeFalse())
		Expect(n).To(Equal(nodes()))
	})
	It("keeps a domain receiver in one domain, trying each", func() {
		// r1 (n1: 4) cannot hold a 3 and a 2; r2 (n2: 3, n3: 2) can.
		n := nodes()
		n["n2"] = ShareNode{Free: 3, Labels: map[string]string{"rack": "r2"}}
		Expect(ShareFitPods(n, []int{3, 2}, "rack")).To(BeTrue())
		Expect(n["n1"].Free).To(Equal(4), "r1 is untouched")
		Expect(n["n2"].Free + n["n3"].Free).To(BeZero())
		Expect(ShareFitPods(nodes(), []int{4, 2}, "rack")).To(BeFalse(), "no single rack holds 4 and 2")
		Expect(ShareFitPods(nodes(), []int{4, 2}, "")).To(BeTrue(), "without a domain, any nodes do (control)")
	})
})
