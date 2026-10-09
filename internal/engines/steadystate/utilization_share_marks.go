package steadystate

// Donor-pod marks: steering which pod a donor gives, checking it went, and
// rebuilding the ledger from the marks after a restart (proposal sections
// 6.3 and 6.5).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/variantmeta"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// transferMark is a transfer as its donor pods record it.
type transferMark struct {
	ID              string    `json:"id"`
	Donor           string    `json:"donor"`
	Receiver        string    `json:"receiver"`
	DonorVariant    string    `json:"donorVariant"`
	ReceiverVariant string    `json:"receiverVariant"`
	GPUs            int       `json:"gpus"`
	DonorGPUs       int       `json:"donorGPUs"`
	Started         time.Time `json:"started"`
	// SetID links the members of a donor set (section 6.5).
	SetID string `json:"setID,omitempty"`
	// Planned is set when a node-aware plan chose exactly the marked pods:
	// the transfer then checks that they are the ones that went.
	Planned bool `json:"planned,omitempty"`
	// PrevCost is the pod's deletion cost before the mark, restored when the
	// mark is removed; nil when it had none.
	PrevCost *string `json:"prevCost,omitempty"`
	// Instance is the CONTROLLER_INSTANCE that wrote the mark: a controller
	// sweeps only its own marks (sweepShareMarks).
	Instance string `json:"instance,omitempty"`
	// Cost is the deletion cost the mark wrote; empty is donorDeletionCost.
	Cost string `json:"cost,omitempty"`
}

// ownCost is the deletion cost this mark wrote on its pod.
func (m transferMark) ownCost() string {
	if m.Cost == "" {
		return donorDeletionCost
	}
	return m.Cost
}

// donorPods lists the pods a donor variant would release, and refuses a donor
// whose release cannot be steered.
//
// On a Deployment: its active pods -- not finished (Succeeded, Failed), not
// being deleted. A pod not yet scheduled refuses the donor: the ReplicaSet
// removes it before it reads any cost. So does a rollout -- by the
// Deployment's status, or pods of more than one ReplicaSet: the Deployment
// controller splits a scale-down across them, and a cost ranks pods only
// within one. So do fewer active pods than spec.replicas: lowering it by one
// removes nothing, since the ReplicaSet removes only pods above the count.
//
// On a LeaderWorkerSet: the pods of the group LWS removes next. LWS removes
// the highest indices first and does not steer by cost, so that is the
// highest group still below spec.replicas (those above are already going) that
// no live transfer has marked (marked groups are earlier transfers', and go
// first). Every pod its StatefulSets own counts, terminating or not. That
// group terminating or not yet scheduled refuses the donor: below
// spec.replicas it is a rollout, not a release. So does a rolling update,
// whose surge groups sit above spec.replicas and stay until it ends, and a
// group below spec.replicas with no pods: the StatefulSet removes the highest
// ordinal, existing or not. A pod that only carries the LWS's labels is not
// its own, and is ignored. marked may be nil.
func (e *Engine) donorPods(ctx context.Context, acc scaletarget.ScaleTargetAccessor, namespace string,
	marked func(*corev1.Pod) bool) ([]corev1.Pod, error) {
	if scaletarget.RollingOut(acc) {
		return nil, fmt.Errorf("%s is mid-rollout; its victim cannot be steered", acc.GetName())
	}
	replicas := -1
	if r := acc.GetReplicas(); r != nil {
		replicas = int(*r)
	}
	if !scaletarget.IsLeaderWorkerSet(acc) {
		// By the Deployment's selector, every ReplicaSet generation: a
		// rollout that changed a template label would otherwise hide the old
		// ReplicaSet's pods from the rollout check below.
		var list []corev1.Pod
		var err error
		if sel := scaletarget.LabelSelector(acc); sel != nil {
			list, err = variantmeta.ListDeploymentPods(ctx, e.client, namespace, acc.GetName(), sel)
		} else {
			list, err = variantmeta.ListVariantPods(ctx, e.client, namespace, acc)
		}
		if err != nil {
			return nil, err
		}
		return deploymentDonorPods(acc.GetName(), replicas, list)
	}
	list, err := variantmeta.ListVariantPods(ctx, e.client, namespace, acc)
	if err != nil {
		return nil, err
	}
	return lwsDonorPods(acc.GetName(), replicas, list, marked)
}

// deploymentDonorPods is the active pods of a Deployment's every ReplicaSet,
// refusing an unscheduled pod, a rollout or a missing pod (donorPods).
// replicas is the Deployment's spec.replicas, or -1 when unknown.
func deploymentDonorPods(name string, replicas int, list []corev1.Pod) ([]corev1.Pod, error) {
	var pods []corev1.Pod
	replicaSets := map[string]bool{}
	for _, p := range list {
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Spec.NodeName == "" {
			return nil, fmt.Errorf("%s has a pod not yet scheduled (%s); its victim cannot be steered", name, p.Name)
		}
		if owner := metav1.GetControllerOf(&p); owner != nil {
			replicaSets[owner.Name] = true
		}
		pods = append(pods, p)
	}
	if len(replicaSets) > 1 {
		return nil, fmt.Errorf("%s is mid-rollout (pods of %d ReplicaSets); its victim cannot be steered",
			name, len(replicaSets))
	}
	// More active pods than spec.replicas is a scale-down already under way;
	// fewer is a pod the ReplicaSet cannot create (a quota, an eviction), and
	// lowering the count would remove none.
	if replicas >= 0 && len(pods) < replicas {
		return nil, fmt.Errorf("%s has %d of its %d pods; lowering its count would remove none",
			name, len(pods), replicas)
	}
	return pods, nil
}

// lwsDonorPods is the group of an LWS's own pods that LWS removes next
// (donorPods). replicas is the LWS's spec.replicas, or -1 when unknown.
func lwsDonorPods(name string, replicas int, list []corev1.Pod, marked func(*corev1.Pod) bool) ([]corev1.Pod, error) {
	groups := map[int][]corev1.Pod{}
	for _, p := range list {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		// An LWS's pods are its StatefulSets': a bare pod that only carries its
		// label -- one a tenant made to stay Pending -- is not the workload's.
		if owner := metav1.GetControllerOf(&p); owner == nil || owner.Kind != constants.StatefulSetKind {
			continue
		}
		if i, err := strconv.Atoi(p.Labels[lwsv1.GroupIndexLabelKey]); err == nil {
			groups[i] = append(groups[i], p)
		}
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no pod of %s carries a group index", name)
	}
	for i := range max(replicas, 0) {
		if len(groups[i]) == 0 {
			return nil, fmt.Errorf("%s has no pod of group %d; lowering its count would remove none", name, i)
		}
	}
	revisions := map[string]bool{}
	for _, pods := range groups {
		for _, p := range pods {
			revisions[p.Labels[lwsv1.RevisionKey]] = true
		}
	}
	if len(revisions) > 1 {
		return nil, fmt.Errorf("%s is mid-rollout (groups of %d revisions); its release cannot be steered", name, len(revisions))
	}
	indices := slices.Sorted(maps.Keys(groups))
	slices.Reverse(indices)
	for _, i := range indices {
		if replicas >= 0 && i >= replicas {
			continue // above spec.replicas: already being removed
		}
		pods := groups[i]
		if marked != nil && slices.ContainsFunc(pods, func(p corev1.Pod) bool { return marked(&p) }) {
			continue // an earlier live transfer's: it goes first
		}
		for _, p := range pods {
			if p.DeletionTimestamp != nil || p.Spec.NodeName == "" {
				return nil, fmt.Errorf("%s's group %d, the next LWS removes, is terminating or not scheduled (%s); its release cannot be steered",
					name, i, p.Name)
			}
		}
		return pods, nil
	}
	return nil, fmt.Errorf("%s: %w", name, errDonorExhausted)
}

// errDonorExhausted is a donor whose every pod or group a live transfer has
// already marked: nothing is wrong with it, it has simply given all it can
// right now. It is held from giving for one release timeout
// (ShareLedger.HoldGiving), so its receiver tries another donor, and it does
// not back off and is not reported as unsteerable.
var errDonorExhausted = errors.New("every pod the donor could give is already given to a live transfer")

// podDeletionCost is a pod's deletion cost, 0 when it has none or an invalid
// one, as the ReplicaSet reads it.
func podDeletionCost(p *corev1.Pod) int64 {
	v, err := strconv.ParseInt(p.Annotations[podDeletionCostAnnotation], 10, 32)
	if err != nil {
		return 0
	}
	return v
}

// markDonorPods marks the pod a transfer's donor gives: the lowest deletion
// cost, so the ReplicaSet removes it and not a sibling, and the transfer
// annotation, so a restarted controller finds it (§6.3, §6.5). It prefers a
// Ready pod: the ReplicaSet removes not-ready pods before it consults the
// cost, so a donor with one cannot be steered.
//
// marked reports whether a pod already belongs to another transfer: a live
// one in the ledger, or one marked earlier this cycle -- the client reads
// through a cache that may not show that patch yet.
//
// private writes a mark that names no receiver and no set: the transfer, or
// the donor set it belongs to, crosses a namespace, and the mark sits on a
// tenant's pod. Restored, each such mark is a release with no receiver.
//
// It returns the pods it marked as the API server returned them: a rollback in
// the same cycle unmarks those, not a cached read that does not show the mark
// yet.
func (e *Engine) markDonorPods(ctx context.Context, t allocation.ShareTransfer,
	acc scaletarget.ScaleTargetAccessor, namespace string, marked func(*corev1.Pod) bool, private bool) ([]corev1.Pod, error) {
	if e.client == nil || acc == nil {
		return nil, fmt.Errorf("no client or scale target for donor variant %q", t.DonorVariant)
	}
	pods, err := e.donorPods(ctx, acc, namespace, marked)
	if err != nil {
		return nil, err
	}
	if !scaletarget.IsLeaderWorkerSet(acc) {
		for _, p := range pods {
			if !variantmeta.PodReady(&p) {
				return nil, fmt.Errorf("donor variant %q has a pod that is not ready; its victim cannot be steered", t.DonorVariant)
			}
		}
	}
	// A pod another transfer has marked is that transfer's: a second
	// concurrent transfer from the same donor gives a different pod, or none.
	// A mark no live transfer owns -- forged in a pod template, or left by a
	// transfer long gone -- holds nothing back.
	live := slices.Clone(pods)
	pods = slices.DeleteFunc(pods, func(p corev1.Pod) bool { return marked(&p) })
	live = slices.DeleteFunc(live, func(p corev1.Pod) bool { return !marked(&p) })
	if len(pods) == 0 {
		return nil, fmt.Errorf("donor variant %q: %w", t.DonorVariant, errDonorExhausted)
	}
	unmarked := slices.Clone(pods)
	if len(t.PlannedPods) > 0 {
		// A node-aware plan placed its holes where these pods run: exactly
		// they go, or the transfer does not start.
		pods = slices.DeleteFunc(pods, func(p corev1.Pod) bool {
			return !slices.Contains(t.PlannedPods, utils.GetNamespacedKey(p.Namespace, p.Name))
		})
		if len(pods) != len(t.PlannedPods) {
			return nil, fmt.Errorf("donor variant %q: %d of the %d planned pods are gone, terminating or already marked",
				t.DonorVariant, len(t.PlannedPods)-len(pods), len(t.PlannedPods))
		}
	} else if !scaletarget.IsLeaderWorkerSet(acc) {
		// The least protected pod: the lowest deletion cost a user set, then
		// the most recently Ready -- the coldest cache, and the ReplicaSet's
		// own preference absent a cost -- then the name. Only a preference:
		// the cost below makes it the one removed.
		slices.SortFunc(pods, func(a, b corev1.Pod) int {
			return cmp.Or(cmp.Compare(podDeletionCost(&a), podDeletionCost(&b)),
				podReadySince(&b).Compare(podReadySince(&a)), cmp.Compare(a.Name, b.Name))
		})
		pods = pods[:1]
	}
	// Below every unmarked sibling that stays, and above every live mark of
	// this donor: the ledger releases transfers in start order, so the
	// ReplicaSet must remove the earlier transfer's pod first. A tie leaves
	// the order to the ReplicaSet's own tie-break, and a planned transfer
	// whose pod lost it would end wrong-pod.
	siblings := slices.DeleteFunc(unmarked, func(p corev1.Pod) bool {
		return slices.ContainsFunc(pods, func(q corev1.Pod) bool { return q.Name == p.Name })
	})
	cost, ok := markCost(siblings, live)
	if !ok && !scaletarget.IsLeaderWorkerSet(acc) {
		return nil, fmt.Errorf("donor variant %q: a sibling's deletion cost leaves no room below it; its victim cannot be steered", t.DonorVariant)
	}
	var done []corev1.Pod
	for i := range pods {
		p := &pods[i]
		mark := transferMark{ID: t.ID, Donor: t.Donor, Receiver: t.Receiver, DonorVariant: t.DonorVariant,
			ReceiverVariant: t.ReceiverVariant, GPUs: t.GPUs, DonorGPUs: t.DonorGPUs, Started: t.Started, SetID: t.SetID,
			Planned: len(t.PlannedPods) > 0, Instance: metrics.GetControllerInstance()}
		if private {
			// The mark sits on the donor tenant's pod: it must not name
			// another tenant's model, nor carry a set link that names another
			// tenant's transfer. Restored, it is a release with no receiver,
			// and the receiver is planned again.
			mark.Receiver, mark.ReceiverVariant, mark.GPUs, mark.SetID = "", "", 0, ""
			mark.DonorGPUs = t.DonorGPUs
		}
		mark.Cost = cost
		// The pod's own deletion cost, restored with the mark's removal. One a
		// stale mark of ours wrote is not the user's -- unless the user has
		// set another since.
		if prev, ok := p.Annotations[podDeletionCostAnnotation]; ok {
			var old transferMark
			if json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &old) == nil &&
				old.ID != "" && prev == old.ownCost() {
				mark.PrevCost = old.PrevCost
			} else {
				mark.PrevCost = &prev
			}
		}
		raw, err := json.Marshal(mark)
		if err != nil {
			return done, err
		}
		patch := client.MergeFrom(p.DeepCopy())
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[podDeletionCostAnnotation] = cost
		p.Annotations[utilizationShareTransferAnnotation] = string(raw)
		if err := e.client.Patch(ctx, p, patch); err != nil {
			return done, err
		}
		done = append(done, *p)
	}
	return done, nil
}

// podKeys are the pods' namespace/name keys.
func podKeys(pods []corev1.Pod) []string {
	keys := make([]string, 0, len(pods))
	for i := range pods {
		keys = append(keys, utils.GetNamespacedKey(pods[i].Namespace, pods[i].Name))
	}
	return keys
}

// unmarkDonorPods removes a cancelled or aborted transfer's marks, so the pods
// are no longer first in line and no restart resurrects the transfer.
//
// It reports how many pods it could not unmark; callers that retry use it.
func (e *Engine) unmarkDonorPods(ctx context.Context, logger logr.Logger, t allocation.ShareTransfer) (failed int) {
	if e.client == nil {
		return 0
	}
	for _, key := range t.DonorPods {
		ns, name, _ := strings.Cut(key, "/")
		var p corev1.Pod
		if err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
			if !apierrors.IsNotFound(err) {
				// Not "gone": the mark stays, and a restart inside the
				// release timeout would restore the transfer. Say so.
				logger.Error(err, "Utilization share: could not read a donor pod to unmark it", "pod", key)
				failed++
			}
			continue
		}
		if t.ID != "" && !markedFor(&p, t.ID) {
			continue // marked for another transfer since: not this one's to clear
		}
		if err := e.unmarkPod(ctx, &p); err != nil {
			logger.Error(err, "Utilization share: could not unmark a donor pod", "pod", key)
			failed++
		}
	}
	return failed
}

// unmarkPod removes the transfer mark from p, as given: the transfer
// annotation, and our deletion cost -- giving back the cost the pod had before
// the mark, unless someone set another since.
func (e *Engine) unmarkPod(ctx context.Context, p *corev1.Pod) error {
	patch := client.MergeFrom(p.DeepCopy())
	var m transferMark
	_ = json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m)
	switch {
	case p.Annotations[podDeletionCostAnnotation] != m.ownCost():
		// Set since the mark -- by the user, or another controller: theirs.
	case m.PrevCost != nil && validDeletionCost(*m.PrevCost):
		p.Annotations[podDeletionCostAnnotation] = *m.PrevCost // the user's, back
	default:
		delete(p.Annotations, podDeletionCostAnnotation)
	}
	delete(p.Annotations, utilizationShareTransferAnnotation)
	return e.client.Patch(ctx, p, patch)
}

// markedFor reports whether a pod's transfer mark names the transfer id.
func markedFor(p *corev1.Pod, id string) bool {
	var m transferMark
	return json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m) == nil && m.ID == id
}

// restoreShareTransfers rebuilds a group's Releasing transfers from its donor
// pods' marks after a restart (§6.3).
//
// A mark sits on a pod in the donor's namespace, which its tenant may be able
// to write, so it is read only from the donor variant's own pods and is
// believed only when it matches what this controller would have written for
// that group now: the donor role and variant it is found under, a receiver
// role and variant of the same group, the replica sizes of both, and a start
// in the past. Anything else is removed with a WARN. A mark older than the
// release timeout is stale and removed quietly; one still on a pod that is not
// terminating is a release that never landed while no controller watched, and
// expired counts it, once per transfer, so it is counted as aborted.
//
// The pods of one transfer -- every pod of an LWS group -- are restored as one
// transfer. If any of them is not yet terminating, the donor is still counted
// in status.replicas and its target is lowered again to keep the release
// going; DonorLowered records that, so a later cancel or abort raises only
// what was lowered.
//
// An error listing a donor's pods is returned before anything is applied.
func (e *Engine) restoreShareTransfers(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger,
	g allocation.ShareGroup, accessor func(string, string) scaletarget.ScaleTargetAccessor,
	variantKey func(string, string) string, now time.Time, tm allocation.ShareTimings) (restored, expired int, err error) {
	if e.client == nil {
		return 0, 0, nil
	}
	type found struct {
		mark transferMark
		pods []string
		live bool
	}
	byID := map[string]*found{}
	var order []string
	var invalid []string
	aborted := map[string]bool{}
	for _, role := range slices.Sorted(maps.Keys(g.Give)) {
		give := g.Give[role]
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		pods, err := variantmeta.ListVariantPods(ctx, e.client, g.Origins[role].Namespace, acc)
		if err != nil {
			return 0, 0, fmt.Errorf("list the pods of donor variant %s: %w", give.Name, err)
		}
		for _, p := range pods {
			raw, ok := p.Annotations[utilizationShareTransferAnnotation]
			if !ok {
				continue
			}
			key := utils.GetNamespacedKey(p.Namespace, p.Name)
			var m transferMark
			if why := validTransferMark(raw, &m, role, give, g, now); why != "" {
				logger.Info("Utilization share: removed a transfer mark it did not write", "pod", key, "reason", why)
				invalid = append(invalid, key)
				continue
			}
			if now.Sub(m.Started) >= tm.ReleaseTimeout {
				invalid = append(invalid, key)
				if p.DeletionTimestamp == nil {
					aborted[role+"|"+m.ID] = true
				}
				continue
			}
			id := role + "|" + m.ID
			f, ok := byID[id]
			if !ok {
				f = &found{mark: m}
				byID[id] = f
				order = append(order, id)
			}
			f.pods = append(f.pods, key)
			f.live = f.live || p.DeletionTimestamp == nil
		}
	}
	// A transfer has one donor: an ID found under two roles is at least one
	// mark this controller did not write -- a set's second primary among them
	// -- and every pod carrying it is unmarked.
	idRoles := map[string]int{}
	for _, id := range order {
		idRoles[byID[id].mark.ID]++
	}
	order = slices.DeleteFunc(order, func(id string) bool {
		f := byID[id]
		if idRoles[f.mark.ID] > 1 {
			logger.Info("Utilization share: removed a transfer mark whose id another donor's pods also carry",
				"id", f.mark.ID, "pods", f.pods)
			invalid = append(invalid, f.pods...)
			return true
		}
		return false
	})
	// A donor set restores whole or not at all (section 6.5): a primary, and
	// members whose donor replicas together cover the receiver's replica. A
	// member without its primary, or a set that gives less than its receiver
	// takes, is a mark this controller did not write -- or the rest of it is
	// gone -- and every member is removed.
	setGives, primaryTakes := map[string]int{}, map[string]int{}
	for _, id := range order {
		m := byID[id].mark
		if m.SetID == "" {
			continue
		}
		setGives[m.SetID] += m.DonorGPUs
		if m.ID == m.SetID && m.Receiver != "" {
			primaryTakes[m.SetID] = m.GPUs
		}
	}
	order = slices.DeleteFunc(order, func(id string) bool {
		f := byID[id]
		if s := f.mark.SetID; s != "" {
			if takes, ok := primaryTakes[s]; !ok || setGives[s] < takes {
				logger.Info("Utilization share: removed an incomplete donor-set mark", "set", s, "pods", f.pods)
				invalid = append(invalid, f.pods...)
				return true
			}
		}
		return false
	})
	e.unmarkDonorPods(ctx, logger, allocation.ShareTransfer{DonorPods: invalid})
	contributors := map[string][]string{}
	for _, id := range order {
		f := byID[id]
		m := f.mark
		if m.SetID != "" && m.ID != m.SetID {
			contributors[m.SetID] = append(contributors[m.SetID], m.ID)
		}
		l.Restore(allocation.ShareTransfer{
			ID: m.ID, Donor: m.Donor, Receiver: m.Receiver, DonorVariant: m.DonorVariant,
			ReceiverVariant: m.ReceiverVariant, GPUs: m.GPUs, DonorGPUs: m.DonorGPUs, Started: m.Started,
			Deadline: m.Started.Add(tm.ReleaseTimeout), DonorPods: f.pods, DonorLowered: f.live, SetID: m.SetID,
			PlannedPods: plannedPods(m.Planned, f.pods),
		}, g.Committed[m.Donor])
		if f.live {
			e.utilizationShare.desired[variantKey(m.Donor, m.DonorVariant)]--
		}
	}
	for set, members := range contributors {
		l.LinkSet(set, members...)
	}
	return len(order), len(aborted), nil
}

// validTransferMark parses a mark into m and reports why it is not one this
// controller would have written for donor role and its giving variant, or "".
func validTransferMark(raw string, m *transferMark, role string, give allocation.ShareVariant,
	g allocation.ShareGroup, now time.Time) string {
	if err := json.Unmarshal([]byte(raw), m); err != nil {
		return "unparseable"
	}
	grow, ok := g.Grow[m.Receiver]
	refill := m.Receiver == "" && m.ReceiverVariant == "" && m.GPUs == 0
	switch {
	case m.ID == "":
		return "no id"
	case m.Donor != role || m.DonorVariant != give.Name:
		return "donor is not the variant the mark is on"
	case refill:
		if m.DonorGPUs != max(give.GPUs, 1) {
			return "replica sizes do not match the variants"
		}
		if m.Started.After(now) {
			return "starts in the future"
		}
		return ""
	case !ok || m.Receiver == role || m.ReceiverVariant != grow.Name:
		return "receiver is not a variant of this group that can grow"
	case m.GPUs != max(grow.GPUs, 1) || (m.SetID == "" && m.DonorGPUs != max(give.GPUs, 1, m.GPUs)) ||
		(m.SetID != "" && m.DonorGPUs != max(give.GPUs, 1)):
		// A single donor gives at least the receiver replica; a set member
		// gives one donor replica of several.
		return "replica sizes do not match the variants"
	case m.Started.After(now):
		return "starts in the future"
	}
	return ""
}

// remarkDonorPods rewrites a redirected transfer's marks on its donor pods, so
// a restart restores it as the release-with-no-receiver it now is, not as the
// transfer to its original receiver.
func (e *Engine) remarkDonorPods(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger, id string) {
	if e.client == nil {
		return
	}
	i := slices.IndexFunc(l.Transfers(), func(t allocation.ShareTransfer) bool { return t.ID == id })
	if i < 0 {
		return
	}
	t := l.Transfers()[i]
	for _, key := range t.DonorPods {
		ns, name, _ := strings.Cut(key, "/")
		var p corev1.Pod
		if err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p); err != nil {
			if !apierrors.IsNotFound(err) {
				logger.Error(err, "Utilization share: could not read a donor pod to re-mark it", "pod", key)
			}
			continue
		}
		if !markedFor(&p, id) {
			continue
		}
		var old transferMark
		_ = json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &old)
		raw, err := json.Marshal(transferMark{ID: t.ID, Donor: t.Donor, DonorVariant: t.DonorVariant,
			DonorGPUs: t.DonorGPUs, Started: t.Started, Planned: len(t.PlannedPods) > 0, PrevCost: old.PrevCost,
			Cost: old.Cost, Instance: metrics.GetControllerInstance()})
		if err != nil {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		p.Annotations[utilizationShareTransferAnnotation] = string(raw)
		if err := e.client.Patch(ctx, &p, patch); err != nil {
			logger.Error(err, "Utilization share: could not re-mark a donor pod", "pod", key)
		}
	}
}

// plannedPods is the restored PlannedPods of a mark: its pods, when the mark
// says a node-aware plan chose them.
func plannedPods(planned bool, pods []string) []string {
	if !planned {
		return nil
	}
	return pods
}

// plannedRunning returns the releasing transfers with a planned donor pod
// still running -- present and not terminating -- and those with one that
// could not be read. Only NotFound means gone: any other error leaves the
// transfer undecided for the cycle, since reading it as gone would raise the
// receiver into a hole that may not be open, and as running would end a
// transfer whose pod did go.
func (e *Engine) plannedRunning(ctx context.Context, logger logr.Logger, l *allocation.ShareLedger) (running, unknown map[string]bool) {
	running, unknown = map[string]bool{}, map[string]bool{}
	if e.client == nil {
		return running, unknown
	}
	for _, t := range l.Transfers() {
		if t.State != allocation.ShareReleasing {
			continue
		}
		for _, key := range t.PlannedPods {
			ns, name, _ := strings.Cut(key, "/")
			var p corev1.Pod
			err := e.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &p)
			switch {
			case apierrors.IsNotFound(err):
			case err != nil:
				logger.V(logging.DEBUG).Info("Utilization share: could not read a planned donor pod; transfer held this cycle",
					"id", t.ID, "pod", key, "error", err.Error())
				unknown[t.ID] = true
			case p.DeletionTimestamp == nil:
				running[t.ID] = true
			}
		}
	}
	return running, unknown
}

// shareMarkOwned reports whether a pod belongs to a transfer: its mark names a
// transfer live in the ledger, or the pod was marked earlier this cycle
// (taken, by namespace/name). A mark nothing in the ledger owns is a stale or
// forged annotation, not a claim on the pod.
func shareMarkOwned(l *allocation.ShareLedger, taken map[string]bool) func(*corev1.Pod) bool {
	live := map[string]bool{}
	for _, t := range l.Transfers() {
		live[t.ID] = true
	}
	return func(p *corev1.Pod) bool {
		if taken[utils.GetNamespacedKey(p.Namespace, p.Name)] {
			return true
		}
		var m transferMark
		return json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &m) == nil && live[m.ID]
	}
}

// dropShareActuation unmarks every live transfer's donor pods and forgets the
// ledgers: the optimizer was switched to shadow or off, and today's optimizer
// takes the variants back -- with pods that carry no deletion cost of ours.
func (e *Engine) dropShareActuation(ctx context.Context, logger logr.Logger) {
	for _, l := range e.utilizationShare.ledgers {
		for _, t := range l.Transfers() {
			e.unmarkDonorPods(ctx, logger, t)
		}
	}
	e.utilizationShare.resetActuation()
	e.sweepShareMarks(ctx, logger)
}

// sweepShareMarks removes, once each time the optimizer stops acting, every
// mark this controller wrote that no ledger holds: after a restart the ledgers
// are empty until the first acting cycle restores them, so a restart into
// shadow mode, or with the optimizer removed, would otherwise leave its pods
// at our deletion cost for good. Marks of another controller instance are
// left alone. A failed list, or a pod it could not unmark, is retried next
// cycle.
func (e *Engine) sweepShareMarks(ctx context.Context, logger logr.Logger) {
	st := &e.utilizationShare
	if st.swept || e.client == nil {
		return
	}
	var pods corev1.PodList
	if err := e.client.List(ctx, &pods); err != nil {
		logger.Error(err, "Utilization share: could not list pods to remove its marks; retrying next cycle")
		return
	}
	instance := metrics.GetControllerInstance()
	var ours []string
	for i := range pods.Items {
		p := &pods.Items[i]
		raw, ok := p.Annotations[utilizationShareTransferAnnotation]
		if !ok {
			continue
		}
		var m transferMark
		if json.Unmarshal([]byte(raw), &m) != nil || m.ID == "" || m.Instance != instance {
			continue
		}
		ours = append(ours, utils.GetNamespacedKey(p.Namespace, p.Name))
	}
	if len(ours) > 0 {
		logger.Info("Utilization share: removing the marks it left on pods while it was not acting", "pods", len(ours))
		if e.unmarkDonorPods(ctx, logger, allocation.ShareTransfer{DonorPods: ours}) > 0 {
			return
		}
	}
	st.swept = true
}

// validDeletionCost reports whether v is a pod-deletion-cost the API server
// accepts (an int32). A prevCost that is not -- a forged mark -- is not
// restored: the patch would be refused every time, and the mark never cleared.
func validDeletionCost(v string) bool {
	_, err := strconv.ParseInt(v, 10, 32)
	return err == nil
}

// markCost is the deletion cost a donor's newly marked pods get, and whether
// it orders them as it must. Below every unmarked sibling that stays, so the
// ReplicaSet removes them first: donorDeletionCost, or lower when a user set a
// sibling lower still. And above every live mark of this donor, so an earlier
// transfer's pod goes before this one's. ok is false when no cost fits both,
// or none fits below a sibling a user pinned at the int32 minimum.
//
// A live mark the cache does not show yet was written this cycle at its own
// base cost or above. That base is at most this one's -- its siblings were a
// superset of these -- and it was raised above base only over an older live
// mark, which would make three marks on one donor: more than
// allocation.ShareMaxConcurrentTransfers admits.
func markCost(unmarked, live []corev1.Pod) (string, bool) {
	lowest := int64(0)
	for i := range unmarked {
		lowest = min(lowest, podDeletionCost(&unmarked[i]))
	}
	cost := max(min(lowest-1, -1000), math.MinInt32)
	ok := cost < lowest
	if len(live) > 0 {
		highest := int64(math.MinInt32)
		for i := range live {
			c := podDeletionCost(&live[i])
			if _, ok := live[i].Annotations[utilizationShareTransferAnnotation]; !ok {
				// Marked earlier this cycle, read through a cache that does
				// not show the patch yet: it carries the base cost.
				c = cost
			}
			highest = max(highest, c)
		}
		if highest+1 < lowest {
			cost = max(cost, highest+1)
		} else {
			ok = false
		}
	}
	return strconv.FormatInt(cost, 10), ok
}

// podReadySince is when p last became Ready; the zero time when it is not.
func podReadySince(p *corev1.Pod) time.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}
