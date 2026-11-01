package allocation

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/accelerator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/decision"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/kueue"
)

// NodeView is one cycle's per-node picture of free GPUs, consumed in order: the
// optimizer debits every replica it orders from the node it fits, so the next
// replica, of this model or another, sees the hole that is left. A pool budget
// counts GPUs; a replica needs them on ONE node (per pod). Sixteen free GPUs as
// 4+4+8 hold one 8-GPU pod, not two.
//
// It is built whenever a physical limiter bounds the cycle (the physical
// provider was not skipped) and a fresh node snapshot exists. Without one -- no
// physical limiter, the provider skipped, no snapshot -- the view is nil and
// nothing is fitted: every method accepts a nil receiver, every placement then
// succeeds, and the copies are nil.
//
// It counts GPUs only. CPU or memory exhaustion on a node is not seen: a node
// is a candidate for a pod when it carries the pod's accelerator, is
// schedulable (decision.NodeGPU.Unschedulable: cordoned, not Ready, or
// tainted for removal), every NoSchedule or NoExecute taint it carries is
// tolerated by the pod's template (ReplicaShape.Tolerations), and its labels
// satisfy the templates' nodeSelector and required node affinity
// (ReplicaShape.Placements; eligibleFit, borrow_node_fit.go) -- on every
// path: the pass, a wake, and the in-flight ledger's holes. On borrowed
// capacity the flavor's node labels and tolerations count too
// (ReplicaShape.Flavor). A pod is placed best-fit, in the smallest hole that
// holds it; the pods of a replica with a topology key
// (ReplicaShape.DomainKey: an exclusive-topology LeaderWorkerSet) are placed
// in one domain, the nodes sharing one value of that label, and a domain
// holds one such replica: none lands in a domain where a LeaderWorkerSet
// group's pod is (decision.NodeGPU.LWSGroup) or where this view placed
// another (domains).
//
// A NodeView is not safe for concurrent use: one optimizer pass, or one wake
// decision, owns it.
//
// What a node is -- its accelerator, capacity, whether it is schedulable, its
// taints and labels -- does not change within a cycle; only what is debited
// from it does. So the nodes are read once, by NewNodeView, and shared
// read-only by every Clone, and so is the list of nodes each accelerator is
// eligible for (eligible): a clone copies only its debits, which are
// sparse. On a cluster of thousands of nodes a tentative placement then costs
// the nodes it touched, not a copy of every node.
//
// Every debit is charged to an owner -- the model being placed, or a holder
// of GPUs whose pods hold no node yet -- so a reader can take back what one
// owner placed (DebitsExcept, without): the utilization share plans the models
// it owns on the view less what today's optimizer placed for them.
type NodeView struct {
	// slots is the snapshot as built, in node-name order, shared by clones and
	// never written. A node is its index in it.
	slots []nodeSlot
	// debit is the GPUs this view placed on each node, on top of Used.
	debit map[int]int
	// elig caches eligible and largest, shared by clones (eligibility does not
	// depend on what is debited).
	elig *eligibleCache
	// owner is who a debit committed now is charged to, and owned what each
	// owner was charged per node. A clone (a tentative placement) records
	// nothing; adopting it charges the difference to the owner.
	owner string
	owned map[string]map[int]int
	// takenAt is when the node snapshot was taken.
	takenAt time.Time
	// layouts is where an unknown pod layout is logged once per variant.
	layouts *LayoutLog
	// domains is the topology domains (domainID) this view placed an
	// exclusive-topology replica in, each with the owner it was charged to:
	// a domain holds one group (WN1). A clone copies it.
	domains map[string]string
}

// nodeSlot is what the view reads of one node.
type nodeSlot struct {
	name          string
	accelerator   string
	capacity      int
	used          int
	unschedulable bool
	// taints is the node's NoSchedule and NoExecute taints a pod must
	// tolerate (decision.NodeGPU.Taints).
	taints []corev1.Taint
	// labels is the snapshot's own label map, read for a topology domain and
	// never written (decision.NodeGPUStore.Read).
	labels map[string]string
	// group is set when a LeaderWorkerSet group's pod is on the node
	// (decision.NodeGPU.LWSGroup).
	group bool
}

// eligibleCache is the nodes, in order, eligible for one accelerator and set of
// tolerations, and the largest node of each accelerator, computed once per
// cycle.
type eligibleCache struct {
	mu      sync.Mutex
	nodes   map[string][]int
	largest map[string]int
	// grouped is, per topology label key, the values of it whose nodes a
	// LeaderWorkerSet group's pod is on (groupedDomains).
	grouped map[string]map[string]bool
	// index is each node's position by name (slotOf).
	index map[string]int
}

// LayoutLog remembers the variants whose unknown pod layout a node view has
// logged, so each is logged once by its engine rather than once per cycle. An
// engine keeps one and hands it to every view it builds (WithLayoutLog); a view
// without one logs once per view.
type LayoutLog struct {
	seen sync.Map
}

// first reports whether variant is logged for the first time.
func (l *LayoutLog) first(variant string) bool {
	_, seen := l.seen.LoadOrStore(variant, true)
	return !seen
}

// NewNodeView builds a view over a node snapshot (decision.NodeGPUStore.Latest),
// or returns nil when there is none: without node information nothing is
// fitted. The snapshot's maps are not kept; its label maps are, read-only.
func NewNodeView(snap map[string]decision.NodeGPU) *NodeView {
	return newNodeViewAt(snap, time.Time{})
}

// newNodeViewAt is NewNodeView for a snapshot taken at takenAt.
func newNodeViewAt(snap map[string]decision.NodeGPU, takenAt time.Time) *NodeView {
	if snap == nil {
		return nil
	}
	v := &NodeView{slots: make([]nodeSlot, 0, len(snap)), debit: map[int]int{},
		elig: &eligibleCache{nodes: map[string][]int{}, largest: map[string]int{}}, owned: map[string]map[int]int{},
		takenAt: takenAt, layouts: &LayoutLog{}, domains: map[string]string{}}
	for _, name := range slices.Sorted(maps.Keys(snap)) {
		n := snap[name]
		v.slots = append(v.slots, nodeSlot{name: name, accelerator: n.Accelerator, capacity: n.Capacity, used: n.Used,
			unschedulable: n.Unschedulable, taints: n.Taints, labels: n.Labels, group: n.LWSGroup})
	}
	return v
}

// WithLayoutLog makes the view log an unknown pod layout once per variant in
// l, across every view that shares it, and returns v.
func (v *NodeView) WithLayoutLog(l *LayoutLog) *NodeView {
	if v != nil && l != nil {
		v.layouts = l
	}
	return v
}

// NodeViewFor is the node view for a decision bounded by constraints: built
// from store's snapshot when the physical limiter is among them, and nil
// otherwise. A physical limiter that was skipped this cycle (no node read, its
// usage unobserved, a failed refresh) is not among them, and its node fit is
// skipped with it; the skip is already reported as the provider's. reason is
// non-empty when the physical limiter bounds the decision but no node snapshot
// younger than decision.NodeGPUsMaxAge exists, so node fit is skipped: the
// caller reports it.
//
// The view holds what the in-flight ledger (DefaultInFlight) holds: the holes
// of replicas admitted earlier whose pods hold no node yet.
func NodeViewFor(constraints []*ResourceConstraints, store *decision.NodeGPUStore, now time.Time) (view *NodeView, reason string) {
	if !PhysicalBounds(constraints) {
		return nil, ""
	}
	if store == nil {
		store = decision.DefaultNodeGPUs
	}
	if !store.Read(decision.NodeGPUsMaxAge, now, func(snap map[string]decision.NodeGPU, at time.Time) {
		view = newNodeViewAt(snap, at)
	}) {
		return nil, "no node snapshot younger than " + decision.NodeGPUsMaxAge.String() +
			": replicas are fitted to the pool, not to nodes"
	}
	DefaultInFlight.holdOn(view, now)
	return view, ""
}

// PhysicalBounds reports whether the physical limiter's constraints are among
// constraints.
func PhysicalBounds(constraints []*ResourceConstraints) bool {
	for _, c := range constraints {
		if c != nil && c.ProviderName == PhysicalLimiterName {
			return true
		}
	}
	return false
}

// ReplicaShape is one replica as PlaceReplicas fits it.
type ReplicaShape struct {
	// Variant is "namespace/name", for the unknown-layout log.
	Variant     string
	Accelerator string
	// PodGPUs is each pod's GPUs, leader first; nil when unknown.
	PodGPUs []int
	// GPUs is the replica's GPUs in all.
	GPUs int
	// Tolerations are the pod template's; a node with a NoSchedule or
	// NoExecute taint they do not tolerate holds none of the replica's pods.
	// A LeaderWorkerSet's workers are judged by its leader's.
	Tolerations []corev1.Toleration
	// DomainKey is the node label every pod of the replica must share a value
	// of (an exclusive-topology LeaderWorkerSet), "" for none.
	DomainKey string
	// Placements is the pod templates' nodeSelector and required node
	// affinity (domain.VariantReplicaState.Placements): one entry for every
	// pod, or the leader's then the workers' (podTemplates); nil leaves them
	// unjudged. Set on every path that fits a replica: the steady-state pass,
	// the borrowing placer, a wake, and their holes in the in-flight ledger.
	Placements []domain.PodPlacement
	// Flavor is the Kueue flavor a borrowed replica is admitted on: its pods
	// land only on nodes carrying every node label it names, and carry its
	// tolerations beside the template's. Nil for a replica not borrowed.
	Flavor *kueue.FlavorPlacement
}

// replicaFit is one replica as the view places it: its pods' GPUs, the
// accelerator, the tolerations and the topology key, and for a replica the
// borrowing placer orders its templates' placement and its flavor's.
type replicaFit struct {
	acc         string
	pods        []int
	tolerations []corev1.Toleration
	domain      string
	templates   []domain.PodPlacement
	flavor      *kueue.FlavorPlacement
}

// fitOf is s as the view places it (replicaPods for its pods).
func (v *NodeView) fitOf(s ReplicaShape) replicaFit {
	return replicaFit{acc: s.Accelerator, pods: v.replicaPods(s.Variant, s.Accelerator, s.PodGPUs, s.GPUs),
		tolerations: s.Tolerations, domain: s.DomainKey, templates: s.Placements, flavor: s.Flavor}
}

// PlaceReplicas reports whether every replica fits the nodes together, each on
// what the ones before it left, and debits them all when commit is set and
// they do. A nil view fits everything.
func (v *NodeView) PlaceReplicas(reps []ReplicaShape, commit bool) bool {
	if v == nil {
		return true
	}
	trial := v.Clone()
	for _, r := range reps {
		if !trial.place(trial.fitOf(r)) {
			return false
		}
	}
	if commit {
		v.adopt(trial)
	}
	return true
}

// Clone returns an independent copy, for a pass that must not consume the
// cycle's view (a tentative placement). It copies only the debits; the nodes
// and the eligibility cache are shared, read-only. A clone records no owners:
// what it placed is charged when the view adopts it.
func (v *NodeView) Clone() *NodeView {
	if v == nil {
		return nil
	}
	return &NodeView{slots: v.slots, debit: maps.Clone(v.debit), elig: v.elig,
		takenAt: v.takenAt, layouts: v.layouts, domains: maps.Clone(v.domains)}
}

// adopt takes another view's debits: the commit of a tentative placement made
// on a Clone of v. What the clone added -- its debits and the topology domains
// it took -- is charged to v's owner as of the adopt, not as of the Clone: a
// caller may set the owner after cloning, just before it adopts (rescale), and
// a clone records no owner of its own. The domains are charged with the
// debits, so without and ReleaseUncreated take both back together. c must be a
// Clone of v from which v has released nothing since (without,
// ReleaseUncreated): a domain v gave back in between would be taken again
// here, under the adopting owner. Every caller adopts the clone it has just
// placed on.
func (v *NodeView) adopt(c *NodeView) {
	if v == nil || c == nil {
		return
	}
	if v.owned != nil {
		for i, g := range c.debit {
			if d := g - v.debit[i]; d != 0 {
				v.record(i, d)
			}
		}
	}
	v.debit = c.debit
	for id := range c.domains {
		if _, ok := v.domains[id]; !ok {
			v.takeDomain(id)
		}
	}
}

// charge debits g GPUs of node i, charged to the view's owner.
func (v *NodeView) charge(i, g int) {
	v.debit[i] += g
	if v.owned != nil {
		v.record(i, g)
	}
}

// record charges g GPUs of node i to the view's owner.
func (v *NodeView) record(i, g int) {
	perNode := v.owned[v.owner]
	if perNode == nil {
		perNode = map[int]int{}
		v.owned[v.owner] = perNode
	}
	perNode[i] += g
}

// setOwner charges every debit committed from now on to owner, and returns
// the previous owner.
func (v *NodeView) setOwner(owner string) string {
	if v == nil {
		return ""
	}
	prev := v.owner
	v.owner = owner
	return prev
}

// DebitsExcept is the GPUs the view debited per node for every owner exclude
// does not name. Nil for a nil view.
func (v *NodeView) DebitsExcept(exclude func(owner string) bool) map[string]int {
	if v == nil {
		return nil
	}
	out := map[string]int{}
	for owner, perNode := range v.owned {
		if exclude != nil && exclude(owner) {
			continue
		}
		for i, g := range perNode {
			out[v.slots[i].name] += g
		}
	}
	return out
}

// without is a copy of v with what the owners exclude names were charged
// taken back. Nil for a nil view.
func (v *NodeView) without(exclude func(owner string) bool) *NodeView {
	if v == nil {
		return nil
	}
	c := v.Clone()
	for owner, perNode := range v.owned {
		if !exclude(owner) {
			continue
		}
		for i, g := range perNode {
			if c.debit[i] -= g; c.debit[i] <= 0 {
				delete(c.debit, i)
			}
		}
	}
	maps.DeleteFunc(c.domains, func(_, owner string) bool { return exclude(owner) })
	return c
}

// PromisedOwner is the owner the GPUs the utilization share promised to a
// receiver are charged to (ReservePromised).
const PromisedOwner = "promised"

// ReservePromised holds promised GPUs (scope -> accelerator -> holes, as
// decision.LatestSharePromisedHoles returns them) on the view's nodes, charged
// to PromisedOwner: a node freed and held for a receiver whose pod is still
// Pending looks free in the snapshot, and neither the optimizer nor a wake
// nor the warm pool may place into it. A hole whose node is known is held on
// that node: the receiver's pod waits for it there. Held on the freest node
// instead, a 1-GPU hole on a nearly full node made a wholly free node look
// short of one, and left the hole itself open to another pod. What a hole's
// node no longer shows free, or a node the view does not count for the
// accelerator, joins the holes of no known node, and those are taken from the
// freest nodes (Reserve), after every known hole of every scope is placed.
// Mirrors the share planner's own node picture (shareFreeNodesAfter).
func ReservePromised(v *NodeView, promised map[string]map[string]decision.PromisedHoles) {
	if v == nil {
		return
	}
	type rest struct {
		acc  string
		gpus int
	}
	var unplaced []rest
	for _, scope := range slices.Sorted(maps.Keys(promised)) {
		for _, acc := range slices.Sorted(maps.Keys(promised[scope])) {
			h := promised[scope][acc]
			unplaced = append(unplaced, rest{acc, h.Unplaced + v.reserveOn(PromisedOwner, acc, h.Nodes)})
		}
	}
	for _, r := range unplaced {
		v.Reserve(PromisedOwner, r.acc, r.gpus)
	}
}

// reserveOn debits, charged to owner, up to perNode[name] GPUs of accelerator
// acc from each named node the view counts for acc, as far as it has them
// free, and returns what it could not debit.
func (v *NodeView) reserveOn(owner, acc string, perNode map[string]int) int {
	if v == nil || len(perNode) == 0 {
		return 0
	}
	prev := v.setOwner(owner)
	defer v.setOwner(prev)
	eligible := v.eligible(acc, nil, true)
	left := 0
	for _, name := range slices.Sorted(maps.Keys(perNode)) {
		g := perNode[name]
		if g <= 0 {
			continue
		}
		i, ok := slices.BinarySearchFunc(v.slots, name, func(s nodeSlot, n string) int { return strings.Compare(s.name, n) })
		if !ok || !slices.Contains(eligible, i) {
			left += g
			continue
		}
		take := min(v.free(i), g)
		if take > 0 {
			v.charge(i, take)
		}
		left += g - take
	}
	return left
}

// Reserve debits gpus GPUs of accelerator acc, charged to owner, from the
// nodes with the most free first, each in one piece: GPUs held for a receiver
// whose pods hold no node yet (promised by the utilization-share optimizer)
// are free on some node, which one is not known, and a freed whole node is
// where whole-node pods would land. Taken a GPU at a time across nodes, eight
// promised over two free 8-GPU nodes would leave four on each, and a
// genuinely free node would stop being placeable. Mirrors the share planner's
// own reserve (shareFreeNodes).
func (v *NodeView) Reserve(owner, acc string, gpus int) {
	if v == nil || gpus <= 0 {
		return
	}
	prev := v.setOwner(owner)
	defer v.setOwner(prev)
	eligible := v.eligible(acc, nil, true)
	for gpus > 0 {
		most, mostFree := -1, 0
		for _, i := range eligible {
			if f := v.free(i); f > mostFree {
				most, mostFree = i, f
			}
		}
		if most < 0 {
			return
		}
		take := min(mostFree, gpus)
		v.charge(most, take)
		gpus -= take
	}
}

// free is the GPUs of node i not used nor debited by this view.
func (v *NodeView) free(i int) int {
	n := &v.slots[i]
	f := n.capacity - n.used
	if len(v.debit) > 0 {
		f -= v.debit[i]
	}
	return max(0, f)
}

// Holes returns, per node accelerator, the free GPUs of each schedulable node
// with any free, after this view's debits: what the warm pool may still place
// into. Nil for a nil view.
func (v *NodeView) Holes() map[string]decision.NodeHoles {
	if v == nil {
		return nil
	}
	out := map[string]decision.NodeHoles{}
	for i := range v.slots {
		n := &v.slots[i]
		if n.unschedulable {
			continue
		}
		h := out[n.accelerator]
		if f := v.free(i); f > 0 {
			h.Free = append(h.Free, f)
		}
		out[n.accelerator] = h
	}
	return out
}

// eligible is, in order, the nodes a pod of accelerator acc with these
// tolerations may land on (anyTaint: whatever its taints), computed once per
// cycle, accelerator and set of tolerations.
func (v *NodeView) eligible(acc string, tolerations []corev1.Toleration, anyTaint bool) []int {
	key := eligibleKey(acc, tolerations, anyTaint)
	v.elig.mu.Lock()
	defer v.elig.mu.Unlock()
	if nodes, ok := v.elig.nodes[key]; ok {
		return nodes
	}
	var nodes []int
	for i := range v.slots {
		if eligibleNode(&v.slots[i], acc, tolerations, anyTaint) {
			nodes = append(nodes, i)
		}
	}
	v.elig.nodes[key] = nodes
	return nodes
}

// eligibleKey is eligible's cache key.
func eligibleKey(acc string, tolerations []corev1.Toleration, anyTaint bool) string {
	if anyTaint {
		return acc + "\x00*"
	}
	if len(tolerations) == 0 {
		return acc
	}
	var b strings.Builder
	b.WriteString(acc)
	for _, t := range tolerations {
		b.WriteString("\x00" + t.Key + "\x01" + string(t.Operator) + "\x01" + t.Value + "\x01" + string(t.Effect))
	}
	return b.String()
}

// eligibleNode reports whether a pod of accelerator acc may land on n: n is
// schedulable, carries acc (accelerator.SameName, so a family name lands on
// any of its products; a pod that names no accelerator lands on any GPU node),
// and, unless anyTaint, tolerations tolerate every taint n carries.
func eligibleNode(n *nodeSlot, acc string, tolerations []corev1.Toleration, anyTaint bool) bool {
	if n.unschedulable {
		return false
	}
	if constants.IsAcceleratorResolved(acc) && !accelerator.SameName(n.accelerator, acc) {
		return false
	}
	if anyTaint {
		return true
	}
	// The numeric Lt/Gt toleration operators stay off, as before k8s.io/api
	// v0.36 made them a parameter: such a toleration tolerates nothing here.
	// With them off the logger is never used.
	for i := range n.taints {
		if !slices.ContainsFunc(tolerations, func(t corev1.Toleration) bool {
			return t.ToleratesTaint(logr.Discard(), &n.taints[i], false)
		}) {
			return false
		}
	}
	return true
}

// NodeAdmits reports whether a pod of accelerator acc whose template carries
// tolerations may land on n, by the node view's own rule (eligibleNode): n is
// schedulable, carries acc (accelerator.SameName), and every taint it
// carries is tolerated. The utilization share's node picture uses it, so the
// share and the view agree on which nodes a pod may use.
func NodeAdmits(n decision.NodeGPU, acc string, tolerations []corev1.Toleration) bool {
	return eligibleNode(&nodeSlot{accelerator: n.Accelerator, unschedulable: n.Unschedulable, taints: n.Taints},
		acc, tolerations, false)
}

// largest is the capacity of the largest node acc is eligible for, whatever
// its taints: how large a replica one node could hold.
func (v *NodeView) largest(acc string) int {
	v.elig.mu.Lock()
	most, ok := v.elig.largest[acc]
	v.elig.mu.Unlock()
	if ok {
		return most
	}
	for _, i := range v.eligible(acc, nil, true) {
		most = max(most, v.slots[i].capacity)
	}
	v.elig.mu.Lock()
	v.elig.largest[acc] = most
	v.elig.mu.Unlock()
	return most
}

// place fits one replica's pods onto the nodes they are eligible for (podFits:
// a LeaderWorkerSet's leader by its template, its workers by theirs), largest
// pod first, each best-fit -- into the smallest hole that holds it, the first
// such node in name order on a tie -- and debits them.
// With a topology key and more than one pod, every pod lands in one domain:
// the domains are tried in label-value order and the first that holds them
// all is used; a node without the label is in none. Reports whether every
// pod fit; nothing is debited otherwise.
func (v *NodeView) place(r replicaFit) bool {
	if v == nil {
		return true
	}
	pods := v.podFits(r)
	if len(pods) == 0 {
		return true
	}
	domains := []string{""}
	exclusive := r.domain != "" && len(pods) > 1
	if exclusive {
		var eligible []int
		for _, p := range pods {
			eligible = append(eligible, p.nodes...)
		}
		domains = v.domainsOf(eligible, r.domain)
	}
	for _, d := range domains {
		if exclusive && v.domainTaken(r.domain, d) {
			continue // a domain holds one group (WN1)
		}
		taken, ok := v.bestFit(pods, r.domain, d)
		if !ok {
			continue
		}
		for _, i := range slices.Sorted(maps.Keys(taken)) {
			v.charge(i, taken[i])
		}
		if exclusive {
			v.takeDomain(domainID(r.domain, d))
		}
		return true
	}
	return false
}

// domainID names value d of topology label key in NodeView.domains.
func domainID(key, d string) string { return key + "\x00" + d }

// takeDomain charges topology domain id to the view's owner.
func (v *NodeView) takeDomain(id string) {
	if v.domains == nil {
		v.domains = map[string]string{}
	}
	v.domains[id] = v.owner
}

// domainTaken reports whether value d of topology label key already holds a
// group: this view placed an exclusive-topology replica in it, or a
// LeaderWorkerSet group's pod is on one of its nodes. LeaderWorkerSet's
// exclusive placement is a pod anti-affinity against every other group's
// pods in the domain, so either refuses a new group there.
func (v *NodeView) domainTaken(key, d string) bool {
	if _, ok := v.domains[domainID(key, d)]; ok {
		return true
	}
	return v.groupedDomains(key)[d]
}

// groupedDomains is the values of label key whose nodes a LeaderWorkerSet
// group's pod is on, computed once per view and key and shared by clones.
func (v *NodeView) groupedDomains(key string) map[string]bool {
	v.elig.mu.Lock()
	defer v.elig.mu.Unlock()
	if g, ok := v.elig.grouped[key]; ok {
		return g
	}
	g := map[string]bool{}
	for i := range v.slots {
		if n := &v.slots[i]; n.group && n.labels[key] != "" {
			g[n.labels[key]] = true
		}
	}
	if v.elig.grouped == nil {
		v.elig.grouped = map[string]map[string]bool{}
	}
	v.elig.grouped[key] = g
	return g
}

// slotOf is the index of the node named name, false when the snapshot has
// none.
func (v *NodeView) slotOf(name string) (int, bool) {
	v.elig.mu.Lock()
	defer v.elig.mu.Unlock()
	if v.elig.index == nil {
		v.elig.index = make(map[string]int, len(v.slots))
		for i := range v.slots {
			v.elig.index[v.slots[i].name] = i
		}
	}
	i, ok := v.elig.index[name]
	return i, ok
}

// bestFit places pods, in order, each into the smallest hole among the nodes
// it may land on (only nodes whose label key is domain, when domain is set)
// that holds it, and returns the GPUs taken per node, or false when some pod
// fits nowhere.
func (v *NodeView) bestFit(pods []podFit, key, domain string) (map[int]int, bool) {
	var taken map[int]int
	for _, p := range pods {
		need := p.gpus
		best, bestFree := -1, 0
		for _, i := range p.nodes {
			if domain != "" && v.slots[i].labels[key] != domain {
				continue
			}
			f := v.free(i) - taken[i]
			if f < need || (best >= 0 && f >= bestFree) {
				continue
			}
			best, bestFree = i, f
			if f == need {
				break // nothing fits tighter
			}
		}
		if best < 0 {
			return nil, false
		}
		if taken == nil {
			taken = map[int]int{}
		}
		taken[best] += need
	}
	return taken, true
}

// domainsOf is the values of label key among nodes, sorted.
func (v *NodeView) domainsOf(nodes []int, key string) []string {
	var out []string
	for _, i := range nodes {
		if d := v.slots[i].labels[key]; d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

// fits reports how many of up to n replicas r fit the nodes together, without
// debiting anything.
func (v *NodeView) fits(r replicaFit, n int) int {
	if v == nil || n <= 0 {
		return n
	}
	if len(r.pods) == 1 {
		// One pod per replica: whole pods per node, summed.
		need := r.pods[0]
		if need <= 0 {
			return n
		}
		count := 0
		for _, i := range v.eligibleFor(r, podTemplates(r, 0)) {
			count += v.free(i) / need
			if count >= n {
				return n
			}
		}
		return count
	}
	trial := v.Clone()
	for i := range n {
		if !trial.place(r) {
			return i
		}
	}
	return n
}

// replicaPods is one replica's pods as GPUs per pod: the variant's measured
// layout when it sums to the replica, else one pod holding the whole replica.
//
// A replica whose layout is unknown and whose GPUs no single node of its
// accelerator holds (a multi-node group, a LeaderWorkerSet whose layout was not
// read) is not judged per node at all: nil, and only the pool budget bounds
// it. Placing it as one pod would hold it back on every cluster, which is the
// opposite of what an unknown layout says. It is logged once per variant
// (variant is "namespace/name") in the view's LayoutLog.
func (v *NodeView) replicaPods(variant, acc string, podGPUs []int, gpusPerReplica int) []int {
	sum := 0
	for _, g := range podGPUs {
		sum += g
	}
	if len(podGPUs) > 0 && sum == gpusPerReplica {
		return podGPUs
	}
	if v != nil && gpusPerReplica > v.largest(acc) {
		if v.layouts.first(variant) {
			ctrl.Log.WithName("allocation").V(1).Info(
				"Pod layout of a replica unknown and larger than any node: not fitting it to nodes",
				"variant", variant, "gpusPerReplica", gpusPerReplica, "podGPUs", podGPUs)
		}
		return nil
	}
	return []int{gpusPerReplica}
}
