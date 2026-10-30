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

// Read parses a recording into its events. A line that does not parse is an
// error: a recording with a hole in it does not replay.
func Read(data []byte) ([]RawEvent, error) {
	var out []RawEvent
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env struct {
			Seq  uint64
			Now  string
			Kind string
			Data json.RawMessage
		}
		if err := json.Unmarshal(line, &env); err != nil {
			return nil, fmt.Errorf("learnrec: line %d: %w", i+1, err)
		}
		now, err := time.Parse(time.RFC3339Nano, env.Now)
		if err != nil {
			return nil, fmt.Errorf("learnrec: line %d: %w", i+1, err)
		}
		out = append(out, RawEvent{Seq: env.Seq, Now: now.UTC(), Kind: env.Kind, Data: env.Data, Line: string(line)})
	}
	return out, nil
}

// DecodeData decodes the event's data into the value v points to.
func (e RawEvent) DecodeData(v any) error {
	return Unmarshal(e.Data, v)
}
