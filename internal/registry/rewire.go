package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// RewireKey is the trigger metadata key a re-wire writes. The scaler ignores
// it (ParseMeta reads only the keys it knows). Changing it is a spec change,
// which is what makes KEDA ask for the metric spec again; an annotation is
// not. deploy/lib/scaler_tls.sh writes the same key for make
// scaledobjects-rewire.
const RewireKey = "wvaRewire"

const (
	// DefaultRewireGrace is how old a ScaledObject must be before its HPA is
	// judged. KEDA creates the HPA and asks for the metric spec within seconds
	// of a create; judging earlier would race it.
	DefaultRewireGrace = 60 * time.Second
	// DefaultRewireBackoff is the wait after the first re-wire of a
	// ScaledObject before the next one. It doubles per attempt.
	DefaultRewireBackoff = time.Minute
	// DefaultRewireMaxBackoff caps the doubling.
	DefaultRewireMaxBackoff = 30 * time.Minute
)

// Rewirer re-wires ScaledObjects whose HPA KEDA left without this scaler's
// External metric.
//
// KEDA asks an external scaler for a ScaledObject's metric spec only when it
// reconciles the ScaledObject: on a spec change, or when its operator starts.
// When that one call fails, KEDA builds the HPA with no External metric,
// Kubernetes defaults it to CPU, and nothing retries: the ScaledObject reads
// Ready and WVA's decisions reach nothing. KEDA still polls the scaler, so the
// workload is in the registry. Rewirer checks the HPA of each one the Enricher
// reads and, when a trigger naming this scaler has no metric on it, changes
// that trigger's RewireKey so KEDA asks again. It never deletes or recreates
// anything, and backs off per ScaledObject so it cannot hot-loop against a
// KEDA that keeps failing.
//
// Safe for concurrent use: the saturation and scale-from-zero loops each own
// an Enricher and may share one Rewirer.
type Rewirer struct {
	// Reader reads the HPA. It MUST be uncached (manager.GetAPIReader), for
	// the same reason as Enricher.Reader.
	Reader client.Reader
	// Writer patches the ScaledObject.
	Writer client.Writer
	// Grace, Backoff and MaxBackoff default to DefaultRewireGrace,
	// DefaultRewireBackoff and DefaultRewireMaxBackoff when zero.
	Grace      time.Duration
	Backoff    time.Duration
	MaxBackoff time.Duration
	// Nudged, if set, is called once per ScaledObject re-wired.
	Nudged func(namespace string)

	mu        sync.Mutex
	state     map[string]*rewireState
	forbidden bool
	// now is the clock, overridden in tests.
	now func() time.Time
}

// rewireState is the backoff of one ScaledObject.
type rewireState struct {
	attempts int
	next     time.Time
	seen     time.Time
}

// NewRewirer builds a Rewirer with the default timings.
func NewRewirer(reader client.Reader, writer client.Writer) *Rewirer {
	return &Rewirer{Reader: reader, Writer: writer}
}

// Check judges one ScaledObject, read by the Enricher for entry, and re-wires
// it when KEDA left a trigger naming this scaler without its metric.
func (r *Rewirer) Check(ctx context.Context, entry Entry, so *kedav1alpha1.ScaledObject) {
	if r == nil || r.Reader == nil || r.Writer == nil || so == nil {
		return
	}
	addr := entry.Metadata[ScalerAddressKey]
	if addr == "" {
		return
	}
	now := r.clock()
	if so.CreationTimestamp.IsZero() || now.Sub(so.CreationTimestamp.Time) < r.grace() {
		return
	}
	if so.NeedToBePausedByAnnotation() {
		return
	}
	if cond := so.Status.Conditions.GetPausedCondition(); cond.IsTrue() {
		return
	}
	indices := triggersNaming(so, addr)
	if len(indices) == 0 {
		return
	}
	k := key(so.Namespace, so.Name)

	logger := log.FromContext(ctx)
	var hpa autoscalingv2.HorizontalPodAutoscaler
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: so.Namespace, Name: hpaName(so)}, &hpa); err != nil {
		// Not found: KEDA has not built it yet, or the ScaledObject is paused.
		// Anything else: unknown, so nothing is patched.
		if !apierrors.IsNotFound(err) {
			logger.V(logging.DEBUG).Error(err, "Could not read the HPA of a ScaledObject; whether KEDA wired the scaler's metric is not checked",
				"namespace", so.Namespace, "scaledObject", so.Name)
		}
		return
	}
	unwired := unwiredTriggers(&hpa, indices)
	if len(unwired) == 0 {
		r.wired(k)
		return
	}
	// Judged on every read, so a ScaledObject KEDA wired forgets its backoff;
	// only the patch waits for it.
	if !r.due(k, now) {
		return
	}

	stamp := strconv.FormatInt(now.Unix(), 10)
	patch, err := rewirePatch(unwired, addr, stamp)
	if err != nil {
		return
	}
	attempt := r.record(k, now)
	if err := r.Writer.Patch(ctx, so, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		if apierrors.IsForbidden(err) {
			r.mu.Lock()
			first := !r.forbidden
			r.forbidden = true
			r.mu.Unlock()
			if first {
				logger.Info("WARNING: KEDA left this ScaledObject's HPA without the scaler's metric, and the controller may not patch ScaledObjects to make KEDA ask again; run make scaledobjects-rewire",
					"namespace", so.Namespace, "scaledObject", so.Name, "error", err.Error())
			}
			return
		}
		logger.Info("Could not re-wire a ScaledObject KEDA left without the scaler's metric; trying again later",
			"namespace", so.Namespace, "scaledObject", so.Name, "error", err.Error())
		return
	}
	logger.Info("KEDA left this ScaledObject's HPA without the scaler's metric; changed trigger metadata "+RewireKey+" so it asks again",
		"namespace", so.Namespace, "scaledObject", so.Name, "triggers", unwired, "attempt", attempt)
	if r.Nudged != nil {
		r.Nudged(so.Namespace)
	}
}

// triggersNaming returns the indices of the external triggers that dial addr.
func triggersNaming(so *kedav1alpha1.ScaledObject, addr string) []int {
	var out []int
	for i, t := range so.Spec.Triggers {
		if t.Type != "external" && t.Type != "external-push" {
			continue
		}
		if t.Metadata[ScalerAddressKey] == addr {
			out = append(out, i)
		}
	}
	return out
}

// unwiredTriggers returns the indices whose metric is not on the HPA. KEDA
// names a trigger's metric s<index>-<name>.
func unwiredTriggers(hpa *autoscalingv2.HorizontalPodAutoscaler, indices []int) []int {
	var out []int
	for _, i := range indices {
		prefix := fmt.Sprintf("s%d-", i)
		found := false
		for _, m := range hpa.Spec.Metrics {
			if m.Type == autoscalingv2.ExternalMetricSourceType && m.External != nil &&
				strings.HasPrefix(m.External.Metric.Name, prefix) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, i)
		}
	}
	return out
}

// hpaName is the HPA KEDA builds for so: status.hpaName, else the name KEDA
// derives.
func hpaName(so *kedav1alpha1.ScaledObject) string {
	if so.Status.HpaName != "" {
		return so.Status.HpaName
	}
	if a := so.Spec.Advanced; a != nil && a.HorizontalPodAutoscalerConfig != nil && a.HorizontalPodAutoscalerConfig.Name != "" {
		return a.HorizontalPodAutoscalerConfig.Name
	}
	return "keda-hpa-" + so.Name
}

// rewirePatch is the JSON patch that sets RewireKey on each trigger in
// indices. Each set is guarded by a test that the trigger still dials addr, so
// a trigger list edited since the read is refused rather than patched at the
// wrong index.
func rewirePatch(indices []int, addr, stamp string) ([]byte, error) {
	type op struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}
	ops := make([]op, 0, 2*len(indices))
	for _, i := range indices {
		base := fmt.Sprintf("/spec/triggers/%d/metadata/", i)
		ops = append(ops,
			op{Op: "test", Path: base + ScalerAddressKey, Value: addr},
			op{Op: "add", Path: base + RewireKey, Value: stamp})
	}
	return json.Marshal(ops)
}

// due reports whether k may be re-wired at now, and prunes ScaledObjects not
// judged for longer than the backoff cap.
func (r *Rewirer) due(k string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for other, st := range r.state {
		if other != k && now.Sub(st.seen) > 2*r.maxBackoff() {
			delete(r.state, other)
		}
	}
	st, ok := r.state[k]
	if !ok {
		return true
	}
	st.seen = now
	return !now.Before(st.next)
}

// record notes a re-wire of k at now, sets when the next may happen, and
// returns which attempt this is.
func (r *Rewirer) record(k string, now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = map[string]*rewireState{}
	}
	st, ok := r.state[k]
	if !ok {
		st = &rewireState{}
		r.state[k] = st
	}
	st.attempts++
	wait := r.backoff()
	for i := 1; i < st.attempts && wait < r.maxBackoff(); i++ {
		wait *= 2
	}
	wait = min(wait, r.maxBackoff())
	st.next = now.Add(wait)
	st.seen = now
	return st.attempts
}

// wired forgets k's backoff: KEDA wired it.
func (r *Rewirer) wired(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.state, k)
}

func (r *Rewirer) grace() time.Duration {
	if r.Grace > 0 {
		return r.Grace
	}
	return DefaultRewireGrace
}

func (r *Rewirer) backoff() time.Duration {
	if r.Backoff > 0 {
		return r.Backoff
	}
	return DefaultRewireBackoff
}

func (r *Rewirer) maxBackoff() time.Duration {
	if r.MaxBackoff > 0 {
		return r.MaxBackoff
	}
	return DefaultRewireMaxBackoff
}

func (r *Rewirer) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}
