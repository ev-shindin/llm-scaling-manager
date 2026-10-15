// Package controllerstatus publishes what the controller has actually read and
// decided, as one ConfigMap in its own namespace, so tools and operators can
// confirm a policy write without reading the controller's log.
//
// # Data contract
//
// The ConfigMap is named wva-controller-status (ConfigMapName), lives in the
// controller's namespace (POD_NAMESPACE), and carries the labels
// app.kubernetes.io/name=workload-variant-autoscaler and
// app.kubernetes.io/component=controller-status. Every value is a string:
//
//	formatVersion          "1". Bumped only for an incompatible change to
//	                       these keys.
//	policyNamespace        the namespace whose scaling-policy ConfigMap is the
//	                       cluster policy: the one limiters and the optimizer
//	                       are read from.
//	policyConfigMap        that ConfigMap's name.
//	policyResourceVersion  metadata.resourceVersion of the policy ConfigMap the
//	                       controller last LOADED, accepted or rejected. Written
//	                       only after that content is applied in memory, so a
//	                       reader that wrote version V and reads V here knows
//	                       the controller is running with it. Empty when no
//	                       policy ConfigMap is in force (none exists, or it was
//	                       deleted). Compare for equality, not order:
//	                       resourceVersions are opaque.
//	policyAccepted         "true", or "false" when the ConfigMap's "default"
//	                       entry was rejected; the controller then keeps the
//	                       "default" entry previously in force, or runs
//	                       without one when there was none.
//	policyError            why the "default" entry was rejected; empty when
//	                       accepted.
//	limiter                the limiter in force, "<mode>/<entry names>": for
//	                       example "quota/install-quota",
//	                       "inventory/gpu-inventory", or "none".
//	utilizationShare       the optimizer's mode: off, invalid, shadow or
//	                       active.
//	utilizationShareError  the optimizer block's configuration error when the
//	                       mode is invalid; empty otherwise.
//	leader                 the pod that wrote this: POD_NAME, else the
//	                       hostname.
//	controllerInstance     the controller_instance metrics label, when set.
//	limiterBudgets         the GPU budget the steady-state engine used in its
//	                       last cycle, JSON (LimiterBudgets); absent until a
//	                       cycle with a limiter. See KeyLimiterBudgets.
//
// limiter and utilizationShare are recomputed from the applied configuration
// in the same update that records policyResourceVersion, so they never lag
// it. The steady-state engine republishes utilizationShare every cycle as a
// backstop; the ConfigMap is rewritten only when a value changes.
//
// # Writing
//
// Only the elected leader writes: Publisher is a manager Runnable that needs
// leader election, so a standby keeps the state in memory and writes it the
// moment it is elected. Content carries no timestamp that moves on its own
// (limiterBudgets' asOf moves only with its figures), and a write happens only
// when the content differs from what this process last wrote, so a quiet
// controller does not write at all. A failed write -- Forbidden included -- is
// logged and retried with backoff; it never stops the controller.
package controllerstatus

import (
	"context"
	"maps"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// ConfigMapName is the name of the controller-status ConfigMap.
const ConfigMapName = "wva-controller-status"

// FormatVersion is the value of KeyFormatVersion this package writes.
const FormatVersion = "1"

// Data keys of the controller-status ConfigMap. See the package documentation
// for what each one means.
const (
	KeyFormatVersion         = "formatVersion"
	KeyPolicyNamespace       = "policyNamespace"
	KeyPolicyConfigMap       = "policyConfigMap"
	KeyPolicyResourceVersion = "policyResourceVersion"
	KeyPolicyAccepted        = "policyAccepted"
	KeyPolicyError           = "policyError"
	KeyLimiter               = "limiter"
	KeyUtilizationShare      = "utilizationShare"
	KeyUtilizationShareError = "utilizationShareError"
	KeyLeader                = "leader"
	KeyControllerInstance    = "controllerInstance"
	// KeyBorrowReleaseLedger is the Kueue borrowing release ledger, JSON
	// (allocation.BorrowLedger): which releases of borrowed capacity to
	// waiting lenders are still in flight, so a restart does not release a
	// second unit for a lender the first has not reached yet. The steady-state
	// engine writes it and reads it back on start.
	KeyBorrowReleaseLedger = "borrowReleaseLedger"
)

// persistedKeys are the keys a new term carries over from the ConfigMap it
// finds, rather than overwriting with this process's empty start: state, not
// status.
var persistedKeys = []string{KeyBorrowReleaseLedger}

// Labels on the controller-status ConfigMap.
const (
	labelName      = "app.kubernetes.io/name"
	labelNameValue = "workload-variant-autoscaler"
	labelComponent = "app.kubernetes.io/component"
	componentValue = "controller-status"
)

const (
	defaultDebounce = time.Second
	defaultRetryMin = 2 * time.Second
	defaultRetryMax = time.Minute
	// errorRelogEvery bounds how often an unchanged write error is logged again.
	errorRelogEvery = 5 * time.Minute
	// conflictAttempts is how many times one flush re-reads and retries after
	// a resourceVersion conflict or a create that lost a race.
	conflictAttempts = 3
)

// Publisher holds the controller status and writes it to the
// controller-status ConfigMap. All methods are safe for concurrent use, and
// every setter is a no-op on a nil *Publisher, so components built without one
// (tests) need no guard.
type Publisher struct {
	writer    client.Client
	reader    client.Reader
	namespace string

	debounce time.Duration
	retryMin time.Duration
	retryMax time.Duration

	mu      sync.Mutex
	desired map[string]string
	// written is the content this process last confirmed in the cluster; nil
	// until the first successful flush.
	written map[string]string
	notify  chan struct{}

	errText   string
	errLogged time.Time

	// now is the clock reportErr throttles by; tests replace it.
	now func() time.Time
}

// New returns a Publisher that writes ConfigMapName in namespace through
// writer and reads it through reader. reader should bypass the manager's
// cache (mgr.GetAPIReader()): the cache may not hold the controller's own
// namespace. A nil reader reads through writer.
func New(writer client.Client, reader client.Reader, namespace string) *Publisher {
	if reader == nil {
		reader = writer
	}
	leader := os.Getenv("POD_NAME")
	if leader == "" {
		var err error
		if leader, err = os.Hostname(); err != nil {
			ctrl.Log.WithName("controller-status").Error(err,
				"Cannot determine the hostname; the controller-status leader key will be empty. Set POD_NAME (the downward API metadata.name) to name the writer.",
				"key", KeyLeader)
		}
	}
	return &Publisher{
		writer:    writer,
		reader:    reader,
		namespace: namespace,
		debounce:  defaultDebounce,
		retryMin:  defaultRetryMin,
		retryMax:  defaultRetryMax,
		notify:    make(chan struct{}, 1),
		now:       time.Now,
		desired: map[string]string{
			KeyFormatVersion:         FormatVersion,
			KeyPolicyNamespace:       config.SystemNamespace(),
			KeyPolicyConfigMap:       config.ScalingPolicyConfigMapName(),
			KeyPolicyResourceVersion: "",
			KeyPolicyAccepted:        strconv.FormatBool(true),
			KeyPolicyError:           "",
			KeyLimiter:               string(config.LimiterTypeNone),
			KeyUtilizationShare:      constants.UtilizationShareModeOff,
			KeyUtilizationShareError: "",
			KeyLeader:                leader,
			KeyControllerInstance:    metrics.GetControllerInstance(),
		},
	}
}

// RecordPolicy records that the policy ConfigMap namespace/name at
// resourceVersion has been applied, with rejected the reason its "default"
// entry was dropped (nil when it was accepted). An empty resourceVersion
// records that no policy ConfigMap is in force. It refreshes the values
// derived from cfg in the same update, so a reader never sees the new version
// beside the old limiter or optimizer mode. Call it only after the content is
// applied to cfg.
func (p *Publisher) RecordPolicy(cfg *config.Config, namespace, name, resourceVersion string, rejected error) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		observe(d, cfg)
		d[KeyPolicyNamespace] = namespace
		d[KeyPolicyConfigMap] = name
		d[KeyPolicyResourceVersion] = resourceVersion
		d[KeyPolicyAccepted] = strconv.FormatBool(rejected == nil)
		d[KeyPolicyError] = errText(rejected)
	})
}

// ObserveConfig refreshes the values derived from cfg -- the limiter and the
// utilization-share mode -- without touching the recorded policy version.
func (p *Publisher) ObserveConfig(cfg *config.Config) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) { observe(d, cfg) })
}

// SetUtilizationShare records the optimizer's mode, one of the
// constants.UtilizationShareMode* values, and its configuration error.
func (p *Publisher) SetUtilizationShare(mode string, err error) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) {
		d[KeyUtilizationShare] = mode
		d[KeyUtilizationShareError] = errText(err)
	})
}

// SetBorrowReleaseLedger records the borrowing release ledger (JSON).
func (p *Publisher) SetBorrowReleaseLedger(ledger string) {
	if p == nil {
		return
	}
	p.update(func(d map[string]string) { d[KeyBorrowReleaseLedger] = ledger })
}

// ReadKey reads one key of the controller-status ConfigMap in namespace
// through reader, which should bypass the manager's cache. found is false when
// the ConfigMap or the key is absent; any other failure is returned.
func ReadKey(ctx context.Context, reader client.Reader, namespace, key string) (value string, found bool, err error) {
	var cm corev1.ConfigMap
	err = reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ConfigMapName}, &cm)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value, found = cm.Data[key]
	return value, found, nil
}

// carryOver copies the persisted keys from the ConfigMap into the desired
// status where this process has not set them yet, so the first write of a
// term does not erase state another term persisted.
func (p *Publisher) carryOver(ctx context.Context) {
	for _, key := range persistedKeys {
		v, found, err := ReadKey(ctx, p.reader, p.namespace, key)
		if err != nil || !found {
			continue
		}
		p.mu.Lock()
		if _, set := p.desired[key]; !set {
			p.desired[key] = v
		}
		p.mu.Unlock()
	}
}

// Data returns a copy of the status as it will be written.
func (p *Publisher) Data() map[string]string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.desired)
}

// NeedLeaderElection reports that only the elected leader writes the status.
func (p *Publisher) NeedLeaderElection() bool { return true }

// Start writes the status, then rewrites it whenever it changes, until ctx is
// done. Changes arriving within the debounce interval are coalesced into one
// write. A failed write is logged and retried with backoff; Start never
// returns an error, because a status the controller cannot publish is no
// reason to stop scaling.
func (p *Publisher) Start(ctx context.Context) error {
	logger := ctrl.Log.WithName("controller-status")
	// A new term compares against the cluster, not against what an earlier
	// term of this process wrote: another leader may have written since.
	p.mu.Lock()
	p.written = nil
	// Read here, not in New: main builds the Publisher before InitMetrics
	// reads the instance from the environment.
	p.desired[KeyControllerInstance] = metrics.GetControllerInstance()
	p.mu.Unlock()
	p.carryOver(ctx)
	p.signal() // the first write after election, whatever is pending
	backoff := p.retryMin
	// One timer for the whole term, armed only while a write is failing. retry
	// is nil while disarmed, so a tick can never be received then; and Stop and
	// Reset (Go 1.23+) discard a tick that fired but was not received, so a
	// re-armed timer never delivers the previous attempt's tick early.
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.notify:
		case <-retry:
		}
		if !sleep(ctx, p.debounce) {
			return nil
		}
		if err := p.flush(ctx); err != nil {
			p.reportErr(logger, err)
			timer.Reset(backoff)
			retry = timer.C
			backoff = min(2*backoff, p.retryMax)
			continue
		}
		p.reportRecovered(logger)
		timer.Stop()
		retry, backoff = nil, p.retryMin
	}
}

// update applies mutate to the desired status and wakes the writer when it
// changed anything.
func (p *Publisher) update(mutate func(map[string]string)) {
	p.mu.Lock()
	before := maps.Clone(p.desired)
	mutate(p.desired)
	changed := !maps.Equal(before, p.desired)
	p.mu.Unlock()
	if changed {
		p.signal()
	}
}

// signal wakes Start without blocking; one pending wake covers any number.
func (p *Publisher) signal() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// flush writes the desired status unless it is what this process last wrote.
// It returns every API error but conflicts it could resolve by re-reading; a
// Forbidden is returned like any other, never treated as nothing to do, and
// the desired status is kept for the next attempt.
func (p *Publisher) flush(ctx context.Context) error {
	p.mu.Lock()
	want := maps.Clone(p.desired)
	same := p.written != nil && maps.Equal(want, p.written)
	p.mu.Unlock()
	if same {
		return nil
	}
	var err error
	for range conflictAttempts {
		if err = p.writeOnce(ctx, want); err == nil {
			p.mu.Lock()
			p.written = want
			p.mu.Unlock()
			return nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return err
}

// writeOnce reads the ConfigMap and creates it, or updates it at the version
// it read, when its data or labels differ from want.
func (p *Publisher) writeOnce(ctx context.Context, want map[string]string) error {
	var cm corev1.ConfigMap
	err := p.reader.Get(ctx, client.ObjectKey{Namespace: p.namespace, Name: ConfigMapName}, &cm)
	if apierrors.IsNotFound(err) {
		return p.writer.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ConfigMapName,
				Namespace: p.namespace,
				Labels:    map[string]string{labelName: labelNameValue, labelComponent: componentValue},
			},
			Data: maps.Clone(want),
		})
	}
	if err != nil {
		return err
	}
	if maps.Equal(cm.Data, want) && cm.Labels[labelName] == labelNameValue && cm.Labels[labelComponent] == componentValue {
		return nil
	}
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels[labelName] = labelNameValue
	cm.Labels[labelComponent] = componentValue
	cm.Data = maps.Clone(want)
	return p.writer.Update(ctx, &cm)
}

// reportErr logs a write failure at Error level when its text changes, and an
// unchanged one again only after errorRelogEvery, so a standing Forbidden is
// visible without flooding the log.
func (p *Publisher) reportErr(logger logr.Logger, err error) {
	p.mu.Lock()
	text := err.Error()
	now := p.now()
	relog := text != p.errText || now.Sub(p.errLogged) >= errorRelogEvery
	p.errText = text
	if relog {
		p.errLogged = now
	}
	p.mu.Unlock()
	if !relog {
		return
	}
	kv := []any{"configMap", ConfigMapName, "namespace", p.namespace}
	if apierrors.IsForbidden(err) {
		kv = append(kv, "hint", "the controller's ServiceAccount needs the wva-controller-status-role Role (get/update on this ConfigMap, create on configmaps)")
	}
	logger.Error(err, "Failed to write the controller-status ConfigMap; retrying", kv...)
	metrics.RecordError(constants.ComponentController, "Failed to write controller status")
}

// reportRecovered logs once that a write succeeded after failing.
func (p *Publisher) reportRecovered(logger logr.Logger) {
	p.mu.Lock()
	failed := p.errText != ""
	p.errText = ""
	p.mu.Unlock()
	if failed {
		logger.Info("Controller-status ConfigMap written after earlier failures",
			"configMap", ConfigMapName, "namespace", p.namespace)
	}
}

// observe sets the values derived from cfg.
func observe(d map[string]string, cfg *config.Config) {
	if cfg == nil {
		return
	}
	d[KeyPolicyNamespace] = cfg.PolicyNamespace()
	d[KeyLimiter] = cfg.EffectiveLimiterSummary()
	mode, err := cfg.UtilizationShareMode()
	d[KeyUtilizationShare] = mode
	d[KeyUtilizationShareError] = errText(err)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sleep waits d or until ctx is done, reporting whether the full wait elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
