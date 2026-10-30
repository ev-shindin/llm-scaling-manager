package learnrec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingDir is a directory outside /tmp (t.TempDir is under it on Linux).
func recordingDir(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(base, 0o755))
	dir, err := os.MkdirTemp(base, "learnrec-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// writeRecording puts a closed recording of size bytes in dir, started at at.
func writeRecording(t *testing.T, dir string, at time.Time, size int64) string {
	t.Helper()
	name := fileName(at)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), int(size)), 0o600))
	return name
}

func listDir(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]int64{}
	for _, e := range entries {
		info, err := e.Info()
		require.NoError(t, err)
		out[e.Name()] = info.Size()
	}
	return out
}

func TestOptions_Check(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want string
	}{
		{"off", Options{}, ""},
		{"defaults", Options{Dir: "/var/lib/wva/recordings"}, ""},
		{"tmp", Options{Dir: "/tmp"}, "/tmp"},
		{"below tmp", Options{Dir: "/tmp/recordings"}, "/tmp"},
		{"tmp after cleaning", Options{Dir: "/tmp/../tmp/x"}, "/tmp"},
		{"a sibling of tmp", Options{Dir: "/tmpfoo"}, ""},
		{"cap below the minimum", Options{Dir: "/var/x", CapBytes: MinCapBytes - 1}, "minimum"},
		{"cap at the minimum", Options{Dir: "/var/x", CapBytes: MinCapBytes}, ""},
		{"ceiling below two caps", Options{Dir: "/var/x", CapBytes: 1 << 20, DirBytes: 2<<20 - 1}, "two recordings"},
		{"ceiling at two caps", Options{Dir: "/var/x", CapBytes: 1 << 20, DirBytes: 2 << 20}, ""},
	}
	for _, c := range cases {
		err := c.opts.Check()
		if c.want == "" {
			assert.NoError(t, err, c.name)
			continue
		}
		if assert.Error(t, err, c.name) {
			assert.Contains(t, err.Error(), c.want, c.name)
		}
	}
	assert.True(t, errors.Is(Options{Dir: "/tmp/x"}.Check(), errTmp))
	d := Options{Dir: "/x"}.withDefaults()
	assert.Equal(t, DefaultCapBytes, d.CapBytes)
	assert.Equal(t, 2*DefaultCapBytes, d.DirBytes)
}

// A directory that reaches /tmp through a symbolic link is /tmp.
func TestOptions_CheckFollowsSymlinks(t *testing.T) {
	if _, err := os.Stat("/tmp"); err != nil {
		t.Skip("no /tmp on this system")
	}
	dir := recordingDir(t)
	link := filepath.Join(dir, "rec")
	require.NoError(t, os.Symlink("/tmp", link))
	err := Options{Dir: filepath.Join(link, "not-yet", "made")}.Check()
	assert.True(t, errors.Is(err, errTmp), "a link to /tmp, then a path that does not exist yet: %v", err)
}

func TestOpen_WritesToAFileNamedByItsStartWithPrivateModes(t *testing.T) {
	dir := filepath.Join(recordingDir(t), "rec")
	at := time.Date(2026, 4, 5, 6, 7, 8, 9, time.UTC)
	r, err := Open(Options{Dir: dir}, at)
	require.NoError(t, err)
	r.Record(KindStart, at, Start{})
	require.NoError(t, r.Close())
	file := filepath.Join(dir, "learnrec-20260405T060708.000000009Z.jsonl")
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	events, err := readEvents(b)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, KindStart, events[0].Kind)
	di, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	fi, err := os.Stat(file)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestOpen_RefusesToOverwriteARecording(t *testing.T) {
	dir := recordingDir(t)
	writeRecording(t, dir, t0, 10)
	_, err := Open(Options{Dir: dir}, t0)
	require.Error(t, err, "two starts in the same nanosecond")
	assert.Equal(t, int64(10), listDir(t, dir)[fileName(t0)], "the one there is untouched")
}

func TestOpen_RefusesBadOptions(t *testing.T) {
	_, err := Open(Options{}, t0)
	assert.Error(t, err)
	_, err = Open(Options{Dir: recordingDir(t), CapBytes: 1 << 20, DirBytes: 1 << 20}, t0)
	assert.Error(t, err, "a ceiling of one cap")
}

// The newest recording with data survives any number of restarts that leave
// only tiny recordings behind: a crash loop.
func TestOpen_ACrashLoopKeepsTheRecordingFromBeforeIt(t *testing.T) {
	dir := recordingDir(t)
	const capBytes = 4 * MinCapBytes
	full := writeRecording(t, dir, t0, capBytes)
	for i := 1; i <= 10; i++ {
		r, err := Open(Options{Dir: dir, CapBytes: capBytes}, t0.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err, "restart %d", i)
		// The crash: a start line and little else.
		r.Record(KindStart, t0, Start{})
		require.NoError(t, r.Close())
		files := listDir(t, dir)
		require.Contains(t, files, full, "restart %d deleted the recording from before the crash loop", i)
		var total int64
		for _, s := range files {
			total += s
		}
		assert.LessOrEqual(t, total, 2*capBytes, "restart %d: over the ceiling", i)
	}
}

// Room is made in order: tiny recordings first, then older ones with data,
// and the newest with data only when what is left would be under the floor;
// short of a full cap, the new recording is capped at what is left.
func TestOpen_MakesRoomInOrder(t *testing.T) {
	const capBytes = 4 * MinCapBytes
	t.Run("tiny first, then older, never the newest", func(t *testing.T) {
		dir := recordingDir(t)
		older := writeRecording(t, dir, t0, capBytes)
		tiny := writeRecording(t, dir, t0.Add(time.Minute), 100)
		newest := writeRecording(t, dir, t0.Add(2*time.Minute), MinCapBytes)
		require.NoError(t, os.Mkdir(filepath.Join(dir, fileName(t0.Add(3*time.Minute))), 0o700),
			"a directory named like a recording is never touched")
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep"), 0o600))
		r, err := Open(Options{Dir: dir, CapBytes: capBytes}, t0.Add(time.Hour))
		require.NoError(t, err)
		require.NoError(t, r.Close())
		files := listDir(t, dir)
		assert.NotContains(t, files, tiny)
		assert.NotContains(t, files, older)
		assert.Contains(t, files, newest)
		assert.Contains(t, files, "notes.txt")
		assert.Contains(t, files, fileName(t0.Add(3*time.Minute)))
		assert.Equal(t, capBytes, r.limit, "after the older one goes, a full cap fits")
	})
	t.Run("short of a cap, the new one gets what is left", func(t *testing.T) {
		dir := recordingDir(t)
		// A recording from a run with a larger cap: keeping it leaves less
		// than a cap but more than the floor.
		newest := writeRecording(t, dir, t0, 2*capBytes-2*MinCapBytes)
		r, err := Open(Options{Dir: dir, CapBytes: capBytes}, t0.Add(time.Hour))
		require.NoError(t, err)
		require.NoError(t, r.Close())
		assert.Contains(t, listDir(t, dir), newest)
		assert.Equal(t, 2*MinCapBytes, r.limit)
	})
	t.Run("under the floor, the newest goes too", func(t *testing.T) {
		dir := recordingDir(t)
		newest := writeRecording(t, dir, t0, 2*capBytes-MinCapBytes/2)
		r, err := Open(Options{Dir: dir, CapBytes: capBytes}, t0.Add(time.Hour))
		require.NoError(t, err)
		require.NoError(t, r.Close())
		assert.NotContains(t, listDir(t, dir), newest)
		assert.Equal(t, capBytes, r.limit)
	})
}

// A capped recording is closed for writing, and Active says so: nothing
// is encoded after it.
func TestRecorder_StopsWorkOnceInactive(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 1)
	assert.True(t, r.Active())
	r.Record("k", t0, 1)
	assert.False(t, r.Active(), "the cap stopped it")
	assert.Nil(t, r.Fragment(1), "nothing is encoded once it is stopped")
	assert.Zero(t, buf.Len())
}

func TestRecorder_CloseStopsEverything(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 0)
	r.Record("k", t0, 1)
	require.NoError(t, r.Close())
	require.NoError(t, r.Close(), "closing twice is safe")
	assert.False(t, r.Active())
	assert.Zero(t, r.Record("k", t0, 2))
	assert.Nil(t, r.Fragment(2))
	events, err := readEvents(buf.Bytes())
	require.NoError(t, err)
	assert.Len(t, events, 1)
}

// A cap below the start line keeps nothing at all.
func TestRecorder_ACapBelowTheStartLineKeepsNothing(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 10)
	assert.Zero(t, r.Record(KindStart, t0, Start{}))
	assert.Zero(t, buf.Len())
	assert.Equal(t, uint64(1), r.droppedCount())
}

// failingFile fails every write after the first n bytes, writing what fits.
type failingFile struct {
	buf       bytes.Buffer
	room      int
	truncated int64
}

func (f *failingFile) Write(p []byte) (int, error) {
	if len(p) <= f.room {
		f.room -= len(p)
		return f.buf.Write(p)
	}
	n := f.room
	f.buf.Write(p[:n])
	f.room = 0
	return n, io.ErrShortWrite
}

func (f *failingFile) Truncate(size int64) error {
	f.truncated = size
	f.buf.Truncate(int(size))
	return nil
}

// A write that fails part way is cut back to the last whole line, and the
// recording stops.
func TestRecorder_AFailedWriteLeavesWholeLines(t *testing.T) {
	line, err := Marshal(Event{Seq: 1, Now: t0, Kind: "k", Data: 1})
	require.NoError(t, err)
	f := &failingFile{room: len(line) + 1 + 5}
	r := New(f, 0)
	require.NotZero(t, r.Record("k", t0, 1))
	assert.Zero(t, r.Record("k", t0, 1))
	require.Error(t, r.Err())
	assert.Equal(t, int64(len(line)+1), f.truncated)
	assert.Equal(t, string(line)+"\n", f.buf.String())
	assert.False(t, r.Active())
}

// Concurrent records number events without a gap or a repeat (run with
// -race).
func TestRecorder_ConcurrentRecordsNumberEveryEventOnce(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 0)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				r.Record(fmt.Sprintf("g%d", g), t0, i)
				_ = r.Fragment(i)
				_ = r.Active()
			}
		}()
	}
	wg.Wait()
	events, err := readEvents(buf.Bytes())
	require.NoError(t, err, "Read holds the numbering contiguous")
	assert.Len(t, events, 400)
}

// A last line cut off mid-write is dropped and reported; a bad line before
// the end, or a gap in the numbering, is an error.
func TestRead_TornTailAndGaps(t *testing.T) {
	var buf bytes.Buffer
	r := New(&buf, 0)
	for i := range 3 {
		r.Record("k", t0, i)
	}
	whole := buf.String()

	rec, err := Read([]byte(whole + `{"Seq":4,"Now":"2026-01`))
	require.NoError(t, err)
	assert.Len(t, rec.Events, 3)
	assert.Equal(t, `{"Seq":4,"Now":"2026-01`, rec.TornTail)

	rec, err = Read([]byte(whole))
	require.NoError(t, err)
	assert.Empty(t, rec.TornTail)

	lines := strings.SplitAfter(whole, "\n")
	_, err = Read([]byte(lines[0] + lines[2]))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sequence number 3, want 2")

	_, err = Read([]byte(lines[0] + "{broken\n" + lines[1]))
	require.Error(t, err, "a bad line that is not the last")
}
