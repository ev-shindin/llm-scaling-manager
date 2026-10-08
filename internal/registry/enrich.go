package registry

import (
	"context"
	"maps"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// DefaultTargetMaxAge bounds how stale an entry's ScaledObject read may be.
//
// Sized against how often the thing it reads actually changes — the scale target
// and the min/max envelope change when someone edits a manifest — not against
// how often WVA looks at it. That matters because the read must be UNCACHED:
// a cached read of a ScaledObject is served by a cluster-wide informer, which is
// the very LIST+WATCH this design exists to remove. So each refresh is a real
// request, and refreshing on the scale-from-zero loop's 10Hz cadence would put
// one request per variant per 100ms on the API server.
//
// It must stay comfortably SHORTER than DefaultTTL, with room for several
// attempts per entry lifetime. At parity, an entry becomes stale enough to
// re-read at about the moment it expires, so Snapshot evicts it in the same pass
// that would have refreshed it and no entry is ever enriched — a fleet that looks
// discovered and never resolves a scale target. Pinned by
// TestEnrichmentWindowMustSitInsideTheTTL.
const DefaultTargetMaxAge = 30 * time.Second

// Enricher fills in the part of an entry the KEDA call does not carry, by
// reading the ScaledObject it names.
//
// Reader MUST be an uncached one (manager.GetAPIReader), for the reason above.
// Passing the cached client would work and would quietly reinstate the
// cluster-wide watch.
type Enricher struct {
	Reader   client.Reader
	Registry *Registry
	// MaxAge is how stale a target read may be before it is refreshed. Zero uses
	// DefaultTargetMaxAge.
	MaxAge time.Duration
	// Tracker, if set, is kept in step with the registry's live namespaces so
	// namespace-scoped configuration loads for exactly the namespaces WVA has
	// workloads in. Optional.
	Tracker NamespaceTracker
	// tracked remembers what was handed to Tracker last pass, so entries that go
	// away can be untracked. Only touched from Refresh.
	tracked map[string]string
	// now is the clock, overridden in tests.
	now func() time.Time
}

// NamespaceTracker is the slice of the datastore the namespace sync needs.
type NamespaceTracker interface {
	NamespaceTrack(resourceType, resourceName, namespace string)
	NamespaceUntrack(resourceType, resourceName, namespace string)
}

// namespaceResourceType labels registry-sourced tracking in the datastore.
const namespaceResourceType = "Scaler"

// syncNamespaces reconciles the tracker against the live entries.
//
// This replaces the ScaledObject reconciler, which tracked namespaces by watching
// the objects — and a watch is the cluster-wide LIST+WATCH this design removes.
// The registry already knows which namespaces have workloads in them, for the
// better reason: they are the namespaces WVA has actually been called about.
func (e *Enricher) syncNamespaces(live []Entry) {
	if e.Tracker == nil {
		return
	}
	now := make(map[string]string, len(live))
	for _, entry := range live {
		now[key(entry.Namespace, entry.Name)] = entry.Namespace
		e.Tracker.NamespaceTrack(namespaceResourceType, entry.Name, entry.Namespace)
	}
	for k, namespace := range e.tracked {
		if _, still := now[k]; still {
			continue
		}
		name := k[len(namespace)+1:]
		e.Tracker.NamespaceUntrack(namespaceResourceType, name, namespace)
	}
	e.tracked = now
}

// NewEnricher builds an Enricher over reader and reg.
//
// maxAge is clamped to a quarter of the registry's TTL. That is not a style
// preference: at parity an entry becomes stale enough to re-read at about the
// moment it expires, so it is evicted in the same pass that would have refreshed
// it and nothing is ever enriched. A quarter leaves room for a failing read to
// be retried several times within one entry lifetime.
func NewEnricher(reader client.Reader, reg *Registry, maxAge time.Duration) *Enricher {
	if reg == nil {
		reg = Default
	}
	if maxAge <= 0 {
		maxAge = DefaultTargetMaxAge
	}
	if ceiling := reg.TTL() / 4; ceiling > 0 && maxAge > ceiling {
		maxAge = ceiling
	}
	return &Enricher{Reader: reader, Registry: reg, MaxAge: maxAge, now: time.Now}
}

// Refresh reads the ScaledObject for every registered entry whose target read has
// gone stale, and records the result.
//
// Errors are not returned. One unreadable ScaledObject must not stop the others
// being refreshed, and an entry that fails to refresh keeps its last known target
// rather than losing it — the workload has not changed shape just because one
// read failed. A NotFound is different and is treated as such: the object is
// gone, so the entry is forgotten immediately instead of lingering for the rest
// of its TTL as a variant in the fleet the optimizer balances against.
func (e *Enricher) Refresh(ctx context.Context) {
	if e == nil || e.Reader == nil || e.Registry == nil {
		return
	}
	logger := log.FromContext(ctx)
	now := e.clock()

	live := e.Registry.Snapshot()
	e.syncNamespaces(live)

	var read, failed, gone int
	for _, entry := range live {
		if entry.Fresh(now, e.MaxAge) {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		target, err := e.readTarget(ctx, entry.Namespace, entry.Name)
		switch {
		case apierrors.IsNotFound(err):
			// KEDA can still be mid-reconcile against an object it has just lost,
			// so this is the authoritative signal that the workload is gone —
			// more so than the absence of calls, which only means "not yet".
			e.Registry.Forget(entry.Namespace, entry.Name)
			gone++
		case err != nil:
			logger.V(logging.DEBUG).Error(err, "Could not read the ScaledObject for a registered workload; keeping its last known target",
				"namespace", entry.Namespace, "scaledObject", entry.Name)
			failed++
		default:
			e.Registry.SetTarget(entry.Namespace, entry.Name, target)
			read++
		}
	}

	if read+failed+gone > 0 {
		logger.V(logging.DEBUG).Info("Refreshed registered workloads",
			"read", read, "failed", failed, "deleted", gone)
	}
}

// readTarget resolves one ScaledObject into the fields WVA needs.
func (e *Enricher) readTarget(ctx context.Context, namespace, name string) (Target, error) {
	var so kedav1alpha1.ScaledObject
	if err := e.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &so); err != nil {
		return Target{}, err
	}
	return TargetFromScaledObject(&so), nil
}

// TargetFromScaledObject extracts the scale target and replica envelope,
// applying KEDA's own defaults for an omitted kind and apiVersion so that a
// minimal `scaleTargetRef: {name: x}` resolves the same way KEDA resolves it.
func TargetFromScaledObject(so *kedav1alpha1.ScaledObject) Target {
	t := Target{
		Labels:      maps.Clone(so.Labels),
		MinReplicas: so.Spec.MinReplicaCount,
		// GetHPAMaxReplicas applies KEDA's default rather than reporting nil, so
		// an omitted maxReplicaCount reads as the ceiling KEDA will actually
		// enforce instead of "unbounded".
		MaxReplicas:            ptr(so.GetHPAMaxReplicas()),
		PollingIntervalSeconds: so.Spec.PollingInterval,
	}
	if a := so.Spec.Advanced; a != nil && a.HorizontalPodAutoscalerConfig != nil {
		if b := a.HorizontalPodAutoscalerConfig.Behavior; b != nil && b.ScaleDown != nil {
			t.ScaleDownWindowSeconds = b.ScaleDown.StabilizationWindowSeconds
		}
	}
	if so.Spec.ScaleTargetRef == nil {
		return t
	}
	t.Name = so.Spec.ScaleTargetRef.Name
	t.Kind = so.Spec.ScaleTargetRef.Kind
	if t.Kind == "" {
		t.Kind = constants.DeploymentKind
	}
	t.APIVersion = so.Spec.ScaleTargetRef.APIVersion
	if t.APIVersion == "" {
		t.APIVersion = "apps/v1"
	}
	return t
}

func ptr(v int32) *int32 { return &v }

func (e *Enricher) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}
