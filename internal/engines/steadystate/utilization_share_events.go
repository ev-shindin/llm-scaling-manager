package steadystate

import (
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/domain"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/allocation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// Events on the scale targets a transfer moves, so a model's owner reads in
// `kubectl describe` why it shrank or grew, without the controller's log. The
// other party is named only within the same namespace: a tenant's Events must
// not name another tenant's models.

// shareEventObject references a scale target for an Event.
func shareEventObject(acc scaletarget.ScaleTargetAccessor) *corev1.ObjectReference {
	ref := &corev1.ObjectReference{Namespace: acc.GetNamespace(), Name: acc.GetName(), UID: acc.GetUID(),
		Kind: constants.DeploymentKind, APIVersion: appsv1.SchemeGroupVersion.String()}
	if scaletarget.IsLeaderWorkerSet(acc) {
		ref.Kind, ref.APIVersion = constants.LeaderWorkerSetKind, lwsv1.GroupVersion.String()
	}
	return ref
}

// shareEvent records an Event on acc's scale target, when there is a recorder
// and a target.
func (e *Engine) shareEvent(acc scaletarget.ScaleTargetAccessor, eventType, reason, message string) {
	if e.Recorder == nil || acc == nil {
		return
	}
	e.Recorder.Event(shareEventObject(acc), eventType, reason, message)
}

// sharePeer names other for an Event on self's scale target: by model within
// the same namespace, otherwise only as a model of the same quota group.
func sharePeer(g allocation.ShareGroup, self, other string) string {
	so, oo := g.Origins[self], g.Origins[other]
	if so.Namespace == "" || so.Namespace != oo.Namespace {
		return "a model in another namespace of its quota group"
	}
	if oo.Role == "" || oo.Role == domain.RoleBoth {
		return "model " + oo.ModelID
	}
	return fmt.Sprintf("model %s (%s)", oo.ModelID, oo.Role)
}

// shareStartedEvents records a started transfer on its donor and receiver.
// set is the transfer's donor set, or the transfer alone; pods are the donor's
// pods it marked.
func (e *Engine) shareStartedEvents(g allocation.ShareGroup, t allocation.ShareTransfer, set []allocation.ShareTransfer,
	pods []string, accessor func(string, string) scaletarget.ScaleTargetAccessor) {
	receiver, urgent := t.Receiver, t.Urgent
	for _, m := range set {
		if t.SetID != "" && m.ID == t.SetID {
			receiver, urgent = m.Receiver, m.Urgent // a contributor funds its primary's receiver
		}
	}
	to := "the quota group's scale-from-zero reserve (reserveGPUs)"
	why := "the reserve was spent"
	if receiver != "" {
		to = sharePeer(g, t.Donor, receiver)
		why = "to even out headroom by weight"
		if urgent {
			why = "because it is below its need"
		}
		if len(set) > 1 {
			to += fmt.Sprintf(", with %d other donor replicas", len(set)-1)
		}
	}
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		_, name, _ := strings.Cut(p, "/")
		names = append(names, name)
	}
	e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareGiving,
		fmt.Sprintf("Utilization share: giving one replica (%d GPUs) to %s %s; %s goes",
			t.DonorGPUs, to, why, strings.Join(names, ", ")))
	if t.Receiver != "" && t.ReceiverVariant != "" {
		e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareReceiving,
			fmt.Sprintf("Utilization share: receiving %d GPUs from %s; raised once its replica is released",
				t.GPUs, sharePeer(g, t.Receiver, t.Donor)))
	}
}

// shareEndedEvent records how a transfer ended, on the side the outcome is
// about: a donor that did not release or lost another pod, a receiver that
// got or did not get its GPUs. backoff is when an aborted donor may give again.
func (e *Engine) shareEndedEvent(g allocation.ShareGroup, end allocation.ShareTransferEnd, tm allocation.ShareTimings,
	backoff time.Time, accessor func(string, string) scaletarget.ScaleTargetAccessor) {
	t := end.Transfer
	switch end.Outcome {
	case allocation.ShareOutcomeAborted:
		switch {
		case t.DonorVariant == "":
		case !backoff.After(t.Started) && end.DonorReleased:
			// Its pod is already gone: the restored count is a cold start.
			e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeWarning,
				constants.K8SEventUtilizationShareReleaseAborted,
				fmt.Sprintf("Utilization share: the transfer was called off because its donor set could not complete. "+
					"This model had already given up %s, so restoring its count starts a cold replacement, which waits "+
					"for GPUs if others took the freed ones. This model is not held back from giving again.",
					shareDonorPodNames(t)))
		case !backoff.After(t.Started):
			// Not this donor's failure: its donor set could not complete (a
			// member broke, or released while a contributor did not), and the
			// ledger set it no back-off for it.
			e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeWarning,
				constants.K8SEventUtilizationShareReleaseAborted,
				"Utilization share: the transfer was called off because its donor set could not complete; "+
					"this model's count is restored, and it is not held back for it")
		default:
			e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeWarning,
				constants.K8SEventUtilizationShareReleaseAborted,
				fmt.Sprintf("Utilization share: the replica was not released within %s; its count is restored, and it is "+
					"not asked to give again before %s (the wait doubles with each abort in a row, up to 16 release "+
					"timeouts). Check the ScaledObject's scale-down window and the pods' termination grace",
					tm.ReleaseTimeout, backoff.UTC().Format(time.RFC3339)))
		}
	case allocation.ShareOutcomeWrongPod:
		e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeWarning,
			constants.K8SEventUtilizationShareWrongPod,
			"Utilization share: a pod other than the marked one was removed (a rollout, an eviction, another "+
				"scale-down?); this model stays one replica lower, and the receiver is not raised")
	case allocation.ShareOutcomeFillTimeout:
		if t.ReceiverVariant != "" {
			e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeWarning,
				constants.K8SEventUtilizationShareFillTimedOut,
				fmt.Sprintf("Utilization share: the new replica did not take the released GPUs within %s; "+
					"it keeps its target and is scheduled when it can be", tm.FillTimeout))
		}
	case allocation.ShareOutcomeDone:
		switch {
		case t.ReceiverVariant == "":
		case t.Donor != "":
			e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal,
				constants.K8SEventUtilizationShareReceived,
				fmt.Sprintf("Utilization share: received %d GPUs from %s", t.GPUs, sharePeer(g, t.Receiver, t.Donor)))
		default:
			// An idle fill: no donor, so nothing else would tell the owner why
			// the model grew with no load to grow it.
			e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal,
				constants.K8SEventUtilizationShareReceived,
				fmt.Sprintf("Utilization share: received %d GPUs from the quota group's spare GPUs, as headroom by weight; "+
					"they are given to another model when it needs them", t.GPUs))
		}
	}
}

// shareCancelledEvents records a transfer called off on a clear reversal, on
// both sides.
func (e *Engine) shareCancelledEvents(t allocation.ShareTransfer, accessor func(string, string) scaletarget.ScaleTargetAccessor) {
	e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareCancelled,
		"Utilization share: the transfer was called off because demand reversed; its replica count is restored")
	if t.ReceiverVariant != "" {
		e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareCancelled,
			"Utilization share: the transfer to this model was called off because demand reversed")
	}
}

// shareRedirectedEvent records, on the receiver, that a model waking from zero
// took the GPUs it was to receive.
func (e *Engine) shareRedirectedEvent(t allocation.ShareTransfer, accessor func(string, string) scaletarget.ScaleTargetAccessor) {
	if t.ReceiverVariant == "" {
		return
	}
	e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareRedirected,
		"Utilization share: the GPUs this model was to receive went to a model waking from zero; it is planned again")
}

// shareUnsteerableEvent records, on a donor whose pods could not be marked,
// why it was not asked to give.
func (e *Engine) shareUnsteerableEvent(acc scaletarget.ScaleTargetAccessor, cause error, until time.Time) {
	e.shareEvent(acc, corev1.EventTypeWarning, constants.K8SEventUtilizationShareDonorNotSteerable,
		fmt.Sprintf("Utilization share: could not choose which pod gives (%v); not asked to give again before %s",
			cause, until.UTC().Format(time.RFC3339)))
}

// shareDonorPodNames names the donor pods a transfer marked, without their
// namespace (the donor's own, where the Event is read), or "its replica" when
// it recorded none.
func shareDonorPodNames(t allocation.ShareTransfer) string {
	names := make([]string, 0, len(t.DonorPods))
	for _, k := range t.DonorPods {
		_, name, _ := strings.Cut(k, "/")
		names = append(names, name)
	}
	if len(names) == 0 {
		return "its replica"
	}
	return strings.Join(names, ", ")
}
