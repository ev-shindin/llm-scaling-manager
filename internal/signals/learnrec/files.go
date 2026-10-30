package learnrec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// Where recordings go (docs/proposals/analyzer-learner.md, 2.4).

const (
	// DefaultCapBytes is one recording's cap: 256Mi. The acceptance run
	// (internal/engines/steadystate, learner_recording_test.go) measures
	// about 1.7 KB per replica per cycle for each analyzer a model lists, each
	// Analyze carrying its input. At a 15 s cycle, with saturation alone, 8
	// replicas reach the cap in about 3.4 days and 100 in about 6.5 hours;
	// with saturation, slo and throughput, in about 27 and 2.1 hours.
	DefaultCapBytes int64 = 256 << 20

	// MinCapBytes is the smallest cap a recording may have: below it a
	// recording holds a start line and a few cycles at most. A new recording
	// is never squeezed below it to keep an older one.
	MinCapBytes int64 = 64 << 10

	// filePrefix and fileSuffix name a recording; between them is its start
	// time, so the names sort in the order the recordings started.
	filePrefix = "learnrec-"
	fileSuffix = ".jsonl"
	// fileTimeLayout is the start time in a file name: UTC, fixed width, no
	// colons (kubectl cp and some volumes refuse them).
	fileTimeLayout = "20060102T150405.000000000Z"
)

// errTmp is returned for a recording directory under /tmp: the controller's
// /tmp is a 64Mi emptyDir, and the kubelet evicts the pod when it fills.
var errTmp = errors.New("learnrec: refusing a recording directory under /tmp; mount a volume of its own (config/components/learner-recorder)")

// Options says where a controller start records and how much it may write.
type Options struct {
	// Dir is the directory recordings are written to; empty means off.
	Dir string
	// CapBytes is one recording's cap (DefaultCapBytes when not positive).
	CapBytes int64
	// DirBytes is what all recordings in Dir may hold together, the one
	// being started included (twice the cap when not positive). It must be
	// at least twice the cap, so one recording can be kept while the next
	// is written, and no larger than the volume under Dir.
	DirBytes int64
}

// withDefaults is opts with the defaults filled in.
func (o Options) withDefaults() Options {
	if o.CapBytes <= 0 {
		o.CapBytes = DefaultCapBytes
	}
	if o.DirBytes <= 0 {
		o.DirBytes = 2 * o.CapBytes
	}
	return o
}

// Check refuses options a recording cannot start with: a directory under
// /tmp, a cap below MinCapBytes, or a directory ceiling below two caps. The
// controller calls it at startup and, on an error, runs without recording:
// a test aid never stops scaling.
func (o Options) Check() error {
	if o.Dir == "" {
		return nil
	}
	o = o.withDefaults()
	if o.CapBytes < MinCapBytes {
		return fmt.Errorf("learnrec: a recording cap of %d bytes is below the %d-byte minimum", o.CapBytes, MinCapBytes)
	}
	if o.DirBytes < 2*o.CapBytes {
		return fmt.Errorf("learnrec: the directory ceiling (%d bytes) holds fewer than two recordings at the %d-byte cap", o.DirBytes, o.CapBytes)
	}
	return checkDir(o.Dir)
}

// Open starts a new recording in opts.Dir, named by start. To make room
// under the directory ceiling it deletes the recordings already there --
// every one is closed: a recording starts only at a controller start, and
// the one before it ended with its process -- in this order: the tiny ones
// (under MinCapBytes, such as a crash loop leaves), oldest first; then the
// older ones with data, oldest first, never the newest of them. If what is
// left is less than a full cap, the new recording is capped at what is left;
// only when that would be under MinCapBytes does the newest go too. So a
// controller that restarts again and again keeps the recording from before
// the first restart.
func Open(opts Options, start time.Time) (*Recorder, error) {
	if opts.Dir == "" {
		return nil, errors.New("learnrec: no recording directory")
	}
	if err := opts.Check(); err != nil {
		return nil, err
	}
	opts = opts.withDefaults()
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("learnrec: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("learnrec: %w", err)
	}
	capBytes, err := makeRoom(dir, opts.DirBytes, opts.CapBytes)
	if err != nil {
		return nil, err
	}
	name := filepath.Join(dir, fileName(start))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("learnrec: %w", err)
	}
	r := New(f, capBytes)
	r.closer = f
	r.name = name
	ctrl.Log.WithName("learnrec").Info("Recording the analyzers' inputs", "file", name, "capBytes", capBytes,
		"dirCeilingBytes", opts.DirBytes)
	return r, nil
}

// fileName is the name of a recording started at start.
func fileName(start time.Time) string {
	return filePrefix + start.UTC().Format(fileTimeLayout) + fileSuffix
}

// checkDir refuses /tmp and anything below it, symbolic links followed as
// far as the path exists.
func checkDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("learnrec: %w", err)
	}
	if underTmp(resolveExisting(abs)) || underTmp(abs) {
		return errTmp
	}
	return nil
}

// resolveExisting is path with its deepest existing ancestor's symbolic
// links resolved, and the rest, which does not exist yet, as written.
func resolveExisting(path string) string {
	rest := ""
	p := path
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// underTmp reports whether dir is /tmp or below it.
func underTmp(dir string) bool {
	d := filepath.ToSlash(filepath.Clean(dir))
	return d == "/tmp" || strings.HasPrefix(d, "/tmp/")
}

// makeRoom deletes recordings in dir, in Open's order, until those left
// leave room under ceiling for a recording capped at capBytes, and returns
// the cap the new recording gets.
func makeRoom(dir string, ceiling, capBytes int64) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("learnrec: %w", err)
	}
	type rec struct {
		name string
		size int64
	}
	var recs []rec
	var total int64
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), filePrefix) || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, fmt.Errorf("learnrec: %w", err)
		}
		recs = append(recs, rec{e.Name(), info.Size()})
		total += info.Size()
	}
	// The start time is in the name, fixed width, so the names sort oldest
	// first.
	sort.Slice(recs, func(i, j int) bool { return recs[i].name < recs[j].name })
	remove := func(r rec, why string) error {
		if err := os.Remove(filepath.Join(dir, r.name)); err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		total -= r.size
		ctrl.Log.WithName("learnrec").Info("Deleted a recording to make room", "file", r.name, "bytes", r.size, "why", why)
		return nil
	}
	newest := -1
	for i, r := range recs {
		if r.size >= MinCapBytes {
			newest = i
		}
	}
	for _, r := range recs {
		if total+capBytes <= ceiling {
			break
		}
		if r.size < MinCapBytes {
			if err := remove(r, "tiny"); err != nil {
				return 0, err
			}
		}
	}
	for i, r := range recs {
		if total+capBytes <= ceiling {
			break
		}
		if r.size >= MinCapBytes && i != newest {
			if err := remove(r, "older"); err != nil {
				return 0, err
			}
		}
	}
	if left := ceiling - total; left < capBytes {
		if left >= MinCapBytes {
			return left, nil
		}
		if newest >= 0 {
			if err := remove(recs[newest], "no room for a new recording"); err != nil {
				return 0, err
			}
		}
		return min(capBytes, ceiling-total), nil
	}
	return capBytes, nil
}
