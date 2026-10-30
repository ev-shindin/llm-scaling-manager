// Package learnrec records what the analyzers learn from, so a run can be
// replayed: every input an analyzer reads that it did not compute, and every
// result it returned, as one stream of JSON lines with one sequence number.
//
// The recorder is a field inside each analyzer and inside the capacity store
// (docs/proposals/analyzer-learner.md, section 2.4), nil when recording is
// off. A wrapper around domain.Analyzer would hide the capabilities the
// engine reaches by type assertion. Each recorded method records and then
// calls an unexported implementation; the analyzer's own calls go to the
// implementation, so a nested call is recorded once.
//
// This package knows nothing of the analyzers: an event's data is whatever
// the recording package hands it, encoded by Marshal. Each package exports
// its event types for one consumer, the replay that reads a recording back
// (internal/engines/analyzers/learnreplay).
package learnrec

import (
	"fmt"
	"io"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Event is one line of a recording.
type Event struct {
	// Seq is the event's place in the recording, from 1, with no gaps: one
	// counter for every analyzer and the store.
	Seq uint64
	// Now is the clock the call read: the analyzer's cycle clock, or the
	// instant the engine passed the store.
	Now time.Time
	// Kind names the call, "<owner>.<Method>"; the owning package defines its
	// kinds and the type of their data.
	Kind string
	// Data is the call's arguments and what it returned, in the form the
	// owning package defines for its kind.
	Data any
}

// Recorder writes events to one sink. Safe for concurrent use; a nil
// Recorder records nothing, and every method is safe to call on one.
type Recorder struct {
	mu sync.Mutex
	w  io.Writer
	// closer is the file under w, when the recorder opened one.
	closer io.Closer
	// seq is the last sequence number handed out.
	seq uint64
	// written is the bytes written so far, and limit the recording's cap:
	// the first line that would pass it stops the recording, which keeps its
	// head (a replay needs a start and every event after it).
	written int64
	limit   int64
	// full is set once the cap was reached; dropped counts the events not
	// written since.
	full    bool
	dropped uint64
	// err is the first write or encode error; the recording stops on it.
	err error
	// closed is set by Close: nothing is written after it.
	closed bool
	name   string
}

// New returns a recorder that writes to w, stopping once limit bytes are
// written. A non-positive limit means no cap.
func New(w io.Writer, limit int64) *Recorder {
	return &Recorder{w: w, limit: limit, name: "stream"}
}

// Active reports whether the recorder still writes: false for a nil
// recorder, and once the cap, an error or Close stopped it. A caller skips
// the work of building an event when it is not.
func (r *Recorder) Active() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeLocked()
}

func (r *Recorder) activeLocked() bool {
	return !r.full && r.err == nil && !r.closed
}

// Record writes one event of kind, read at now, with data, and returns its
// sequence number (0 when nothing was written). A closed recorder writes
// nothing and numbers nothing; until then a sequence number is handed out
// whether or not the line fits, and what does not is counted as dropped.
func (r *Recorder) Record(kind string, now time.Time, data any) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0
	}
	r.seq++
	if !r.activeLocked() {
		r.dropped++
		return 0
	}
	line, err := Marshal(Event{Seq: r.seq, Now: now, Kind: kind, Data: data})
	if err != nil {
		r.fail(fmt.Errorf("encoding %s: %w", kind, err))
		r.dropped++
		return 0
	}
	line = append(line, '\n')
	if r.limit > 0 && r.written+int64(len(line)) > r.limit {
		r.full = true
		r.dropped++
		ctrl.Log.WithName("learnrec").Info("Recording reached its cap; keeping its head and dropping every later event",
			"recording", r.name, "capBytes", r.limit, "writtenBytes", r.written, "firstDroppedSeq", r.seq,
			"dropped", r.dropped)
		return 0
	}
	n, err := r.w.Write(line)
	if err != nil {
		// A torn line would make the recording's tail unreadable: cut it
		// back to the last whole line when the sink can.
		if t, ok := r.w.(interface{ Truncate(int64) error }); ok && n > 0 {
			_ = t.Truncate(r.written)
		} else {
			r.written += int64(n)
		}
		r.fail(fmt.Errorf("writing %s: %w", kind, err))
		r.dropped++
		return 0
	}
	r.written += int64(n)
	return r.seq
}

// Fragment encodes v now, for an event written later: what a call read,
// encoded before the call can change it. It does nothing once the recorder
// is not Active. An encoding error stops the recording, as it would have at
// Record.
func (r *Recorder) Fragment(v any) Raw {
	if !r.Active() {
		return nil
	}
	b, err := Marshal(v)
	if err != nil {
		r.mu.Lock()
		if r.err == nil {
			r.fail(fmt.Errorf("encoding a fragment of %T: %w", v, err))
		}
		r.mu.Unlock()
		return nil
	}
	return b
}

func (r *Recorder) fail(err error) {
	r.err = err
	ctrl.Log.WithName("learnrec").Error(err, "Recording stopped", "recording", r.name, "seq", r.seq)
}

// droppedCount is how many events were not written: past the cap, or after
// an error stopped the recording.
func (r *Recorder) droppedCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// writtenBytes is the bytes written so far.
func (r *Recorder) writtenBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written
}

// Err is the error that stopped the recording, nil while it runs.
func (r *Recorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close ends the recording: nothing is recorded or encoded after it. It logs
// what was dropped and closes the file the recorder opened, if any. Closing
// twice is safe.
func (r *Recorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.dropped > 0 {
		ctrl.Log.WithName("learnrec").Info("Recording closed with events dropped",
			"recording", r.name, "dropped", r.dropped, "writtenBytes", r.written, "capBytes", r.limit)
	}
	if r.closer == nil {
		return nil
	}
	err := r.closer.Close()
	r.closer = nil
	return err
}
