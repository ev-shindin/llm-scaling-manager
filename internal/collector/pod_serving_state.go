package collector

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// podServingState is what a Pod's own object says about whether it can serve.
//
// Separate from anything Prometheus reports, because a series says only that
// something answered a scrape. It does not say the Pod still exists, is not
// being deleted, or is in the rotation.
type podServingState struct {
	// live is false for a Pod that is being deleted. Its capacity is going away
	// and counting it holds the fleet up on supply that is about to vanish.
	live bool
	// ready is the kubelet's readiness verdict, which is what decides whether a
	// Service or an EPP will route to the Pod at all.
	ready bool
	// startedAt is when the kubelet started the Pod, zero when it has not yet
	// (or the field is unset). It bounds how long any request this Pod reports
	// can possibly have taken -- see podUptime, and the guard in
	// attributeInstance that spends it.
	startedAt time.Time
	// startSeconds is how long this Pod took to become Ready: the Ready
	// condition's LastTransitionTime less the Pod's CreationTimestamp. Zero
	// when the Pod is not ready yet, or when either timestamp is missing.
	//
	// From the Pod object rather than from polling. The demand floor projects
	// the backlog forward over this figure and multiplies it by the arrival
	// rate, so a whole cycle of rounding error -- which "the first cycle we saw
	// it Ready" would carry -- lands as tens of requests of phantom backlog.
	//
	// It is the time to READY, not to serving. Readiness is what the Service and
	// the EPP route on, so it is the moment the replica starts draining a queue,
	// which is what the projection is about.
	startSeconds float64
	// readySince is when the Pod last became Ready: the Ready condition's
	// LastTransitionTime. Zero when it is not Ready or the time is unset.
	readySince time.Time
	// revision is the pod template revision the Pod was created from
	// (podRevision), "" when it carries none.
	revision string
}

// namespacePods is one namespace's listing, plus whether it could be read.
//
// The two are not the same and conflating them is dangerous. A listing that
// SUCCEEDED and does not contain a Pod says the Pod is gone. A listing that
// FAILED says nothing at all -- and treating that as "every Pod is gone" would
// drop every series in the namespace, hand the analyzer zero supply, and scale
// the fleet to the floor on an RBAC error or a moment of API unavailability.
type namespacePods struct {
	byName map[string]podServingState
	// listed is false when the read failed. Callers fail OPEN on it: behave
	// exactly as WVA did before Pod state was consulted at all.
	listed bool
}

// podStates lists every Pod in a namespace and reduces each to the two facts
// attribution needs.
//
// A LIST rather than a Get per Pod, and an UNCACHED read rather than the
// manager's client. One call per namespace per cycle answers for every Pod at
// once, where a Get per reporting Pod would be dozens. The read is uncached
// for freshness, not memory: the manager's cache does hold Pods (discovery and
// the GPU usage walk list them through it, cluster-wide on a cluster-scoped
// install, trimmed by internal/kubecache), but a deletion is the one event this
// must never miss behind an informer's lag.
//
// These facts are also MUTABLE, which is why they are not put in the locator's
// resolution cache beside the scale target and labels. That cache is correct
// precisely because what it holds -- ownerReferences -- cannot change. Readiness
// changes minute to minute, and a deletion is the one event that must never be
// served from a cache.
//
// A failed listing is counted (wva_metrics_collection_errors_total,
// query_type=pod_list) and logged. Every caller fails OPEN on it -- every Pod
// reads Ready and present -- which is the safe answer for supply, but it also
// lets a not-Ready Pod's saturated reading become the variant's k2, which the
// analyzer otherwise refuses (P1-obs-unadmitted). Silent, that is a low k2 no
// one can trace back to an RBAC change. The listing is memoised per namespace
// per cycle (servingState), so this fires once per namespace per cycle.
func podStates(ctx context.Context, reader client.Reader, namespace string) namespacePods {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		reason := podListErrorReason(err)
		metrics.IncMetricsCollectionErrors(constants.QueryTypePodList, reason)
		ctrl.LoggerFrom(ctx).V(logging.DEFAULT).Info("pod listing failed; every Pod in the namespace is treated as Ready and present this cycle",
			"namespace", namespace, "reason", reason, "error", err.Error())
		return namespacePods{listed: false}
	}
	out := make(map[string]podServingState, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		state := podServingState{
			live:  p.DeletionTimestamp == nil,
			ready: podIsReady(p),
		}
		if p.Status.StartTime != nil {
			state.startedAt = p.Status.StartTime.Time
		}
		state.startSeconds = podStartSeconds(p)
		state.readySince = podReadySince(p)
		state.revision = podRevision(p)
		out[p.Name] = state
	}
	return namespacePods{byName: out, listed: true}
}

// podRevisionLabels are the labels a workload controller stamps a Pod's
// template revision under, the first one present wins: a LeaderWorkerSet's
// leader and workers, a Deployment's ReplicaSet, a StatefulSet.
var podRevisionLabels = []string{
	"leaderworkerset.sigs.k8s.io/template-revision-hash",
	"pod-template-hash",
	"controller-revision-hash",
}

// podRevision is the template revision the Pod was created from, or "".
func podRevision(p *corev1.Pod) string {
	for _, l := range podRevisionLabels {
		if v := p.Labels[l]; v != "" {
			return v
		}
	}
	return ""
}

// podListErrorReason names a failed Pod listing for the reason label of
// wva_metrics_collection_errors_total.
func podListErrorReason(err error) string {
	switch {
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return "forbidden"
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err):
		return "unavailable"
	default:
		return "other"
	}
}

// podStartSeconds is how long the Pod took to become Ready, from its own
// timestamps. Zero when it is not ready, or when either timestamp is absent.
//
// Negative and absurd values are refused rather than clamped. A clock skew
// between the API server and the kubelet can order the two timestamps wrongly,
// and a zero here reads as "not measured" -- which the caller already handles --
// where a negative or a nonsense figure would travel into a backlog projection.
func podStartSeconds(p *corev1.Pod) float64 {
	if p.CreationTimestamp.IsZero() {
		return 0
	}
	for _, c := range p.Status.Conditions {
		if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
			continue
		}
		if c.LastTransitionTime.IsZero() {
			return 0
		}
		d := c.LastTransitionTime.Time.Sub(p.CreationTimestamp.Time).Seconds()
		if d <= 0 || d > maxCredibleStartSeconds {
			return 0
		}
		return d
	}
	return 0
}

// maxCredibleStartSeconds bounds what is accepted as a start time. An hour is
// far beyond any real engine start -- the longest measured here is 82 s, and a
// GLM-5.2 cold start with a cold JIT cache is about 500 s -- so anything past it
// is a clock problem or a Pod that was created long before it was scheduled,
// neither of which describes how long the NEXT replica will take.
const maxCredibleStartSeconds = 3600.0

// podReadySince is when the Pod last became Ready, zero when it is not Ready
// or its Ready condition carries no transition time.
func podReadySince(p *corev1.Pod) time.Time {
	if !podIsReady(p) {
		return time.Time{}
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.LastTransitionTime.Time
		}
	}
	return time.Time{}
}

// podIsReady reports the Pod's Ready condition.
//
// The CONDITION, not the phase. A Pod stays in phase Running while its
// containers fail their probes, so phase alone would call a Pod ready that
// nothing is routing to -- which is the state a rolling replacement spends its
// first seconds in, and the one an engine spends serving /metrics before it can
// serve a request.
func podIsReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// servingState answers for one Pod, listing the namespace once per cycle.
//
// Outside a cycle there is no memo and every call lists. That is the same
// bargain BeginCycle already makes for the queries: correct either way, shared
// only when a caller has said a cycle is open.
//
// Returns the state, whether the listing CONTAINED this Pod, and whether the
// listing could be read at all. Callers must keep the last two apart: absent
// from a good listing means gone, while an unreadable listing means unknown and
// must not be acted on.
func (c *ReplicaMetricsCollector) servingState(
	ctx context.Context, namespace, podName string,
) (state podServingState, found, listed bool) {
	if podName == "" || c.apiReader == nil {
		return podServingState{}, false, false
	}

	c.cycleMu.Lock()
	if c.cyclePods != nil {
		if cached, ok := c.cyclePods[namespace]; ok {
			c.cycleMu.Unlock()
			st, in := cached.byName[podName]
			return st, in, cached.listed
		}
	}
	c.cycleMu.Unlock()

	pods := podStates(ctx, c.apiReader, namespace)

	c.cycleMu.Lock()
	if c.cyclePods != nil {
		c.cyclePods[namespace] = pods
	}
	c.cycleMu.Unlock()

	st, in := pods.byName[podName]
	return st, in, pods.listed
}

// podIsGone reports a Pod that a GOOD listing says is absent or being deleted.
//
// False whenever the listing could not be read, so a failed List behaves exactly
// as WVA did before Pod state was consulted: every series is kept. Dropping them
// instead would turn an RBAC error into an empty fleet.
func (c *ReplicaMetricsCollector) podIsGone(ctx context.Context, namespace, podName string) bool {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed {
		return false
	}
	return !found || !state.live
}

// podRevision reports the template revision the Pod was created from, "" when
// the listing could not be read, does not contain the Pod, or the Pod carries
// none -- an unknown, which the analyzer's revision check does not count.
func (c *ReplicaMetricsCollector) podRevision(ctx context.Context, namespace, podName string) string {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed || !found {
		return ""
	}
	return state.revision
}

// podReady reports whether the Pod is in the rotation.
//
// Fails OPEN: an unreadable listing answers ready, which is the behaviour before
// this existed. The alternative -- calling every Pod unready -- would publish a
// serving count of zero, and zero is a load-bearing answer meaning "its replicas
// are demonstrably not serving", which is not what an API error establishes.
func (c *ReplicaMetricsCollector) podReady(ctx context.Context, namespace, podName string) bool {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed {
		return true
	}
	return found && state.ready
}

// podUptime reports how long the Pod has been running, and whether that could
// be established at all.
//
// False whenever the listing could not be read, the Pod is not in it, or it
// carries no start time — the same fail-open contract as podReady. A caller
// bounding a metric by uptime must not treat "unknown" as "zero", which would
// make every value look impossible.
func (c *ReplicaMetricsCollector) podUptime(
	ctx context.Context, namespace, podName string, now time.Time,
) (time.Duration, bool) {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed || !found || state.startedAt.IsZero() {
		return 0, false
	}
	uptime := now.Sub(state.startedAt)
	if uptime < 0 {
		// A start time in the future is clock skew between this process and the
		// node, not a Pod that has not started. Unknown rather than zero.
		return 0, false
	}
	return uptime, true
}

// podStartSeconds reports how long this Pod took to become Ready, and zero when
// that cannot be established.
//
// Zero rather than a fail-open guess, which is the opposite of podReady's
// contract and deliberately so: readiness has a safe default ("assume it is
// serving") because the fleet is already sized for it, while a start time has
// none. A guessed dead time multiplies the arrival rate straight into a backlog
// projection, so "not measured" has to stay distinguishable from "measured as
// something" -- the analyzer falls back to the ScaledObject's seed on zero.
func (c *ReplicaMetricsCollector) podStartSeconds(
	ctx context.Context, namespace, podName string,
) float64 {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed || !found {
		return 0
	}
	return state.startSeconds
}

// podReadySince reports when this Pod last became Ready, and zero when that
// cannot be established: the listing could not be read, does not contain the
// Pod, or the Pod is not Ready. Zero is "unknown", not "long ago".
func (c *ReplicaMetricsCollector) podReadySince(
	ctx context.Context, namespace, podName string,
) time.Time {
	state, found, listed := c.servingState(ctx, namespace, podName)
	if !listed || !found {
		return time.Time{}
	}
	return state.readySince
}

// seriesPodName pulls the Pod identity out of a series' labels.
//
// Two conventions, because the scrape decides which: the Prometheus operator's
// target relabeling produces `pod`, while raw scrape jobs and
// kube-state-metrics-style configs produce `pod_name`. Shared so that every path
// which asks "which Pod is this series from" answers it the same way.
func seriesPodName(labels map[string]string) string {
	if p := labels["pod"]; p != "" {
		return p
	}
	return labels["pod_name"]
}

// SeriesPodIsGone reports whether the Pod behind a series has been deleted or is
// terminating, so a caller holding raw series can drop it.
//
// Exported for the analyzers that do NOT go through the collector's rows. An
// external analyzer runs the operator's own PromQL and sums the result itself,
// so it never reaches buildInstanceKey -- without this it would count deleted
// Pods for as long as Prometheus keeps their series, which is the same defect
// the builder exists to prevent, reached by a path the builder cannot see.
//
// A series with no Pod label is never dropped. That is an ALREADY-AGGREGATED
// query, where Prometheus did the summing and there is no per-Pod identity left
// to judge; the honest answer is to leave it alone rather than guess.
func (c *ReplicaMetricsCollector) SeriesPodIsGone(ctx context.Context, namespace string, labels map[string]string) bool {
	podName := seriesPodName(labels)
	if podName == "" {
		return false
	}
	return c.podIsGone(ctx, namespace, podName)
}
