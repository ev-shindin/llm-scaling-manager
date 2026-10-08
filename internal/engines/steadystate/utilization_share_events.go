package steadystate

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	lwsv1 "sigs.k8s.io/lws/api/leaderworkerset/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
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
		Kind: "Deployment", APIVersion: appsv1.SchemeGroupVersion.String()}
	if scaletarget.IsLeaderWorkerSet(acc) {
		ref.Kind, ref.APIVersion = "LeaderWorkerSet", lwsv1.GroupVersion.String()
	}
	return ref
}

// shareEvent records an Event on the scale target of role's variant, when there
// is a recorder and the target is known.
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
	if other == "" {
		return "the quota's reserve"
	}
	if so.Namespace != "" && so.Namespace == oo.Namespace {
		return fmt.Sprintf("model %s (%s)", oo.ModelID, oo.Role)
	}
	return "a model in another namespace of its quota group"
}

// shareStartedEvents records a started transfer on its donor and receiver.
func (e *Engine) shareStartedEvents(g allocation.ShareGroup, t allocation.ShareTransfer,
	accessor func(string, string) scaletarget.ScaleTargetAccessor) {
	e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareGiving,
		fmt.Sprintf("Utilization share: giving one replica (%d GPUs) to %s; the pod marked with deletion cost %s goes first",
			t.DonorGPUs, sharePeer(g, t.Donor, t.Receiver), donorDeletionCost))
	if t.Receiver != "" && t.ReceiverVariant != "" {
		e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal, constants.K8SEventUtilizationShareReceiving,
			fmt.Sprintf("Utilization share: receiving %d GPUs from %s; raised once its replica is released",
				t.GPUs, sharePeer(g, t.Receiver, t.Donor)))
	}
}

// shareEndedEvent records how a transfer ended, on the side the outcome is
// about: a donor that did not release or lost another pod, a receiver that
// got or did not get its GPUs.
func (e *Engine) shareEndedEvent(g allocation.ShareGroup, end allocation.ShareTransferEnd, tm allocation.ShareTimings,
	accessor func(string, string) scaletarget.ScaleTargetAccessor) {
	t := end.Transfer
	switch end.Outcome {
	case allocation.ShareOutcomeAborted:
		if t.DonorVariant != "" {
			e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeWarning,
				constants.K8SEventUtilizationShareReleaseAborted,
				fmt.Sprintf("Utilization share: the replica was not released within %s; its count is restored, "+
					"and it is not asked to give again for a while", tm.ReleaseTimeout))
		}
	case allocation.ShareOutcomeWrongPod:
		e.shareEvent(accessor(t.Donor, t.DonorVariant), corev1.EventTypeWarning,
			constants.K8SEventUtilizationShareWrongPod,
			"Utilization share: a pod other than the marked one went; the receiver is not raised")
	case allocation.ShareOutcomeFillTimeout:
		if t.ReceiverVariant != "" {
			e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeWarning,
				constants.K8SEventUtilizationShareFillTimedOut,
				fmt.Sprintf("Utilization share: the new replica did not take the released GPUs within %s; "+
					"it keeps its target and is scheduled when it can be", tm.FillTimeout))
		}
	case allocation.ShareOutcomeDone:
		if t.ReceiverVariant != "" && t.Donor != "" {
			e.shareEvent(accessor(t.Receiver, t.ReceiverVariant), corev1.EventTypeNormal,
				constants.K8SEventUtilizationShareReceived,
				fmt.Sprintf("Utilization share: received %d GPUs from %s", t.GPUs, sharePeer(g, t.Receiver, t.Donor)))
		}
	}
}
