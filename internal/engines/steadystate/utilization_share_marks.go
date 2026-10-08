package steadystate

// Donor-pod marks: steering which pod a donor gives, checking it went, and
// rebuilding the ledger from the marks after a restart (proposal sections
// 6.3 and 6.5).

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

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
}

// donorPods lists the pods a donor variant would release: on a Deployment, its
// active pods; on a LeaderWorkerSet, the pods of its highest-index group, which
// LWS removes first and does not steer by cost. Pods that finished
// (Succeeded, Failed) or are being deleted are not active, as the ReplicaSet
// counts them. A donor with an active pod not yet scheduled cannot be steered
// -- the ReplicaSet removes an unscheduled pod before it consults the cost, and
// LWS's highest group may be that pod's -- and is refused.
func (e *Engine) donorPods(ctx context.Context, acc scaletarget.ScaleTargetAccessor, namespace string) ([]corev1.Pod, error) {
	lws := scaletarget.IsLeaderWorkerSet(acc)
	list, err := variantmeta.ListVariantPods(ctx, e.client, namespace, acc)
	if err != nil {
		return nil, err
	}
	var pods []corev1.Pod
	maxGroup := -1
	for _, p := range list {
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Spec.NodeName == "" {
			return nil, fmt.Errorf("%s has a pod not yet scheduled (%s); its victim cannot be steered", acc.GetName(), p.Name)
		}
		if lws {
			if i, err := strconv.Atoi(p.Labels[lwsv1.GroupIndexLabelKey]); err == nil && i > maxGroup {
				maxGroup = i
			}
		}
		pods = append(pods, p)
	}
	if lws {
		if maxGroup < 0 {
			return nil, fmt.Errorf("no pod of %s carries a group index", acc.GetName())
		}
		pods = slices.DeleteFunc(pods, func(p corev1.Pod) bool {
			return p.Labels[lwsv1.GroupIndexLabelKey] != strconv.Itoa(maxGroup)
		})
	}
	return pods, nil
}

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
// It returns the pods it marked as the API server returned them: a rollback in
// the same cycle unmarks those, not a cached read that does not show the mark
// yet.
func (e *Engine) markDonorPods(ctx context.Context, t allocation.ShareTransfer,
	acc scaletarget.ScaleTargetAccessor, namespace string, marked func(*corev1.Pod) bool) ([]corev1.Pod, error) {
	if e.client == nil || acc == nil {
		return nil, fmt.Errorf("no client or scale target for donor variant %q", t.DonorVariant)
	}
	pods, err := e.donorPods(ctx, acc, namespace)
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
	pods = slices.DeleteFunc(pods, func(p corev1.Pod) bool { return marked(&p) })
	if len(pods) == 0 {
		return nil, fmt.Errorf("donor variant %q has no unmarked pod to give", t.DonorVariant)
	}
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
		// The pod the ReplicaSet would remove anyway, or the least protected:
		// the lowest deletion cost a user set, then the name.
		slices.SortFunc(pods, func(a, b corev1.Pod) int {
			return cmp.Or(cmp.Compare(podDeletionCost(&a), podDeletionCost(&b)), cmp.Compare(a.Name, b.Name))
		})
		pods = pods[:1]
	}
	var done []corev1.Pod
	for i := range pods {
		p := &pods[i]
		mark := transferMark{ID: t.ID, Donor: t.Donor, Receiver: t.Receiver, DonorVariant: t.DonorVariant,
			ReceiverVariant: t.ReceiverVariant, GPUs: t.GPUs, DonorGPUs: t.DonorGPUs, Started: t.Started, SetID: t.SetID,
			Planned: len(t.PlannedPods) > 0, Instance: metrics.GetControllerInstance()}
		// The pod's own deletion cost, restored with the mark's removal. One a
		// stale mark of ours wrote is not the user's.
		if prev, ok := p.Annotations[podDeletionCostAnnotation]; ok {
			var old transferMark
			if json.Unmarshal([]byte(p.Annotations[utilizationShareTransferAnnotation]), &old) == nil && old.ID != "" {
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
		p.Annotations[podDeletionCostAnnotation] = donorDeletionCost
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
				logger.Error(err, "could not read a donor pod to unmark it", "pod", key)
				failed++
			}
			continue
		}
		if t.ID != "" && !markedFor(&p, t.ID) {
			continue // marked for another transfer since: not this one's to clear
		}
		if err := e.unmarkPod(ctx, &p); err != nil {
			logger.Error(err, "could not unmark a donor pod", "pod", key)
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
	case p.Annotations[podDeletionCostAnnotation] != donorDeletionCost:
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
// release timeout is stale and removed quietly.
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
	variantKey func(string, string) string, now time.Time, tm allocation.ShareTimings) (int, error) {
	if e.client == nil {
		return 0, nil
	}
	type found struct {
		mark transferMark
		pods []string
		live bool
	}
	byID := map[string]*found{}
	var order []string
	var invalid []string
	for _, role := range slices.Sorted(maps.Keys(g.Give)) {
		give := g.Give[role]
		acc := accessor(role, give.Name)
		if acc == nil {
			continue
		}
		pods, err := variantmeta.ListVariantPods(ctx, e.client, g.Origins[role].Namespace, acc)
		if err != nil {
			return 0, fmt.Errorf("list the pods of donor variant %s: %w", give.Name, err)
		}
		for _, p := range pods {
			raw, ok := p.Annotations[utilizationShareTransferAnnotation]
			if !ok {
				continue
			}
			key := utils.GetNamespacedKey(p.Namespace, p.Name)
			var m transferMark
			if why := validTransferMark(raw, &m, role, give, g, now); why != "" {
				logger.Info("utilization share removed a transfer mark it did not write", "pod", key, "reason", why)
				invalid = append(invalid, key)
				continue
			}
			if now.Sub(m.Started) >= tm.ReleaseTimeout {
				invalid = append(invalid, key)
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
			logger.Info("utilization share removed a transfer mark whose id another donor's pods also carry",
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
				logger.Info("utilization share removed an incomplete donor-set mark", "set", s, "pods", f.pods)
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
	return len(order), nil
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
				logger.Error(err, "could not read a donor pod to re-mark it", "pod", key)
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
			Instance: metrics.GetControllerInstance()})
		if err != nil {
			continue
		}
		patch := client.MergeFrom(p.DeepCopy())
		p.Annotations[utilizationShareTransferAnnotation] = string(raw)
		if err := e.client.Patch(ctx, &p, patch); err != nil {
			logger.Error(err, "could not re-mark a donor pod", "pod", key)
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
		logger.Error(err, "utilization share could not list pods to remove its marks; retrying next cycle")
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
		logger.Info("utilization share is removing the marks it left on pods while it was not acting", "pods", len(ours))
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
