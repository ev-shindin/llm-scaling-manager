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
	// DefaultCapBytes is one recording's cap: 256Mi. At about 1-1.5 KB per
	// replica per cycle and a 15 s cycle, eight replicas take about five
	// days to reach it and a hundred about eight hours.
	DefaultCapBytes int64 = 256 << 20

	// filePrefix and fileSuffix name a recording; between them is its start
	// time, so the names sort in the order the recordings started.
	filePrefix = "learnrec-"
	fileSuffix = ".jsonl"
	// fileTimeLayout is the start time in a file name: UTC, fixed width, no
	// colons (kubectl cp and some volumes refuse them).
	fileTimeLayout = "20060102T150405.000000000Z"
)

// ErrTmp is returned for a recording directory under /tmp: the controller's
// /tmp is a 64Mi emptyDir, and the kubelet evicts the pod when it fills.
var ErrTmp = errors.New("learnrec: refusing a recording directory under /tmp; mount a separate volume (config/recorder)")

// Options says where a controller start records and how much it may write.
type Options struct {
	// Dir is the directory recordings are written to; empty means off.
	Dir string
	// CapBytes is one recording's cap (DefaultCapBytes when not positive).
	CapBytes int64
	// VolumeBytes is what all recordings in Dir may hold together, the one
	// being started included (twice the cap when not positive). Older
	// recordings are deleted, oldest first, to make room for the new one.
	VolumeBytes int64
}

// Open starts a new recording in opts.Dir, named by start, after deleting
// the oldest recordings that would not leave it room under the volume
// ceiling. Every recording already there is closed: a recording starts only
// at a controller start, and the one before it ended with its process.
func Open(opts Options, start time.Time) (*Recorder, error) {
	if opts.Dir == "" {
		return nil, errors.New("learnrec: no recording directory")
	}
	if err := CheckDir(opts.Dir); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("learnrec: %w", err)
	}
	capBytes := opts.CapBytes
	if capBytes <= 0 {
		capBytes = DefaultCapBytes
	}
	volume := opts.VolumeBytes
	if volume <= 0 {
		volume = 2 * capBytes
	}
	if volume < capBytes {
		return nil, fmt.Errorf("learnrec: the volume ceiling (%d bytes) is below one recording's cap (%d bytes)", volume, capBytes)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("learnrec: %w", err)
	}
	if err := makeRoom(dir, volume-capBytes); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, FileName(start))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("learnrec: %w", err)
	}
	r := New(f, capBytes)
	r.closer = f
	r.name = name
	ctrl.Log.WithName("learnrec").Info("Recording the analyzers' inputs", "file", name, "capBytes", capBytes, "volumeBytes", volume)
	return r, nil
}

// FileName is the name of a recording started at start.
func FileName(start time.Time) string {
	return filePrefix + start.UTC().Format(fileTimeLayout) + fileSuffix
}

// CheckDir refuses a recording directory the controller must not write to:
// /tmp and anything below it (ErrTmp). The controller calls it at startup,
// so a wrong flag stops it there rather than at its first cycle.
func CheckDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("learnrec: %w", err)
	}
	if underTmp(abs) {
		return ErrTmp
	}
	return nil
}

// underTmp reports whether dir is /tmp or below it.
func underTmp(dir string) bool {
	d := filepath.ToSlash(filepath.Clean(dir))
	return d == "/tmp" || strings.HasPrefix(d, "/tmp/")
}

// makeRoom deletes recordings in dir, oldest first, until those left hold at
// most keep bytes.
func makeRoom(dir string, keep int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("learnrec: %w", err)
	}
	type rec struct {
		name string
		size int64
	}
	var recs []rec
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), filePrefix) || !strings.HasSuffix(e.Name(), fileSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		recs = append(recs, rec{e.Name(), info.Size()})
		total += info.Size()
	}
	// The start time is in the name, fixed width, so the names sort oldest
	// first.
	sort.Slice(recs, func(i, j int) bool { return recs[i].name < recs[j].name })
	for _, r := range recs {
		if total <= keep {
			break
		}
		if err := os.Remove(filepath.Join(dir, r.name)); err != nil {
			return fmt.Errorf("learnrec: %w", err)
		}
		total -= r.size
		ctrl.Log.WithName("learnrec").Info("Deleted the oldest recording to make room", "file", r.name, "bytes", r.size)
	}
	return nil
}
