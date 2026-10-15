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
	"slices"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
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
	// lastWrite is the last write attempt, which paces the reads;
	// lastWriteDone the last write that landed, which paces the refresh.
	lastWrite     time.Time
	lastWriteDone time.Time
	lastDigest    string
	lastErr       string
}

// learnedStater is the part of the saturation analyzer the store needs,
// reached through an assertion for the same reason as staleHistoryEvictor.
type learnedStater interface {
	ExportLearnedState() saturation.LearnedState
	RestoreLearnedState(saturation.LearnedState) saturation.RestoreReport
	ForgetLearnedState()
}

// Pinned at compile time: the assertion below fails silently at run time.
var _ learnedStater = (*saturation.SaturationAnalyzer)(nil)

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

// reportLearnedStateErr logs a store failure at Error once per change of the
// error, and its recovery once.
func (e *Engine) reportLearnedStateErr(logger logr.Logger, err error, msg string) {
	st := &e.learnedState
	text := ""
	if err != nil {
		text = err.Error()
	}
	if text == st.lastErr {
		return
	}
	prev := st.lastErr
	st.lastErr = text
	kv := []any{"configMap", learnedStateConfigMap, "namespace", config.SystemNamespace()}
	if err != nil {
		logger.Error(err, msg, kv...)
		return
	}
	if prev != "" {
		logger.Info("Learned state: the snapshot store works again", kv...)
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
// is retried; it is never taken as an empty store.
func (e *Engine) restoreLearnedState(ctx context.Context, now time.Time) {
	if e.Config == nil || !e.Config.LearnedStateSnapshotEnabled() {
		return
	}
	st := &e.learnedState
	if st.restored || (!st.lastTry.IsZero() && now.Sub(st.lastTry) < learnedStateInterval) {
		return
	}
	analyzer, ok := e.saturationV2Analyzer.(learnedStater)
	if !ok {
		return
	}
	st.lastTry = now
	logger := ctrl.LoggerFrom(ctx).WithName("learned-state")
	cm, err := e.readLearnedState(ctx)
	if err != nil {
		e.reportLearnedStateErr(logger, err, "Learned state: could not read the snapshot; nothing restored, and nothing will be written until a read succeeds")
		return
	}
	e.reportLearnedStateErr(logger, nil, "")
	st.restored = true
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
		"borrowClocks", rep.BorrowClocks, "forgotten", rep.Forgotten)
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
	if !st.restored || (!st.lastWrite.IsZero() && now.Sub(st.lastWrite) < learnedStateInterval) {
		return
	}
	analyzer, ok := e.saturationV2Analyzer.(learnedStater)
	if !ok {
		return
	}
	logger := ctrl.LoggerFrom(ctx).WithName("learned-state")
	st.lastWrite = now

	// Read every interval, written or not: a flush must be noticed within an
	// interval, not at the next refresh.
	cm, err := e.readLearnedState(ctx)
	if err != nil {
		e.reportLearnedStateErr(logger, err, "Learned state: could not read the snapshot to update it; not written")
		return
	}
	// A read that works is the store working, whether or not a write follows.
	e.reportLearnedStateErr(logger, nil, "")
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
		e.reportLearnedStateErr(logger, err, "Learned state: the snapshot does not fit its bound beside the other versions' documents; not written")
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
		e.reportLearnedStateErr(logger, err, "Learned state: could not write the snapshot")
		return
	}
	e.reportLearnedStateErr(logger, nil, "")
	st.seen, st.lastDigest, st.lastWriteDone = true, digest, now
	if dropped > 0 {
		logger.Info("Learned state: the snapshot was over its size bound; the least recently used entries were left out",
			"dropped", dropped, "maxBytes", budget)
	}
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
	for k, m := range s.Accelerators {
		m.LastUsed = time.Time{}
		s.Accelerators[k] = m
	}
	for k := range s.VariantSeenAt {
		s.VariantSeenAt[k] = time.Time{}
	}
	// A borrow clock's tick moves on every serving cycle and its seen on
	// every cycle its variant exists, as a forget mark's seen does: kept in,
	// they changed the digest every interval and rewrote the document every
	// minute for as long as any borrow or forget existed. Left out, a running
	// borrow's tick is written at least every learnedStateRefresh; a restart
	// reads the part since then as a pause, which keeps the borrow slightly
	// longer, never shorter. The start, which a pause moves, stays in.
	for k, st := range s.K2BorrowState {
		st.Tick, st.Seen = time.Time{}, time.Time{}
		s.K2BorrowState[k] = st
	}
	for k, m := range s.Forgotten {
		m.Seen = time.Time{}
		s.Forgotten[k] = m
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
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
	entries := learnedStateEntries(doc.State)
	dropped := 0
	for len(raw) > budget && dropped < len(entries) {
		n := max(len(entries)/10, 1)
		for _, e := range entries[dropped:min(dropped+n, len(entries))] {
			e.drop()
		}
		dropped = min(dropped+n, len(entries))
		if raw, err = json.Marshal(doc); err != nil {
			return "", 0, err
		}
	}
	if len(raw) > budget {
		return "", dropped, fmt.Errorf("the learned-state snapshot is %d bytes with every entry left out, over its %d-byte bound", len(raw), budget)
	}
	return string(raw), dropped, nil
}

// cloneLearnedState copies the state's maps, so leaving entries out of the
// copy cannot reach the original. The values are plain data, except the
// window values, which nothing here writes into.
func cloneLearnedState(s saturation.LearnedState) saturation.LearnedState {
	s.K2 = maps.Clone(s.K2)
	s.Throughput = maps.Clone(s.Throughput)
	s.ITLBaseline = maps.Clone(s.ITLBaseline)
	s.StartSeconds = maps.Clone(s.StartSeconds)
	s.VariantSeenAt = maps.Clone(s.VariantSeenAt)
	s.Accelerators = maps.Clone(s.Accelerators)
	s.ITLLines = maps.Clone(s.ITLLines)
	s.K2BorrowedSince = maps.Clone(s.K2BorrowedSince)
	s.K2BorrowState = maps.Clone(s.K2BorrowState)
	s.Forgotten = maps.Clone(s.Forgotten)
	s.Config = maps.Clone(s.Config)
	return s
}

// learnedStateEntry is one entry of the state, with the clock that says how
// recently it was used and how to leave it out.
type learnedStateEntry struct {
	at   time.Time
	drop func()
}

// learnedStateEntries lists the state's entries, least recently used first.
func learnedStateEntries(s saturation.LearnedState) []learnedStateEntry {
	out := make([]learnedStateEntry, 0, len(s.K2)+len(s.Throughput)+len(s.VariantSeenAt)+
		len(s.ITLLines)+len(s.Accelerators)+len(s.K2BorrowedSince)+len(s.Forgotten))
	for k, w := range s.K2 {
		out = append(out, learnedStateEntry{w.LastUsed, func() { delete(s.K2, k) }})
	}
	for k, w := range s.Throughput {
		out = append(out, learnedStateEntry{w.LastUsed, func() { delete(s.Throughput, k) }})
	}
	for k, seen := range s.VariantSeenAt {
		out = append(out, learnedStateEntry{seen, func() {
			delete(s.VariantSeenAt, k)
			delete(s.ITLBaseline, k)
			delete(s.StartSeconds, k)
		}})
	}
	for k, l := range s.ITLLines {
		out = append(out, learnedStateEntry{l.LearnedAt, func() { delete(s.ITLLines, k) }})
	}
	for k, m := range s.Accelerators {
		out = append(out, learnedStateEntry{m.LastUsed, func() { delete(s.Accelerators, k) }})
	}
	// A borrow clock is ordered by when its variant was last seen, not by its
	// start: a spent clock is kept for its variant's life, so its start is
	// the oldest time in the document and would go first. It goes with its
	// state; an orphaned state restores no clock, and the variant re-armed.
	for k, since := range s.K2BorrowedSince {
		at := since
		if st, ok := s.K2BorrowState[k]; ok && st.Seen.After(at) {
			at = st.Seen
		}
		out = append(out, learnedStateEntry{at, func() {
			delete(s.K2BorrowedSince, k)
			delete(s.K2BorrowState, k)
		}})
	}
	for k, m := range s.Forgotten {
		out = append(out, learnedStateEntry{m.Seen, func() { delete(s.Forgotten, k) }})
	}
	slices.SortStableFunc(out, func(a, b learnedStateEntry) int { return cmp.Compare(a.at.UnixNano(), b.at.UnixNano()) })
	return out
}
