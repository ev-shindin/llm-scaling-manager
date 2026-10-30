package learnrec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// RawEvent is an Event as read back, its Data still undecoded.
type RawEvent struct {
	Seq  uint64
	Now  time.Time
	Kind string
	// Data is the event's data as written; DecodeData turns it into the
	// kind's own type.
	Data json.RawMessage
	// Line is the whole line as written, for a byte-for-byte comparison.
	Line string
}

// Recording is a recording as read back.
type Recording struct {
	Events []RawEvent
	// TornTail is a last line that was cut off mid-write (no newline, and it
	// does not parse): the process died, or the volume filled, while writing
	// it. It is dropped; empty when the recording ends cleanly.
	TornTail string
}

// Read parses a recording into its events. Every line must parse and the
// sequence numbers must run from 1 with no gap, except a last line cut off
// mid-write, which is reported in TornTail and dropped. Anything else is an
// error: a recording with a hole in it does not replay.
func Read(data []byte) (Recording, error) {
	var out Recording
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev, err := readLine(line)
		if err != nil {
			if i == len(lines)-1 {
				// No newline after it: the write that would have ended it
				// never happened.
				out.TornTail = string(line)
				break
			}
			return Recording{}, fmt.Errorf("learnrec: line %d: %w", i+1, err)
		}
		if want := uint64(len(out.Events)) + 1; ev.Seq != want {
			return Recording{}, fmt.Errorf("learnrec: line %d: sequence number %d, want %d", i+1, ev.Seq, want)
		}
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

func readLine(line []byte) (RawEvent, error) {
	var env struct {
		Seq  uint64
		Now  string
		Kind string
		Data json.RawMessage
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return RawEvent{}, err
	}
	now, err := time.Parse(time.RFC3339Nano, env.Now)
	if err != nil {
		return RawEvent{}, err
	}
	return RawEvent{Seq: env.Seq, Now: now.UTC(), Kind: env.Kind, Data: env.Data, Line: string(line)}, nil
}

// DecodeData decodes the event's data into the value v points to.
func (e RawEvent) DecodeData(v any) error {
	return Unmarshal(e.Data, v)
}
