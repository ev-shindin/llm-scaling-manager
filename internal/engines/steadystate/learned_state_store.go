package steadystate

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
)

// The saturation analyzer's learned state survives a controller restart in one
// ConfigMap in the controller's namespace, when LEARNED_STATE_SNAPSHOT is on.
// See docs/proposals/learned-state-snapshot.md for the design and why it is a
// ConfigMap and not a metric read back or a refit from the engines' series.
//
// Only the acting (leader) controller runs a cycle, so only it restores and
// writes. It restores once, on its first cycle, before anything is learned or
// swept; until a read has succeeded it writes nothing, so a store it could not
// read is never overwritten from an empty memory. It writes at most once per
// learnedStateInterval, when what was learned changed or learnedStateRefresh
// has passed, in one read-modify-write with the ConfigMap's resourceVersion as
// the concurrency check. Deleting the
// ConfigMap is a flush: a controller that has seen it and finds it gone forgets
// what it knows rather than writing it straight back.
const (
	// learnedStateConfigMap is the ConfigMap's name. The controller's Role
	// (config/base/rbac/learned-state-role.yaml) grants get and update on this
	// name only.
	learnedStateConfigMap = "wva-learned-state"
	// learnedStateVersion is the document's format version, and part of its
	// data key: a document of another version is left where it is, untouched,
	// so a controller rolled back to that version still finds its own.
	learnedStateVersion = 1
	// learnedStateInterval is the least time between two writes, and between
	// two restore attempts after a failed read.
	learnedStateInterval = time.Minute
	// learnedStateRefresh is the most time between two writes while anything
	// is stored. Reading a figure moves its last-used clock, and a change of
	// clock alone is not worth a write a minute -- the horizons it feeds are
	// days -- but the stored clocks must not fall far behind either.
	learnedStateRefresh = 15 * time.Minute
	// learnedStateMaxBytes bounds the document. learnedStateMaxData bounds all
	// the ConfigMap's data, other versions' documents included, below the API
	// server's 1 MiB object limit with room for its metadata: this version's
	// document gets what the others leave, up to learnedStateMaxBytes.
	learnedStateMaxBytes = 700 << 10
	learnedStateMaxData  = 900 << 10
	// learnedStateBoundHold is how long, from the first restore attempt, the
	// lower bounds are held while the snapshot cannot be read
	// (SaturationAnalyzer.HoldLowerBounds): long enough for a transient
	// failure to pass, short enough that a store that stays unreadable --
	// a missing Role -- does not keep every lower bound at full strength.
	learnedStateBoundHold = 10 * time.Minute
	// learnedStateFlushTimeout bounds the last write as the loop stops: the
	// manager waits for runnables only so long before it releases the lease.
	learnedStateFlushTimeout = 5 * time.Second
)

// learnedStateKey is the ConfigMap data key this version's document lives
// under.
func learnedStateKey() string { return fmt.Sprintf("v%d", learnedStateVersion) }

// learnedStateDoc is the stored document.
type learnedStateDoc struct {
	Version   int                     `json:"version"`
	WrittenAt time.Time               `json:"writtenAt"`
	Writer    learnedStateWriter      `json:"writer"`
	State     saturation.LearnedState `json:"state"`
}

// learnedStateWriter names the controller that wrote a document. A restore
// rejects a document with no writer, or one from another namespace: a ConfigMap
// in this namespace can be that only if it was copied or edited by hand. A
// guard against accident; an actor who can write the controller's ConfigMaps
// can write this too.
type learnedStateWriter struct {
	Namespace string `json:"namespace"`
	Identity  string `json:"identity"`
}

// learnedStateStoreState is the engine's between-cycle state for the store.
type learnedStateStoreState struct {
	// restored is set once a read has succeeded -- the document restored,
	// rejected, or absent. Until then nothing is written.
	restored bool
	// seen is set once the ConfigMap has been read or written, so that
	// finding it gone afterwards is a flush rather than a first start.
	seen    bool
	lastTry time.Time
	// firstTry is the first restore attempt, which the lower bounds' hold
	// runs from (learnedStateBoundHold).
	firstTry time.Time
	// lastWrite is the last write attempt, which paces the reads;
	// lastWriteDone the last write that landed, which paces the refresh.
	lastWrite     time.Time
	lastWriteDone time.Time
	lastDigest    string
	// lastReadErr and lastWriteErr are the last failure of each operation,
	// "" when it last worked (reportLearnedStateErr).
	lastReadErr  string
	lastWriteErr string
}

// learnedStater is the part of the saturation analyzer the store needs, one
// of its capabilities in the registry (saturationCapabilities).
type learnedStater interface {
	ExportLearnedState() saturation.LearnedState
	RestoreLearnedState(saturation.LearnedState) saturation.RestoreReport
	ForgetLearnedState()
	TakeRestoreOutcomes() map[string]int
	HoldLowerBounds(held bool)
}

func (e *Engine) learnedStateReader() client.Reader {
	if e.apiReader != nil {
		return e.apiReader
	}
	return e.client
}

func learnedStateIdentity() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

// reportLearnedStateErr logs a failure of one store operation (op, read or
// write) at Error once per change of its error, and its recovery once.
//
// Per operation: every write is preceded by a read, and the read may work
// where the write does not -- the manager Role reads ConfigMaps in the
// controller's namespace in every install mode, and only the learned-state
// Role writes this one. Kept in one slot, each minute's read
// cleared the write's error, so a store that could not be written logged
// "works again" and the same Forbidden every minute, which read as a store
// recovering, not as one that never would.
func (e *Engine) reportLearnedStateErr(logger logr.Logger, op string, err error, msg string) {
	st := &e.learnedState
	last, recovered := &st.lastReadErr, "Learned state: the snapshot can be read again"
	if op == metrics.LearnedStateOpWrite {
		last, recovered = &st.lastWriteErr, "Learned state: the snapshot can be written again"
	}
	text := ""
	if err != nil {
		text = err.Error()
	}
	if text == *last {
		return
	}
	prev := *last
	*last = text
	kv := []any{"configMap", learnedStateConfigMap, "namespace", config.SystemNamespace()}
	if err != nil {
		logger.Error(err, msg, kv...)
		return
	}
	if prev != "" {
		logger.Info(recovered, kv...)
	}
}

func (e *Engine) readLearnedState(ctx context.Context) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := e.learnedStateReader().Get(ctx, client.ObjectKey{Namespace: config.SystemNamespace(), Name: learnedStateConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cm, nil
}

// restoreLearnedState runs at the start of a cycle until a read has
// succeeded. A read that fails -- Forbidden included -- restores nothing and
// is retried; it is never taken as an empty store. Until a read succeeds, for
// at most learnedStateBoundHold, the analyzer's lower bounds are held: a
// cycle in between would arm one the restore then kept over the stored one.
func (e *Engine) restoreLearnedState(ctx context.Context, now time.Time) {
	if e.Config == nil || !e.Config.LearnedStateSnapshotEnabled() {
		return
	}
	st := &e.learnedState
	if st.restored {
		return
	}
	analyzer := e.analyzers.saturationCaps().learned
	if analyzer == nil {
		return
	}
	if st.firstTry.IsZero() {
		st.firstTry = now
	}
	analyzer.HoldLowerBounds(now.Sub(st.firstTry) < learnedStateBoundHold)
	if !st.lastTry.IsZero() && now.Sub(st.lastTry) < learnedStateInterval {
		return
	}
	st.lastTry = now
	logger := ctrl.LoggerFrom(ctx).WithName("learned-state")
	cm, err := e.readLearnedState(ctx)
	if err != nil {
		e.metricsEmitter.IncLearnedStateSnapshotError(metrics.LearnedStateOpRead)
		e.reportLearnedStateErr(logger, metrics.LearnedStateOpRead, err, "Learned state: could not read the snapshot; nothing restored, and nothing will be written until a read succeeds")
		return
	}
	e.reportLearnedStateErr(logger, metrics.LearnedStateOpRead, nil, "")
	st.restored = true
	// Released before this cycle's analysis, which runs after the restore
	// below.
	analyzer.HoldLowerBounds(false)
	if cm == nil {
		logger.Info("Learned state: no snapshot to restore", "configMap", learnedStateConfigMap)
		return
	}
	st.seen = true
	raw, ok := cm.Data[learnedStateKey()]
	if !ok {
		logger.Info("Learned state: the snapshot holds no document of this version", "version", learnedStateVersion)
		return
	}
	var doc learnedStateDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		e.metricsEmitter.IncLearnedStateSnapshotError(metrics.LearnedStateOpRead)
		logger.Error(err, "Learned state: the snapshot could not be read; nothing restored, and it will be replaced")
		return
	}
	if why := doc.rejected(); why != "" {
		logger.Info("Learned state: snapshot not restored", "reason", why,
			"writerNamespace", doc.Writer.Namespace, "writer", doc.Writer.Identity)
		return
	}
	rep := analyzer.RestoreLearnedState(doc.State)
	logger.Info("Learned state: snapshot restored",
		"writtenAt", doc.WrittenAt, "writer", doc.Writer.Identity,
		"pendingVariants", rep.PendingVariants, "itlLines", rep.ITLLines,
		"aged", rep.Aged, "unusable", rep.Unusable, "unattributable", rep.Unattributable,
		"lowerBounds", rep.BorrowClocks, "forgotten", rep.Forgotten, "stashed", rep.Stashed)
}

// rejected says why a document will not be restored, or "".
func (d learnedStateDoc) rejected() string {
	switch {
	case d.Version != learnedStateVersion:
		return fmt.Sprintf("version %d, this controller reads %d", d.Version, learnedStateVersion)
	case d.Writer.Identity == "" || d.Writer.Namespace == "":
		return "the document names no writer"
	case d.Writer.Namespace != config.SystemNamespace():
		return "written by a controller in another namespace"
	}
	return ""
}

// writeLearnedState writes the learned state, at most once per interval and
// only when it changed.
func (e *Engine) writeLearnedState(ctx context.Context, now time.Time) {
	if e.Config == nil || !e.Config.LearnedStateSnapshotEnabled() {
		return
	}
	st := &e.learnedState
	// Every cycle, not every interval: the restores the analyzer decided this
	// cycle are counted in this cycle's scrape.
	if a := e.analyzers.saturationCaps().learned; a != nil {
		e.metricsEmitter.AddLearnedStateRestores(a.TakeRestoreOutcomes())
	}
	if !st.restored || (!st.lastWrite.IsZero() && now.Sub(st.lastWrite) < learnedStateInterval) {
		return
	}
	analyzer := e.analyzers.saturationCaps().learned
	if analyzer == nil {
		return
	}
	logger := ctrl.LoggerFrom(ctx).WithName("learned-state")
	st.lastWrite = now

	// Read every interval, written or not: a flush must be noticed within an
	// interval, not at the next refresh.
	cm, err := e.readLearnedState(ctx)
	if err != nil {
		e.metricsEmitter.IncLearnedStateSnapshotError(metrics.LearnedStateOpRead)
		e.reportLearnedStateErr(logger, metrics.LearnedStateOpRead, err, "Learned state: could not read the snapshot to update it; not written")
		return
	}
	// A read that works is the store working, whether or not a write follows.
	e.reportLearnedStateErr(logger, metrics.LearnedStateOpRead, nil, "")
	if cm == nil && st.seen {
		// The operator deleted it. Written back from memory, the deletion
		// would undo nothing.
		analyzer.ForgetLearnedState()
		st.seen, st.lastDigest = false, ""
		logger.Info("Learned state: the snapshot was deleted; everything learned is forgotten, as after a restart with no store",
			"configMap", learnedStateConfigMap)
		return
	}
	state := analyzer.ExportLearnedState()
	digest := learnedStateDigest(state)
	if digest == st.lastDigest && now.Sub(st.lastWriteDone) < learnedStateRefresh {
		return
	}
	others := 0
	if cm != nil {
		for k, v := range cm.Data {
			if k != learnedStateKey() {
				others += len(k) + len(v)
			}
		}
	}
	budget := max(min(learnedStateMaxBytes, learnedStateMaxData-others), 0)
	raw, dropped, err := encodeLearnedState(state, now, budget)
	if err != nil {
		e.metricsEmitter.IncLearnedStateSnapshotError(metrics.LearnedStateOpWrite)
		e.reportLearnedStateErr(logger, metrics.LearnedStateOpWrite, err, "Learned state: the snapshot does not fit its bound beside the other versions' documents; not written")
		return
	}
	if cm == nil {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: learnedStateConfigMap, Namespace: config.SystemNamespace(),
			Labels: map[string]string{"app.kubernetes.io/component": "learned-state",
				"app.kubernetes.io/part-of": "workload-variant-autoscaler"},
		}}
		cm.Data = map[string]string{learnedStateKey(): raw}
		err = e.client.Create(ctx, cm)
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[learnedStateKey()] = raw
		err = e.client.Update(ctx, cm)
	}
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			// Another writer moved first; this state goes next interval. Two
			// writers alternating is two controllers acting at once -- leader
			// election off with more than one replica -- so it is logged.
			logger.V(1).Info("Learned state: another writer updated the snapshot first; retrying next interval",
				"reason", apierrors.ReasonForError(err))
			st.lastDigest = ""
			return
		}
		e.metricsEmitter.IncLearnedStateSnapshotError(metrics.LearnedStateOpWrite)
		e.reportLearnedStateErr(logger, metrics.LearnedStateOpWrite, err, "Learned state: could not write the snapshot")
		return
	}
	e.reportLearnedStateErr(logger, metrics.LearnedStateOpWrite, nil, "")
	e.metricsEmitter.RecordLearnedStateSnapshotSize(len(raw), budget, dropped)
	st.seen, st.lastDigest, st.lastWriteDone = true, digest, now
	if dropped > 0 {
		logger.Info("Learned state: the snapshot was over its size bound; the least recently used entries were left out",
			"dropped", dropped, "maxBytes", budget)
	}
}

// flushLearnedState writes the learned state once more as the optimize loop
// stops, whatever the interval and the digest say. Without it, a planned
// upgrade or a leader hand-off lost up to a minute of readings and up to
// learnedStateRefresh of lower-bound clock progress: the digest leaves the
// clocks out, so they are written only on the refresh, and the next leader
// read the time since then as a pause, holding bounds at full strength
// longer. Best effort, on its own short context: the loop's is cancelled.
//
// It only updates a snapshot this controller has read or written (seen),
// never creates one. A snapshot found deleted was forgotten, and a flush that
// created it from what was relearned in the seconds since undid the deletion
// -- a benchmark reset by deleting it and restarting started warm. A store
// that has not written yet loses at most its first interval.
func (e *Engine) flushLearnedState() {
	if e.Config == nil || !e.Config.LearnedStateSnapshotEnabled() || !e.learnedState.restored || !e.learnedState.seen {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), learnedStateFlushTimeout)
	defer cancel()
	st := &e.learnedState
	st.lastWrite, st.lastDigest = time.Time{}, ""
	// The wall clock, not the cycle clock: the loop has stopped, so nothing
	// is pinned, and the cadence it would be compared with was just reset.
	e.writeLearnedState(ctx, time.Now())
}

// learnedStateDigest identifies what was learned, so a state that changed only
// in the clocks a read moves is not rewritten every interval: the windows' and
// the memo's last-used clocks and the variants' last-seen times are left out.
// learnedStateRefresh writes them anyway, often enough for horizons of days.
func learnedStateDigest(s saturation.LearnedState) string {
	s = cloneLearnedState(s)
	for k, w := range s.K2 {
		w.LastUsed = time.Time{}
		s.K2[k] = w
	}
	for k, w := range s.Throughput {
		w.LastUsed = time.Time{}
		s.Throughput[k] = w
	}
	for k, w := range s.K2Stash {
		w.LastUsed = time.Time{}
		s.K2Stash[k] = w
	}
	// A put-aside throughput window's held time moves on every serving cycle
	// the floor holds a fleet on it; the same trade as a bound's tick.
	for k, w := range s.ThroughputStash {
		w.LastUsed, w.HeldSeconds = time.Time{}, 0
		s.ThroughputStash[k] = w
	}
	for k, m := range s.Accelerators {
		m.LastUsed = time.Time{}
		s.Accelerators[k] = m
	}
	for k := range s.VariantSeenAt {
		s.VariantSeenAt[k] = time.Time{}
	}
	// A lower bound's tick moves on every serving cycle and its seen on every
	// cycle its variant exists, as a forget mark's seen does: kept in, they
	// changed the digest every interval and rewrote the document every
	// minute for as long as any bound or forget existed. Left out, a running
	// bound's tick is written at least every learnedStateRefresh; a restart
	// reads the part since then as a pause, which keeps the bound slightly
	// longer, never shorter. The start, which a pause moves, stays in.
	for k, lb := range s.LowerBounds {
		lb.Tick, lb.Seen, lb.PriorUsed = time.Time{}, time.Time{}, time.Time{}
		s.LowerBounds[k] = lb
	}
	// A fitted ITL line is republished every cycle a variant fits one: its
	// learnedAt moves every cycle and its A and B drift in their last digits
	// with every refit, so kept in, any decode fleet rewrote the document
	// every minute. Left out, and rounded to three significant figures, a
	// line that really moved is still written; one that only refitted is
	// written with the refresh.
	for k, l := range s.ITLLines {
		l.LearnedAt, l.A, l.B = time.Time{}, sig3(l.A), sig3(l.B)
		s.ITLLines[k] = l
	}
	for k, b := range s.ITLBaseline {
		s.ITLBaseline[k] = sig3(b)
	}
	// A slope is refitted, and restamped, every cycle a variant fits one. As
	// for a line, its fit time is left out of the digest and its A rounded,
	// so a refit that moves neither is written with the refresh: after a
	// crash the restored fit time can be up to one refresh early, which ends
	// the hold that much sooner, never later.
	for k, sl := range s.ITLSlopes {
		sl.FittedAt, sl.A = time.Time{}, sig3(sl.A)
		s.ITLSlopes[k] = sl
	}
	for k, m := range s.Forgotten {
		m.Seen = time.Time{}
		s.Forgotten[k] = m
	}
	// The rollback copy of a previous figure is dated from its last use,
	// which moves with the bound's PriorUsed.
	for k, p := range s.PriorK2 {
		p.InstalledAt = time.Time{}
		s.PriorK2[k] = p
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// sig3 rounds x to three significant figures, for the digest only.
func sig3(x float64) float64 {
	v, err := strconv.ParseFloat(strconv.FormatFloat(x, 'g', 3, 64), 64)
	if err != nil {
		return x
	}
	return v
}

// encodeLearnedState encodes the document, leaving out the least recently used
// entries until it fits the bound, and reports how many it left out. The
// caller's state is not modified: the trimming works on a copy of its maps.
func encodeLearnedState(s saturation.LearnedState, now time.Time, budget int) (string, int, error) {
	s = cloneLearnedState(s)
	doc := learnedStateDoc{
		Version:   learnedStateVersion,
		WrittenAt: now,
		Writer:    learnedStateWriter{Namespace: config.SystemNamespace(), Identity: learnedStateIdentity()},
		State:     s,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", 0, err
	}
	if len(raw) <= budget {
		return string(raw), 0, nil
	}
	// The fewest entries, least recently used first, whose leaving out makes
	// it fit: a binary search over how many, each try on a fresh copy, since
	// a dropped entry cannot be put back. Dropped in chunks of a tenth, a
	// document one byte over its bound lost a tenth of what it had learned.
	orig := doc.State
	try := func(n int) ([]byte, error) {
		doc.State = cloneLearnedState(orig)
		for _, e := range learnedStateEntries(doc.State)[:n] {
			e.drop()
		}
		return json.Marshal(doc)
	}
	total := len(learnedStateEntries(orig))
	if raw, err = try(total); err != nil {
		return "", 0, err
	}
	if len(raw) > budget {
		return "", total, fmt.Errorf("the learned-state snapshot is %d bytes with every entry left out, over its %d-byte bound", len(raw), budget)
	}
	lo, hi := 0, total // lo does not fit, hi does
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		r, err := try(mid)
		if err != nil {
			return "", 0, err
		}
		if len(r) <= budget {
			hi = mid
		} else {
			lo = mid
		}
	}
	if raw, err = try(hi); err != nil {
		return "", 0, err
	}
	return string(raw), hi, nil
}

// cloneLearnedState copies every map of the state, so leaving entries out of
// the copy cannot reach the original. Every map field, by reflection: a list
// of fields written by hand missed two of the four the learned-state PRs
// added, and the trim then reached the caller's maps. The values are plain
// data, except the window values, which nothing here writes into.
func cloneLearnedState(s saturation.LearnedState) saturation.LearnedState {
	v := reflect.ValueOf(&s).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Map || f.IsNil() {
			continue
		}
		c := reflect.MakeMapWithSize(f.Type(), f.Len())
		for it := f.MapRange(); it.Next(); {
			c.SetMapIndex(it.Key(), it.Value())
		}
		f.Set(c)
	}
	return s
}

// learnedStateEntry is one entry of the state, with the clock that says how
// recently it was used and how to leave it out.
type learnedStateEntry struct {
	// spare is an entry that goes before every other whatever its clock: a
	// spent lower bound holding no figure (learnedStateEntries).
	spare bool
	at    time.Time
	// key breaks a tie between two clocks, so the order -- and the entries a
	// trim leaves out -- is the same on every call.
	key  string
	drop func()
}

// learnedStateEntries lists the state's entries, least recently used first.
func learnedStateEntries(s saturation.LearnedState) []learnedStateEntry {
	out := make([]learnedStateEntry, 0, len(s.K2)+len(s.Throughput)+len(s.VariantSeenAt)+
		len(s.ITLLines)+len(s.Accelerators)+len(s.LowerBounds)+len(s.K2BorrowedSince)+len(s.Forgotten)+
		len(s.K2Stash)+len(s.ThroughputStash))
	// A window goes with the arguments it was written under.
	for k, w := range s.K2 {
		out = append(out, learnedStateEntry{at: w.LastUsed, key: "k2|" + k, drop: func() {
			delete(s.K2, k)
			delete(s.K2Args, k)
		}})
	}
	for k, w := range s.Throughput {
		out = append(out, learnedStateEntry{at: w.LastUsed, key: "throughput|" + k, drop: func() {
			delete(s.Throughput, k)
			delete(s.ThroughputArgs, k)
		}})
	}
	// A window put aside goes on its own clock, like the windows.
	for k, w := range s.K2Stash {
		out = append(out, learnedStateEntry{at: w.LastUsed, key: "k2stash|" + k, drop: func() { delete(s.K2Stash, k) }})
	}
	for k, w := range s.ThroughputStash {
		out = append(out, learnedStateEntry{at: w.LastUsed, key: "throughputstash|" + k, drop: func() { delete(s.ThroughputStash, k) }})
	}
	for k, seen := range s.VariantSeenAt {
		out = append(out, learnedStateEntry{at: seen, key: "seen|" + k, drop: func() {
			delete(s.VariantSeenAt, k)
			delete(s.ITLBaseline, k)
			delete(s.ITLSlopes, k)
			delete(s.StartSeconds, k)
			maps.DeleteFunc(s.ITLBaselineStash, func(sk string, _ float64) bool { return strings.HasPrefix(sk, k+"#") })
		}})
	}
	// A put-aside ITL baseline goes with its variant's liveness; one whose
	// variant left none goes first, as a spent bound does.
	for k := range s.ITLBaselineStash {
		if base, _, _ := strings.Cut(k, "#"); s.VariantSeenAt[base].IsZero() {
			out = append(out, learnedStateEntry{spare: true, key: "itlstash|" + k, drop: func() { delete(s.ITLBaselineStash, k) }})
		}
	}
	// A held ITL slope goes with its variant's liveness, as its baseline
	// does; one whose variant left none goes first.
	for k := range s.ITLSlopes {
		if s.VariantSeenAt[k].IsZero() {
			out = append(out, learnedStateEntry{spare: true, key: "itlslope|" + k, drop: func() { delete(s.ITLSlopes, k) }})
		}
	}
	for k, l := range s.ITLLines {
		out = append(out, learnedStateEntry{at: l.LearnedAt, key: "itl|" + k, drop: func() { delete(s.ITLLines, k) }})
	}
	for k, m := range s.Accelerators {
		out = append(out, learnedStateEntry{at: m.LastUsed, key: "accel|" + k, drop: func() { delete(s.Accelerators, k) }})
	}
	// A lower bound is one entry, ordered by when its variant was last seen,
	// not by its start: a running bound, or one holding a previous figure, is
	// kept for its variant's life, so its start is the oldest time in the
	// document and would go first. Its rollback copies (K2BorrowedSince,
	// PriorK2) go with it. A SPENT bound holding no figure goes before
	// everything else: it only keeps a borrow from re-arming without new
	// evidence, which costs replicas for a bounded time, where a k2 window
	// dropped for it priced its bucket at derived or k1 when the traffic came
	// back. Ordered by Seen, which NoteVariantsPresent refreshes every cycle
	// a variant exists, every spent bound outlived the windows of buckets
	// served two days ago. A spent bound is exported ARMED, its start a
	// whole hold and relaxation before its last tick (the analyzer keeps
	// it so, to judge new evidence against); one never armed holding no
	// figure is as dispensable.
	//
	// Among themselves the spent bounds go oldest last serving cycle first,
	// and a bound still running or holding a figure is ordered by its own
	// last use -- its last serving cycle, or the figure's last use -- not by
	// Seen: a bound under a key the variant no longer serves (a retuned
	// queue threshold, another GPU count) is refreshed every cycle by its
	// variant's liveness, and ordered by Seen it outlived the windows of
	// buckets in use. A spent bound on a bucket still served is ticked every
	// serving cycle, so it goes last of the spent ones.
	for k, lb := range s.LowerBounds {
		e := learnedStateEntry{at: lb.Seen, key: "bound|" + k}
		switch {
		case lb.Prior <= 0 && (!lb.Armed || spentBound(lb)):
			e.spare, e.at = true, lb.Tick
		case lb.Armed && !lb.Tick.IsZero():
			// Its clock waits while the variant idles, its figure's last use
			// does not: one in use overnight is not one a retuned key left.
			e.at = lb.Tick
			if lb.PriorUsed.After(e.at) {
				e.at = lb.PriorUsed
			}
		case !lb.Armed && !lb.PriorUsed.IsZero():
			e.at = lb.PriorUsed
		}
		out = append(out, learnedStateEntry{spare: e.spare, at: e.at, key: e.key, drop: func() {
			delete(s.LowerBounds, k)
			delete(s.K2BorrowedSince, k)
			delete(s.PriorK2, k)
		}})
	}
	for k, since := range s.K2BorrowedSince {
		if _, bound := s.LowerBounds[k]; bound {
			continue
		}
		out = append(out, learnedStateEntry{at: since, key: "clock|" + k, drop: func() { delete(s.K2BorrowedSince, k) }})
	}
	for k, m := range s.Forgotten {
		out = append(out, learnedStateEntry{at: m.Seen, key: "forgotten|" + k, drop: func() { delete(s.Forgotten, k) }})
	}
	// A PR #151 document's previous figures, where no bound holds the key;
	// one a bound holds goes with it.
	for k, p := range s.PriorK2 {
		if _, bound := s.LowerBounds[k]; bound {
			continue
		}
		out = append(out, learnedStateEntry{at: p.InstalledAt, key: "priork2|" + k, drop: func() { delete(s.PriorK2, k) }})
	}
	slices.SortFunc(out, func(a, b learnedStateEntry) int {
		if a.spare != b.spare {
			if a.spare {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.at.UnixNano(), b.at.UnixNano()), cmp.Compare(a.key, b.key))
	})
	return out
}

// spentBound reports an armed lower bound whose hold and relaxation have run
// their course: its serving time, tick minus since, is the whole of both.
func spentBound(lb saturation.LowerBoundSnapshot) bool {
	return lb.Armed && !lb.Since.IsZero() && !lb.Tick.IsZero() &&
		lb.Tick.Sub(lb.Since) >= saturation.BorrowedK2Hold+saturation.BorrowedK2Relax
}
