package learnreplay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
)

// Goldens (docs/proposals/analyzer-learner.md, 2.5): each committed fixture
// is an exported recording beside one CSV file per output its replay
// produces. A golden check replays every fixture and compares what comes out
// with the files; the manifest, goldens.yaml, lists every fixture and every
// file, and the check fails when the directory and the manifest disagree.
//
// What is compared is outputs, keyed on what they are, never on sequence
// numbers: the kind of event, its clock, a group (namespace/model, or the
// line's key, or the learned-state section) and an item (the variant, or the
// key). Two things vary between replays of one recording that are not a
// change of output, and the comparison is built for both:
//
//   - Order. Store filings within a cycle come in map order, so the events
//     around them can be numbered differently from one recording to the
//     next. No row carries a sequence number, and each file is sorted by its
//     key.
//   - The last bits of a float. A sum over three or more variants taken in
//     map order can differ in its last bits. Each row's data is compared as
//     JSON, and two numbers are equal within a relative 1e-9 (FloatTolerance);
//     everything else is compared exactly.

// GoldenDir is where the committed goldens live, from the module root.
const GoldenDir = "internal/engines/analyzers/learnreplay/testdata/goldens"

// ManifestName is the manifest's file name inside the golden directory.
const ManifestName = "goldens.yaml"

// RecordingName is a fixture's exported recording, gzipped, inside its
// directory.
const RecordingName = "recording.jsonl.gz"

// MaxFixtureBytes is the largest compressed recording a fixture may commit.
// A longer run is cut to its head first (AddOptions.HeadLines): a cut
// recording replays.
const MaxFixtureBytes = 4 << 20

// FloatTolerance is the relative difference under which two numbers in a
// golden row are equal.
const FloatTolerance = 1e-9

// The outputs, one file each, so a diff names the output.
const (
	// OutputResults is each Analyze's result without its variant
	// capacities -- role demand, held role demand, start seconds -- and its
	// error.
	OutputResults = "results.csv"
	// OutputVariantCapacities is each Analyze's variant capacities, one row
	// per variant: the priced figures, their sources and keys.
	OutputVariantCapacities = "variant-capacities.csv"
	// OutputStoreRecords is the analyzer-written fields of each capacity
	// record, as an Analyze read them (2.4).
	OutputStoreRecords = "store-records.csv"
	// OutputLines is the ITL lines published and borrowed.
	OutputLines = "lines.csv"
	// OutputSLOPublished is what each slo Analyze published.
	OutputSLOPublished = "slo-published.csv"
	// OutputLearnedState is every learned-state document written, one row
	// per key of each section: the learned figures per key.
	OutputLearnedState = "learned-state.csv"
)

// OutputNames is every output, in the order the manifest lists them.
var OutputNames = []string{OutputLearnedState, OutputLines, OutputResults, OutputSLOPublished,
	OutputStoreRecords, OutputVariantCapacities}

// storeOutputFields are the capacity record's fields an analyzer writes: the
// store's golden output. The rest the engine supplied, and the replay checks
// them itself.
var storeOutputFields = []string{"TotalKvCapacityTokens", "EffectiveCapacity", "PriorLiveCapacity", "PriorLiveAt",
	"LearnedFrom", "LearnedAt"}

var csvHeader = []string{"kind", "now", "group", "item", "data"}

// row is one golden row.
type row struct {
	kind, now, group, item, data string
}

func (r row) key() string { return r.kind + "\x00" + r.now + "\x00" + r.group + "\x00" + r.item }

// Outputs extracts the golden outputs from a replay's own recording
// (Report.Recording), one CSV file per output, each sorted by its key.
func Outputs(recording []byte) (map[string][]byte, error) {
	rows, err := outputRows(recording)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(OutputNames))
	for _, name := range OutputNames {
		b, err := writeRows(rows[name])
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}

func outputRows(recording []byte) (map[string][]row, error) {
	read, err := readRecording(recording)
	if err != nil {
		return nil, err
	}
	rows := map[string][]row{}
	for _, ev := range read {
		now := ev.now
		switch ev.kind {
		case saturation.KindAnalyze, slo.KindAnalyze, throughput.KindAnalyze:
			in := ev.data.get("Input")
			group := in.get("Namespace").str() + "/" + in.get("ModelID").str()
			res := ev.data.get("Result")
			summary := &node{kind: nodeObject, keys: []string{"Error", "Result"}, vals: []*node{ev.data.get("Error"), res}}
			if res != nil && res.kind == nodeObject {
				summary.vals[1] = res.without("VariantCapacities")
				for _, vc := range res.get("VariantCapacities").items() {
					rows[OutputVariantCapacities] = append(rows[OutputVariantCapacities],
						row{ev.kind, now, group, vc.get("VariantName").str(), string(vc.bytes())})
				}
			}
			rows[OutputResults] = append(rows[OutputResults], row{ev.kind, now, group, "", string(summary.bytes())})
			if recs := ev.data.get("Records"); recs != nil && recs.kind == nodeObject {
				for i, variant := range recs.keys {
					rec := recs.vals[i]
					picked := &node{kind: nodeNull}
					if rec.kind == nodeObject {
						picked = &node{kind: nodeObject}
						for _, f := range storeOutputFields {
							if v := rec.get(f); v != nil {
								picked.keys = append(picked.keys, f)
								picked.vals = append(picked.vals, v)
							}
						}
					}
					rows[OutputStoreRecords] = append(rows[OutputStoreRecords], row{ev.kind, now, group, variant, string(picked.bytes())})
				}
			}
			if pub := ev.data.get("Published"); pub != nil && pub.kind != nodeNull {
				rows[OutputSLOPublished] = append(rows[OutputSLOPublished], row{ev.kind, now, group, "", string(pub.bytes())})
			}
		case saturation.KindPublishLine, saturation.KindBorrowLine:
			rows[OutputLines] = append(rows[OutputLines],
				row{ev.kind, now, ev.data.get("PhysicsKey").str(), ev.data.get("Variant").str(), string(ev.data.bytes())})
		case saturation.KindExportLearnedState:
			if ev.data.kind != nodeObject {
				continue
			}
			for i, section := range ev.data.keys {
				v := ev.data.vals[i]
				switch v.kind {
				case nodeNull:
				case nodeObject:
					for j, k := range v.keys {
						rows[OutputLearnedState] = append(rows[OutputLearnedState], row{ev.kind, now, section, k, string(v.vals[j].bytes())})
					}
				default:
					rows[OutputLearnedState] = append(rows[OutputLearnedState], row{ev.kind, now, section, "", string(v.bytes())})
				}
			}
		}
	}
	return rows, nil
}

// str is a string node's text, "" for anything else.
func (n *node) str() string {
	if n == nil || n.kind != nodeString {
		return ""
	}
	return n.text
}

// items is a list node's items, nil for anything else.
func (n *node) items() []*node {
	if n == nil || n.kind != nodeList {
		return nil
	}
	return n.vals
}

// event is a recorded event with its data parsed.
type event struct {
	kind, now string
	data      *node
}

func readRecording(recording []byte) ([]event, error) {
	var out []event
	for i, line := range bytes.Split(recording, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		n, err := parseNode(line)
		if err != nil {
			return nil, fmt.Errorf("learnreplay: line %d: %w", i+1, err)
		}
		data := n.get("Data")
		if data == nil {
			data = &node{kind: nodeNull}
		}
		out = append(out, event{kind: n.get("Kind").str(), now: n.get("Now").str(), data: data})
	}
	return out, nil
}

func sortRows(rows []row) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.key() != b.key() {
			return a.key() < b.key()
		}
		return a.data < b.data
	})
}

func writeRows(rows []row) ([]byte, error) {
	sortRows(rows)
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(csvHeader); err != nil {
		return nil, err
	}
	for _, r := range rows {
		if err := w.Write([]string{r.kind, r.now, r.group, r.item, r.data}); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return buf.Bytes(), w.Error()
}

func readRows(b []byte) ([]row, error) {
	r := csv.NewReader(bytes.NewReader(b))
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 || strings.Join(recs[0], ",") != strings.Join(csvHeader, ",") {
		return nil, fmt.Errorf("the header is not %s", strings.Join(csvHeader, ","))
	}
	out := make([]row, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		out = append(out, row{rec[0], rec[1], rec[2], rec[3], rec[4]})
	}
	return out, nil
}

// OutputDiff is how one output differs from its golden file.
type OutputDiff struct {
	// Output is the file.
	Output string
	// Rows is the golden file's rows; Changed, Added and Removed how many of
	// them moved, are new, or are gone.
	Rows, Changed, Added, Removed int
	// First describes the first row that differs, in key order: its key and
	// the first field that moved, with the sign and size of the change.
	First string
}

// String is the diff on one line, as the replay's per-output summary prints
// it.
func (d OutputDiff) String() string {
	return fmt.Sprintf("%s: %d of %d rows changed, %d added, %d removed; first: %s",
		d.Output, d.Changed, d.Rows, d.Added, d.Removed, d.First)
}

// compareOutput compares a golden file with the replay's output. A nil
// result is equal.
func compareOutput(name string, want, got []byte) (*OutputDiff, error) {
	w, err := readRows(want)
	if err != nil {
		return nil, fmt.Errorf("%s: golden: %w", name, err)
	}
	g, err := readRows(got)
	if err != nil {
		return nil, fmt.Errorf("%s: replay: %w", name, err)
	}
	wk, gk := numbered(w), numbered(g)
	keys := make([]string, 0, len(wk)+len(gk))
	for k := range wk {
		keys = append(keys, k)
	}
	for k := range gk {
		if _, ok := wk[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	d := &OutputDiff{Output: name, Rows: len(w)}
	note := func(s string) {
		if d.First == "" {
			d.First = s
		}
	}
	for _, k := range keys {
		wr, inW := wk[k]
		gr, inG := gk[k]
		switch {
		case !inG:
			d.Removed++
			note("removed " + describeKey(wr))
		case !inW:
			d.Added++
			note("added " + describeKey(gr))
		default:
			if detail := sameData(wr.data, gr.data); detail != "" {
				d.Changed++
				note(describeKey(wr) + ": " + detail)
			}
		}
	}
	if d.Changed+d.Added+d.Removed == 0 {
		return nil, nil
	}
	return d, nil
}

// numbered keys each row by its key and its place among rows with the same
// key, in file order (the file is sorted by key, then data).
func numbered(rows []row) map[string]row {
	out := make(map[string]row, len(rows))
	seen := map[string]int{}
	for _, r := range rows {
		k := r.key()
		out[k+"\x00"+strconv.Itoa(seen[k])] = r
		seen[k]++
	}
	return out
}

func describeKey(r row) string {
	parts := []string{r.kind, r.now}
	if r.group != "" {
		parts = append(parts, r.group)
	}
	if r.item != "" {
		parts = append(parts, r.item)
	}
	return strings.Join(parts, " ")
}

// sameData compares two rows' data as JSON, numbers within FloatTolerance,
// and describes the first difference ("" when equal).
func sameData(want, got string) string {
	if want == got {
		return ""
	}
	w, err := parseNode([]byte(want))
	if err != nil {
		return "the golden row is not JSON"
	}
	g, err := parseNode([]byte(got))
	if err != nil {
		return "the replayed row is not JSON"
	}
	return diffNode("", w, g)
}

func diffNode(at string, w, g *node) string {
	if at == "" {
		at = "."
	}
	if w.kind != g.kind {
		return fmt.Sprintf("%s: %s, was %s", at, clip(g.bytes()), clip(w.bytes()))
	}
	switch w.kind {
	case nodeNumber:
		a, errA := strconv.ParseFloat(w.text, 64)
		b, errB := strconv.ParseFloat(g.text, 64)
		if errA != nil || errB != nil {
			if w.text != g.text {
				return fmt.Sprintf("%s: %s, was %s", at, g.text, w.text)
			}
			return ""
		}
		if a == b || math.Abs(a-b) <= FloatTolerance*math.Max(math.Abs(a), math.Abs(b)) {
			return ""
		}
		change := fmt.Sprintf("%+g", b-a)
		if a != 0 {
			change += fmt.Sprintf(" (%+.3g%%)", 100*(b-a)/math.Abs(a))
		}
		return fmt.Sprintf("%s: %s, was %s, %s", at, g.text, w.text, change)
	case nodeList:
		if len(w.vals) != len(g.vals) {
			return fmt.Sprintf("%s: %d items, was %d", at, len(g.vals), len(w.vals))
		}
		for i := range w.vals {
			if d := diffNode(fmt.Sprintf("%s[%d]", strings.TrimSuffix(at, "."), i), w.vals[i], g.vals[i]); d != "" {
				return d
			}
		}
	case nodeObject:
		if strings.Join(w.keys, "\x00") != strings.Join(g.keys, "\x00") {
			return fmt.Sprintf("%s: fields %v, were %v", at, g.keys, w.keys)
		}
		for i, k := range w.keys {
			if d := diffNode(strings.TrimSuffix(at, ".")+"."+k, w.vals[i], g.vals[i]); d != "" {
				return d
			}
		}
	default:
		if w.text != g.text {
			return fmt.Sprintf("%s: %s, was %s", at, clip(g.bytes()), clip(w.bytes()))
		}
	}
	return ""
}

func clip(b []byte) string {
	const limit = 80
	if len(b) > limit {
		return string(b[:limit]) + "..."
	}
	return string(b)
}

// Manifest is goldens.yaml: every fixture and every file of it. The golden
// check fails when it and the directory disagree.
type Manifest struct {
	Goldens []ManifestEntry `json:"goldens"`
}

// ManifestEntry is one fixture.
type ManifestEntry struct {
	// Name is the fixture's directory under the golden directory.
	Name string `json:"name"`
	// Source says where the recording came from, in words: the scenario,
	// the build, the policy it ran under. No cluster or namespace names.
	Source string `json:"source"`
	// Recording is the exported recording, and Outputs the golden files,
	// each relative to the golden directory.
	Recording string   `json:"recording"`
	Outputs   []string `json:"outputs"`
}

const manifestHeader = `# The learner goldens: each fixture is an exported recording and the output
# files its replay produces (docs/developer-guide/testing.md, Learner Goldens).
# Written by make learner-golden-add and make learner-goldens; never by hand.
# The golden check (make test) fails when a golden no longer reproduces, or
# when this list and the directory disagree.
`

// ReadManifest reads dir's manifest.
func ReadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.UnmarshalStrict(b, &m); err != nil {
		return nil, fmt.Errorf("learnreplay: %s: %w", ManifestName, err)
	}
	return &m, nil
}

func writeManifest(dir string, m *Manifest) error {
	sort.Slice(m.Goldens, func(i, j int) bool { return m.Goldens[i].Name < m.Goldens[j].Name })
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append([]byte(manifestHeader), b...), 0o644)
}

// GoldenCheck is what a golden check found.
type GoldenCheck struct {
	// Problems is every failure: a file the manifest does not list, a listed
	// file missing, a fixture over the size limit, a replay that failed, an
	// output that moved. Empty is a pass.
	Problems []string
	// Fixtures is how many fixtures were replayed, and Rows how many golden
	// rows were compared, per output file: a check that compared nothing is
	// not a pass, and the caller can see that it did not.
	Fixtures int
	Rows     map[string]int
}

// CheckGoldens replays every fixture dir's manifest lists and compares its
// outputs with the committed files.
func CheckGoldens(ctx context.Context, dir string) (*GoldenCheck, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	c := &GoldenCheck{Rows: map[string]int{}}
	problem := func(format string, args ...any) { c.Problems = append(c.Problems, fmt.Sprintf(format, args...)) }
	if len(m.Goldens) == 0 {
		problem("%s lists no fixture", ManifestName)
	}
	listed := map[string]bool{ManifestName: true}
	names := map[string]bool{}
	for _, g := range m.Goldens {
		if names[g.Name] {
			problem("%s lists the fixture %s twice", ManifestName, g.Name)
		}
		names[g.Name] = true
		for _, f := range append([]string{g.Recording}, g.Outputs...) {
			if listed[f] {
				problem("%s lists %s twice", ManifestName, f)
			}
			listed[f] = true
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
				problem("%s lists %s, which is missing", ManifestName, f)
			}
		}
	}
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !listed[rel] {
			problem("%s is not listed in %s", rel, ManifestName)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, g := range m.Goldens {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		got, err := replayFixture(ctx, dir, g)
		if err != nil {
			problem("%s: %v", g.Name, err)
			continue
		}
		c.Fixtures++
		want := map[string]bool{}
		for _, f := range g.Outputs {
			want[path.Base(f)] = true
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
			if err != nil {
				continue // reported above
			}
			out, ok := got[path.Base(f)]
			if !ok {
				problem("%s: %s is not an output a replay produces", g.Name, f)
				continue
			}
			d, err := compareOutput(f, b, out)
			if err != nil {
				problem("%s: %v", g.Name, err)
				continue
			}
			if d != nil {
				problem("%s: golden no longer reproduces: %s", g.Name, d)
			}
			rows, _ := readRows(b)
			c.Rows[f] = len(rows)
		}
		for _, name := range OutputNames {
			if !want[name] {
				problem("%s: the output %s has no golden file in %s", g.Name, name, ManifestName)
			}
		}
	}
	return c, nil
}

// replayFixture replays one fixture as every later commit does (Normal) and
// returns its outputs.
func replayFixture(ctx context.Context, dir string, g ManifestEntry) (map[string][]byte, error) {
	gz, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(g.Recording)))
	if err != nil {
		return nil, err
	}
	if len(gz) > MaxFixtureBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte fixture limit", g.Recording, len(gz), MaxFixtureBytes)
	}
	recording, err := gunzip(gz)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", g.Recording, err)
	}
	return goldenOutputs(ctx, recording, Normal)
}

// goldenOutputs replays recording in mode and returns its outputs; a replay
// that fails in its mode, or analyzes nothing, is an error.
func goldenOutputs(ctx context.Context, recording []byte, mode Mode) (map[string][]byte, error) {
	rep, err := Replay(ctx, recording, Options{Mode: mode})
	if err != nil {
		return nil, err
	}
	if failed := rep.Failed(); len(failed) > 0 {
		return nil, fmt.Errorf("the replay failed: %s (and %d more)", failed[0], len(failed)-1)
	}
	if mode == Fidelity && !bytes.Equal(rep.Recording, recording) {
		return nil, errors.New("the replay did not reproduce the recording byte for byte")
	}
	n := 0
	for _, c := range rep.Analyzes {
		n += c
	}
	if n == 0 {
		return nil, errors.New("the replay analyzed nothing")
	}
	return Outputs(rep.Recording)
}

// AddOptions configure AddGolden.
type AddOptions struct {
	// Source is the manifest's account of where the recording came from.
	Source string
	// HeadLines, when positive, keeps only the recording's first lines: a
	// long run cut to fit MaxFixtureBytes. The cut replays (Replay trims the
	// checks of an Analyze the cut lost).
	HeadLines int
	// Replace lets a fixture of the same name be overwritten.
	Replace bool
}

// fixtureName is what a fixture's directory may be called.
var fixtureName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// AddGolden exports a raw recording (Export), holds the export to a Fidelity
// replay at this commit -- a recording that does not reproduce byte for byte
// is not committed -- and writes it as the fixture name with its goldens, and
// lists it in the manifest. It returns the export's report.
func AddGolden(ctx context.Context, dir, name string, raw []byte, opts AddOptions) (*ExportReport, error) {
	if !fixtureName.MatchString(name) {
		return nil, fmt.Errorf("learnreplay: a fixture name is lowercase letters, digits and dashes: %q", name)
	}
	if opts.HeadLines > 0 {
		raw = headLines(raw, opts.HeadLines)
	}
	exported, rep, err := Export(raw, ExportOptions{})
	if err != nil {
		return nil, err
	}
	outputs, err := goldenOutputs(ctx, exported, Fidelity)
	if err != nil {
		return nil, fmt.Errorf("learnreplay: the export of %s: %w", name, err)
	}
	// The goldens are what every later commit compares with: a Normal
	// replay's, which at this commit must equal the Fidelity one.
	normal, err := goldenOutputs(ctx, exported, Normal)
	if err != nil {
		return nil, fmt.Errorf("learnreplay: the export of %s, Normal: %w", name, err)
	}
	for _, o := range OutputNames {
		if !bytes.Equal(outputs[o], normal[o]) {
			return nil, fmt.Errorf("learnreplay: the export of %s: %s differs between a Fidelity and a Normal replay", name, o)
		}
	}
	gz, err := Gzip(exported)
	if err != nil {
		return nil, err
	}
	if len(gz) > MaxFixtureBytes {
		return nil, fmt.Errorf("learnreplay: the export of %s is %d bytes compressed, over the %d-byte fixture limit; cut it with a head", name, len(gz), MaxFixtureBytes)
	}
	m, err := ReadManifest(dir)
	if errors.Is(err, fs.ErrNotExist) {
		m, err = &Manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	kept := m.Goldens[:0]
	for _, g := range m.Goldens {
		if g.Name == name {
			if !opts.Replace {
				return nil, fmt.Errorf("learnreplay: the fixture %s exists; replace it explicitly", name)
			}
			continue
		}
		kept = append(kept, g)
	}
	m.Goldens = kept
	fixture := filepath.Join(dir, name)
	if opts.Replace {
		if err := os.RemoveAll(fixture); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(fixture, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(fixture, RecordingName), gz, 0o644); err != nil {
		return nil, err
	}
	entry := ManifestEntry{Name: name, Source: opts.Source, Recording: path.Join(name, RecordingName)}
	for _, o := range OutputNames {
		if err := os.WriteFile(filepath.Join(fixture, o), outputs[o], 0o644); err != nil {
			return nil, err
		}
		entry.Outputs = append(entry.Outputs, path.Join(name, o))
	}
	m.Goldens = append(m.Goldens, entry)
	return rep, writeManifest(dir, m)
}

// UpdateGoldens rewrites every fixture's golden files from a Normal replay at
// this commit: what a PR that names a golden change runs.
func UpdateGoldens(ctx context.Context, dir string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	for i, g := range m.Goldens {
		outputs, err := replayFixture(ctx, dir, g)
		if err != nil {
			return fmt.Errorf("learnreplay: %s: %w", g.Name, err)
		}
		for _, o := range g.Outputs {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(o))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		m.Goldens[i].Outputs = nil
		for _, o := range OutputNames {
			if err := os.WriteFile(filepath.Join(dir, g.Name, o), outputs[o], 0o644); err != nil {
				return err
			}
			m.Goldens[i].Outputs = append(m.Goldens[i].Outputs, path.Join(g.Name, o))
		}
	}
	return writeManifest(dir, m)
}

func headLines(b []byte, n int) []byte {
	off := 0
	for i := 0; i < n; i++ {
		j := bytes.IndexByte(b[off:], '\n')
		if j < 0 {
			return b
		}
		off += j + 1
	}
	return b[:off]
}

// Gzip compresses b the same way every time: no name, no time, so a
// fixture written twice is the same bytes.
func Gzip(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
