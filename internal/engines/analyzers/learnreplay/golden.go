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
	"maps"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/slo"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/throughput"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/enginerec"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
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
//     JSON; two integers must be equal, and two other numbers are equal
//     within a relative 1e-9 (FloatTolerance). Everything else is compared
//     exactly.

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

// FloatTolerance is the relative difference under which two non-integer
// numbers in a golden row are equal.
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

// The engine's outputs (issue #345, step 0), from each composite the engine
// events record: written for a fixture whose recording carries the engine
// layer, and for no other.
const (
	// OutputCompositeResults is each composite at model scope: its supply,
	// required and spare capacity, score and liveness, and its result
	// without the per-variant and per-role parts the files below hold.
	OutputCompositeResults = "composite-results.csv"
	// OutputCompositeVariants is each composite's variant capacities, one
	// row per variant, with the decision path that sized it.
	OutputCompositeVariants = "composite-variants.csv"
	// OutputCompositeRoles is each composite's role capacities, one row per
	// role of a P/D model.
	OutputCompositeRoles = "composite-roles.csv"
	// OutputCompositeHolds is each role a composite holds, with the reasons.
	OutputCompositeHolds = "composite-holds.csv"
	// OutputCompositeRelease is, per role another analyzer's frame sizes, the
	// most it may release (ReleaseBound) and what the other frames read of
	// it (OtherFrames).
	OutputCompositeRelease = "composite-release.csv"
	// OutputWarmPoolSupply is what each variant gets from the warm pool's
	// bridges, as the engine publishes it for the pool.
	OutputWarmPoolSupply = "warm-pool-supply.csv"
	// OutputWarmPoolPressure is how hard each variant is pushed, as the
	// engine publishes it for the pool.
	OutputWarmPoolPressure = "warm-pool-pressure.csv"
)

// OutputNames is every analyzer output, in the order the manifest lists
// them. Every fixture has each.
func OutputNames() []string {
	return []string{OutputLearnedState, OutputLines, OutputResults, OutputSLOPublished,
		OutputStoreRecords, OutputVariantCapacities}
}

// EngineOutputNames is every engine output, in the order the manifest lists
// them. A fixture whose recording carries engine events has each.
func EngineOutputNames() []string {
	return []string{OutputCompositeHolds, OutputCompositeRelease, OutputCompositeResults, OutputCompositeRoles,
		OutputCompositeVariants, OutputWarmPoolPressure, OutputWarmPoolSupply}
}

// producedNames is the outputs a replay produced, in manifest order: the
// analyzer outputs, then the engine outputs when it produced them.
func producedNames(outputs map[string][]byte) []string {
	out := OutputNames()
	if _, ok := outputs[OutputCompositeResults]; ok {
		out = append(out, EngineOutputNames()...)
	}
	return out
}

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
	out := make(map[string][]byte)
	names := OutputNames()
	if _, ok := rows[OutputCompositeResults]; ok {
		names = append(names, EngineOutputNames()...)
	}
	for _, name := range names {
		b, err := writeRows(rows[name])
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}

func outputRows(recording []byte) (map[string][]row, error) {
	read, err := learnrec.Read(recording)
	if err != nil {
		return nil, err
	}
	rows := map[string][]row{}
	engine := false
	for _, ev := range read.Events {
		data, err := parseNode(ev.Data)
		if err != nil {
			return nil, fmt.Errorf("learnreplay: seq %d: %w", ev.Seq, err)
		}
		now := ev.Now.Format(time.RFC3339Nano)
		switch ev.Kind {
		case saturation.KindAnalyze, slo.KindAnalyze, throughput.KindAnalyze:
			in := data.get("Input")
			group := in.get("Namespace").str() + "/" + in.get("ModelID").str()
			res := data.get("Result")
			summary := &node{kind: nodeObject, keys: []string{"Error", "Result"}, vals: []*node{data.get("Error"), res}}
			if res != nil && res.kind == nodeObject {
				summary.vals[1] = res.without("VariantCapacities")
				for _, vc := range res.get("VariantCapacities").items() {
					rows[OutputVariantCapacities] = append(rows[OutputVariantCapacities],
						row{ev.Kind, now, group, vc.get("VariantName").str(), string(vc.bytes())})
				}
			}
			rows[OutputResults] = append(rows[OutputResults], row{ev.Kind, now, group, "", string(summary.bytes())})
			if recs := data.get("Records"); recs != nil && recs.kind == nodeObject {
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
					rows[OutputStoreRecords] = append(rows[OutputStoreRecords], row{ev.Kind, now, group, variant, string(picked.bytes())})
				}
			}
			if pub := data.get("Published"); pub != nil && pub.kind != nodeNull {
				rows[OutputSLOPublished] = append(rows[OutputSLOPublished], row{ev.Kind, now, group, "", string(pub.bytes())})
			}
		case enginerec.KindComposite:
			engine = true
			compositeRows(rows, ev.Kind, now, data)
		case saturation.KindPublishLine, saturation.KindBorrowLine:
			rows[OutputLines] = append(rows[OutputLines],
				row{ev.Kind, now, data.get("PhysicsKey").str(), data.get("Variant").str(), string(data.bytes())})
		case saturation.KindExportLearnedState:
			if data.kind != nodeObject {
				continue
			}
			for i, section := range data.keys {
				v := data.vals[i]
				switch v.kind {
				case nodeNull:
				case nodeObject:
					for j, k := range v.keys {
						rows[OutputLearnedState] = append(rows[OutputLearnedState], row{ev.Kind, now, section, k, string(v.vals[j].bytes())})
					}
				default:
					rows[OutputLearnedState] = append(rows[OutputLearnedState], row{ev.Kind, now, section, "", string(v.bytes())})
				}
			}
		}
	}
	if engine {
		for _, name := range EngineOutputNames() {
			if rows[name] == nil {
				rows[name] = []row{}
			}
		}
	}
	return rows, nil
}

// compositeRows adds one composite event's rows to the engine outputs.
func compositeRows(rows map[string][]row, kind, now string, data *node) {
	group := data.get("Namespace").str() + "/" + data.get("ModelID").str()
	add := func(output, item string, n *node) {
		if n == nil {
			n = &node{kind: nodeNull}
		}
		rows[output] = append(rows[output], row{kind, now, group, item, string(n.bytes())})
	}
	comp := data.get("Composite")
	if comp == nil || comp.kind != nodeObject {
		return
	}
	summary := comp.without("RoleCapacities", "Holds", "ReleaseBound", "OtherFrames")
	if res := comp.get("Result"); res != nil && res.kind == nodeObject {
		for i, k := range summary.keys {
			if k == "Result" {
				summary.vals[i] = res.without("VariantCapacities")
			}
		}
		for _, vc := range res.get("VariantCapacities").items() {
			add(OutputCompositeVariants, vc.get("VariantName").str(), vc)
		}
	}
	add(OutputCompositeResults, "", summary)
	if rc := comp.get("RoleCapacities"); rc != nil && rc.kind == nodeObject {
		for i, role := range rc.keys {
			add(OutputCompositeRoles, role, rc.vals[i])
		}
	}
	byRole := func(fields map[string]*node) []string {
		seen := map[string]bool{}
		for _, n := range fields {
			if n != nil && n.kind == nodeObject {
				for _, k := range n.keys {
					seen[k] = true
				}
			}
		}
		return slices.Sorted(maps.Keys(seen))
	}
	pair := func(a string, an *node, b string, bn *node, role string) *node {
		return &node{kind: nodeObject, keys: []string{a, b}, vals: []*node{an.get(role), bn.get(role)}}
	}
	holds, reasons := comp.get("Holds"), data.get("HoldReasons")
	for _, role := range byRole(map[string]*node{"h": holds, "r": reasons}) {
		add(OutputCompositeHolds, role, nullFill(pair("Hold", holds, "Reasons", reasons, role)))
	}
	bound, frames := comp.get("ReleaseBound"), comp.get("OtherFrames")
	for _, role := range byRole(map[string]*node{"b": bound, "f": frames}) {
		add(OutputCompositeRelease, role, nullFill(pair("ReleaseBound", bound, "OtherFrames", frames, role)))
	}
	for _, s := range data.get("Supply").items() {
		add(OutputWarmPoolSupply, s.get("Variant").str(), s)
	}
	for _, p := range data.get("Pressure").items() {
		add(OutputWarmPoolPressure, p.get("Variant").str(), p)
	}
}

// nullFill writes a missing member of n as null.
func nullFill(n *node) *node {
	for i, v := range n.vals {
		if v == nil {
			n.vals[i] = &node{kind: nodeNull}
		}
	}
	return n
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

func sortRows(rows []row) {
	slices.SortStableFunc(rows, func(a, b row) int {
		if c := strings.Compare(a.key(), b.key()); c != 0 {
			return c
		}
		return strings.Compare(a.data, b.data)
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
	keys := slices.Collect(func(yield func(string) bool) {
		for k := range wk {
			if !yield(k) {
				return
			}
		}
		for k := range gk {
			if _, ok := wk[k]; !ok && !yield(k) {
				return
			}
		}
	})
	slices.Sort(keys)
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

// sameData compares two rows' data as JSON and describes the first
// difference ("" when equal).
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

// sameNumber reports whether two numbers' texts are equal outputs: two
// integers exactly, anything else within FloatTolerance.
func sameNumber(want, got string) bool {
	if want == got {
		return true
	}
	if isInteger(want) && isInteger(got) {
		return false
	}
	a, errA := strconv.ParseFloat(want, 64)
	b, errB := strconv.ParseFloat(got, 64)
	if errA != nil || errB != nil {
		return false
	}
	return a == b || math.Abs(a-b) <= FloatTolerance*math.Max(math.Abs(a), math.Abs(b))
}

func isInteger(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
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
		if sameNumber(w.text, g.text) {
			return ""
		}
		a, errA := strconv.ParseFloat(w.text, 64)
		b, errB := strconv.ParseFloat(g.text, 64)
		if errA != nil || errB != nil {
			return fmt.Sprintf("%s: %s, was %s", at, g.text, w.text)
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
		if !slices.Equal(w.keys, g.keys) {
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
	// the build, the policy it ran under, and what it does not cover. No
	// cluster or namespace names; the add refuses a name the recording
	// holds.
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
	slices.SortFunc(m.Goldens, func(a, b ManifestEntry) int { return strings.Compare(a.Name, b.Name) })
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+ManifestName+".tmp")
	if err := os.WriteFile(tmp, append([]byte(manifestHeader), b...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, ManifestName))
}

// GoldenCheck is what a golden check found.
type GoldenCheck struct {
	// Problems is every failure: a file the manifest does not list, a listed
	// file missing, a fixture over the size limit, a replay that failed, an
	// output that moved, a golden that compares nothing. Empty is a pass.
	Problems []string
	// Fixtures is how many fixtures were replayed, and Rows how many golden
	// rows were compared, per output file.
	Fixtures int
	Rows     map[string]int
	// Paths is how often each learner path ran, summed over the fixtures
	// replayed (requiredPaths), and Uncovered names each required path no
	// fixture ran. The committed goldens must leave Uncovered empty (make
	// test and the check command hold them to it); a directory of one new
	// golden, as an add writes it, need not.
	Paths     map[string]int64
	Uncovered []string
	// NotRun names, per fixture, each analyzer its run built and never ran
	// (builtNotRun), for example "e1: slo: built, not run (0 Analyze
	// events)". No golden compares it there. A note, not a problem.
	NotRun []string
}

// requiredPaths is every learner path the goldens must run at least once
// between them: a golden set that never reaches a path pins nothing about
// it (analyzer-learner.md, 2.5). Each can run on this base; the settle
// tracker's verdicts join from the PR that adds the tracker. With the
// engine's replay registered, its composite paths are required too.
func requiredPaths() []string { return append(saturation.PathNames(), enginePathNames()...) }

// pathProblems names each required path the fixtures ran on no cycle.
func pathProblems(paths map[string]int64) []string {
	var out []string
	for _, p := range requiredPaths() {
		if paths[p] <= 0 {
			out = append(out, fmt.Sprintf("no golden runs the learner path %s: a replay that never reaches a path pins nothing about it; add a recording that does (docs/developer-guide/testing.md, Learner Goldens)", p))
		}
	}
	return out
}

// CheckGoldens replays every fixture dir's manifest lists and compares its
// outputs with the committed files.
func CheckGoldens(ctx context.Context, dir string) (*GoldenCheck, error) {
	m, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	c := &GoldenCheck{Rows: map[string]int{}, Paths: map[string]int64{}}
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
		got, paths, notRun, err := replayFixture(ctx, dir, g)
		if err != nil {
			problem("%s: %v", g.Name, err)
			continue
		}
		for _, n := range notRun {
			c.NotRun = append(c.NotRun, g.Name+": "+n)
		}
		c.Fixtures++
		for p, n := range paths {
			c.Paths[p] += n
		}
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
			rows, err := readRows(b)
			if err != nil {
				problem("%s: %s: %v", g.Name, f, err)
				continue
			}
			c.Rows[f] = len(rows)
			d, err := compareOutput(f, b, out)
			if err != nil {
				problem("%s: %v", g.Name, err)
				continue
			}
			if d != nil {
				problem("%s: golden no longer reproduces: %s", g.Name, d)
			}
		}
		for _, name := range producedNames(got) {
			if !want[name] {
				problem("%s: the output %s has no golden file in %s", g.Name, name, ManifestName)
			}
		}
	}
	if c.Fixtures > 0 {
		c.Uncovered = pathProblems(c.Paths)
	}
	return c, nil
}

// replayFixture replays one fixture as every later commit does (Normal) and
// returns its outputs, how often each learner path ran, and each analyzer
// it built and never ran (builtNotRun).
func replayFixture(ctx context.Context, dir string, g ManifestEntry) (map[string][]byte, map[string]int64, []string, error) {
	gz, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(g.Recording)))
	if err != nil {
		return nil, nil, nil, err
	}
	if len(gz) > MaxFixtureBytes {
		return nil, nil, nil, fmt.Errorf("%s is %d bytes, over the %d-byte fixture limit", g.Recording, len(gz), MaxFixtureBytes)
	}
	recording, err := gunzip(gz)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", g.Recording, err)
	}
	return goldenReplay(ctx, recording, Normal)
}

// goldenOutputs is goldenReplay without the path counts, for the callers that
// compare outputs and not coverage: an add, which checks a Fidelity and a
// Normal replay produce the same files, and the tests. The coverage of the
// committed set is CheckGoldens' business, over every fixture together.
func goldenOutputs(ctx context.Context, recording []byte, mode Mode) (map[string][]byte, error) {
	out, _, _, err := goldenReplay(ctx, recording, mode)
	return out, err
}

// goldenReplay replays recording in mode and returns its outputs, how often
// each learner path ran, and each analyzer the run built and never ran
// (builtNotRun). It is an error when the replay fails in its mode, and when
// the goldens would compare nothing (comparedNothing): the same rule for the
// check make test runs and for an add.
func goldenReplay(ctx context.Context, recording []byte, mode Mode) (map[string][]byte, map[string]int64, []string, error) {
	rep, err := Replay(ctx, recording, Options{Mode: mode})
	if err != nil {
		return nil, nil, nil, err
	}
	for kind := range rep.Ignored {
		if enginerec.Owns(kind) {
			return nil, nil, nil, fmt.Errorf("the recording carries engine events (%s) and no engine replay is registered (learnreplay.RegisterEngine)", kind)
		}
	}
	if failed := rep.Failed(); len(failed) > 0 {
		return nil, nil, nil, fmt.Errorf("the replay failed: %s (%d in all)", failed[0], len(failed))
	}
	if mode == Fidelity && !bytes.Equal(rep.Recording, recording) {
		return nil, nil, nil, fmt.Errorf("the replay did not reproduce the recording byte for byte: %s", firstDifferentLine(recording, rep.Recording))
	}
	outputs, err := Outputs(rep.Recording)
	if err != nil {
		return nil, nil, nil, err
	}
	var start learnrec.Start
	read, err := learnrec.Read(recording)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := read.Events[0].DecodeData(&start); err != nil {
		return nil, nil, nil, err
	}
	notRun := builtNotRun(start, read.Events)
	if missing := comparedNothing(start, outputs, notRun); len(missing) > 0 {
		return nil, nil, nil, fmt.Errorf("the goldens would compare nothing for %s", strings.Join(missing, ", "))
	}
	var notes []string
	for _, a := range notRun {
		notes = append(notes, a+": built, not run (0 Analyze events)")
	}
	return outputs, rep.Paths, notes, nil
}

// builtNotRun names each analyzer the start line says the run built and that
// ran no Analyze in the recording. The controller builds the slo analyzer
// whatever the policy lists, so a run whose models list only saturation
// records an slo start and never an slo Analyze: it has no slo results to
// compare, and comparedNothing asks for none. An analyzer with even one
// Analyze ran, and comparedNothing holds it to its results. Saturation is
// not among them: its results are what every golden compares, so a
// recording without a saturation Analyze is refused.
func builtNotRun(start learnrec.Start, events []learnrec.RawEvent) []string {
	analyzed := map[string]int{}
	for _, e := range events {
		analyzed[e.Kind]++
	}
	var out []string
	if start.SLO != nil && analyzed[slo.KindAnalyze] == 0 {
		out = append(out, "slo")
	}
	if start.Throughput && analyzed[throughput.KindAnalyze] == 0 {
		out = append(out, "throughput")
	}
	return out
}

// comparedNothing names what a fixture's goldens would not compare: a golden
// that compares nothing passes whatever changes. Every analyzer the start
// says ran must have results, and the results must price variants and read
// the store; the slo analyzer, when it ran, must have published. An analyzer
// in notRun was built and ran no Analyze (builtNotRun): nothing is asked of
// it.
//
// Two outputs may be empty whatever ran, and the start cannot say when:
// lines.csv holds ITL lines published and borrowed, which happen only when
// the policy turns reuse on (EnableLearnedStateReuse) and a variant fits a
// line; learned-state.csv holds the snapshot documents, written only when
// the controller's learned-state snapshot is on. Both are the run's
// configuration, not an analyzer's, and the start line records neither.
func comparedNothing(start learnrec.Start, outputs map[string][]byte, notRun []string) []string {
	count := func(name string, kind string) int {
		rows, err := readRows(outputs[name])
		if err != nil {
			return 0
		}
		n := 0
		for _, r := range rows {
			if kind == "" || r.kind == kind {
				n++
			}
		}
		return n
	}
	var missing []string
	need := func(ok bool, what string) {
		if !ok {
			missing = append(missing, what)
		}
	}
	need(count(OutputResults, saturation.KindAnalyze) > 0, "saturation results")
	need(count(OutputVariantCapacities, "") > 0, "variant capacities")
	need(count(OutputStoreRecords, "") > 0, "store records")
	if start.SLO != nil && !slices.Contains(notRun, "slo") {
		need(count(OutputResults, slo.KindAnalyze) > 0, "slo results")
		need(count(OutputSLOPublished, "") > 0, "slo published")
	}
	if start.Throughput && !slices.Contains(notRun, "throughput") {
		need(count(OutputResults, throughput.KindAnalyze) > 0, "throughput results")
	}
	if _, ok := outputs[OutputCompositeResults]; ok {
		need(count(OutputCompositeResults, "") > 0, "composites")
		need(count(OutputCompositeVariants, "") > 0, "composite variants")
		need(count(OutputWarmPoolPressure, "") > 0, "warm-pool pressure")
	}
	return missing
}

// firstDifferentLine names the first line where two recordings differ, by
// its sequence number and kind.
func firstDifferentLine(want, got []byte) string {
	w := bytes.Split(want, []byte("\n"))
	g := bytes.Split(got, []byte("\n"))
	for i := 0; i < max(len(w), len(g)); i++ {
		var a, b []byte
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if bytes.Equal(a, b) {
			continue
		}
		line := a
		if len(line) == 0 {
			line = b
		}
		n, err := parseNode(line)
		if err != nil {
			return fmt.Sprintf("line %d", i+1)
		}
		return fmt.Sprintf("first at seq %s (%s)", n.get("Seq").text, n.get("Kind").str())
	}
	return "no line differs"
}

// AddOptions configure AddGolden.
type AddOptions struct {
	// Source is the manifest's account of where the recording came from. It
	// may not hold a name the recording holds.
	Source string
	// HeadLines, when positive, keeps only the recording's first lines: a
	// long run cut to fit MaxFixtureBytes. The export cuts what follows the
	// last Analyze, so any head replays.
	HeadLines int
	// Replace lets a fixture of the same name be overwritten.
	Replace bool
	// Commit is the commit the add runs at (learnrec.BuildCommit). A
	// recording made at another commit is refused: its replay need not
	// reproduce it. "unknown" on either side skips the check.
	Commit string
}

// fixtureName is what a fixture's directory may be called.
var fixtureName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// AddGolden exports a raw recording (Export), holds the export to a Fidelity
// replay at this commit -- a recording that does not reproduce byte for byte
// is not committed -- and writes it as the fixture name with its goldens,
// listed in the manifest. The cheap refusals come first: the name, the
// commit, the size. The fixture is written to a temporary directory and
// moved into place, and the manifest is written last, so a refusal or a
// failure leaves the golden directory as it was.
func AddGolden(ctx context.Context, dir, name string, raw []byte, opts AddOptions) (*ExportReport, error) {
	if !fixtureName.MatchString(name) {
		return nil, fmt.Errorf("learnreplay: a fixture name is lowercase letters, digits and dashes: %q", name)
	}
	m, err := ReadManifest(dir)
	if errors.Is(err, fs.ErrNotExist) {
		m, err = &Manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	listed := slices.ContainsFunc(m.Goldens, func(g ManifestEntry) bool { return g.Name == name })
	if listed && !opts.Replace {
		return nil, fmt.Errorf("learnreplay: the fixture %s exists; replace it explicitly", name)
	}
	fixture := filepath.Join(dir, name)
	if _, err := os.Stat(fixture); err == nil && !listed {
		return nil, fmt.Errorf("learnreplay: %s exists and %s does not list it; it is not this tool's to replace", fixture, ManifestName)
	}
	// The commit first, before anything else reads the recording: one made
	// at another commit need not replay here, and need not even export.
	if recorded := recordedCommit(raw); knownCommit(recorded) && knownCommit(opts.Commit) && recorded != opts.Commit {
		return nil, fmt.Errorf("learnreplay: the recording was made at %s and you are at %s: its replay here need not reproduce it. Add it from a checkout of %s and carry the fixture over (docs/developer-guide/testing.md)", recorded, opts.Commit, recorded)
	}
	if err := removeStaleAdds(dir, name); err != nil {
		return nil, err
	}
	if opts.HeadLines > 0 {
		raw = headLines(raw, opts.HeadLines)
	}
	exported, rep, err := Export(raw, ExportOptions{Source: opts.Source, Text: []string{name}})
	if err != nil {
		return nil, err
	}
	gz, err := Gzip(exported)
	if err != nil {
		return nil, err
	}
	if len(gz) > MaxFixtureBytes {
		head := suggestHead(exported, len(gz), MaxFixtureBytes)
		if head == 0 {
			return nil, fmt.Errorf("learnreplay: the export of %s is %d bytes compressed, over the %d-byte fixture limit, and no head that fits holds an Analyze of every analyzer the run had", name, len(gz), MaxFixtureBytes)
		}
		return nil, fmt.Errorf("learnreplay: the export of %s is %d bytes compressed, over the %d-byte fixture limit; cut it to its head, for example HEAD_LINES=%d", name, len(gz), MaxFixtureBytes, head)
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
	for _, o := range producedNames(outputs) {
		if !bytes.Equal(outputs[o], normal[o]) {
			return nil, fmt.Errorf("learnreplay: the export of %s: %s differs between a Fidelity and a Normal replay", name, o)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(dir, ".add-"+name+"-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.WriteFile(filepath.Join(tmp, RecordingName), gz, 0o644); err != nil {
		return nil, err
	}
	entry := ManifestEntry{Name: name, Source: opts.Source, Recording: path.Join(name, RecordingName)}
	for _, o := range producedNames(outputs) {
		if err := os.WriteFile(filepath.Join(tmp, o), outputs[o], 0o644); err != nil {
			return nil, err
		}
		entry.Outputs = append(entry.Outputs, path.Join(name, o))
	}
	old := ""
	if listed {
		old = filepath.Join(tmp, "old")
		if err := os.Rename(fixture, old); err != nil {
			return nil, err
		}
	}
	fresh := filepath.Join(dir, ".add-"+name+"-ready")
	if err := os.Rename(tmp, fresh); err != nil {
		return nil, restore(err, old, fixture)
	}
	tmp = fresh // cleaned up by the deferred RemoveAll if anything below fails
	if err := os.Rename(fresh, fixture); err != nil {
		return nil, restore(err, filepath.Join(fresh, "old"), fixture)
	}
	tmp = ""
	if old != "" {
		if err := os.RemoveAll(filepath.Join(fixture, "old")); err != nil {
			return nil, err
		}
	}
	m.Goldens = slices.DeleteFunc(m.Goldens, func(g ManifestEntry) bool { return g.Name == name })
	m.Goldens = append(m.Goldens, entry)
	return rep, writeManifest(dir, m)
}

// restore moves a replaced fixture back after a failed move, and returns
// the failure.
func restore(err error, old, fixture string) error {
	if old != "" {
		if rerr := os.Rename(old, fixture); rerr != nil {
			return fmt.Errorf("%w; and the old fixture could not be put back from %s: %v", err, old, rerr)
		}
	}
	return err
}

// UpdateGoldens rewrites every fixture's golden files from a Normal replay at
// this commit: what a PR that names a golden change runs. It repairs what
// the check reports: a moved output, a missing file, an output the manifest
// leaves out. A fixture whose replay fails leaves its files as they were.
func UpdateGoldens(ctx context.Context, dir string) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	for i, g := range m.Goldens {
		outputs, _, _, err := replayFixture(ctx, dir, g)
		if err != nil {
			return fmt.Errorf("learnreplay: %s: %w", g.Name, err)
		}
		for _, o := range g.Outputs {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(o))); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		m.Goldens[i].Outputs = nil
		for _, o := range producedNames(outputs) {
			if err := os.WriteFile(filepath.Join(dir, g.Name, o), outputs[o], 0o644); err != nil {
				return err
			}
			m.Goldens[i].Outputs = append(m.Goldens[i].Outputs, path.Join(g.Name, o))
		}
	}
	return writeManifest(dir, m)
}

// headLines is b's first n lines.
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

// suggestHead is a HEAD_LINES that fits limit, from how well this export
// compressed, snapped back to the last line of an Analyze after which
// every analyzer the export ran has analyzed: a head whose goldens
// would compare nothing (comparedNothing) is no suggestion. Zero when no
// head fits that holds them all.
func suggestHead(exported []byte, compressed, limit int) int {
	budget := int(float64(len(exported)) * 0.9 * float64(limit) / float64(compressed))
	read, err := learnrec.Read(exported)
	if err != nil || len(read.Events) == 0 {
		return 0
	}
	var start learnrec.Start
	if err := read.Events[0].DecodeData(&start); err != nil {
		return 0
	}
	// An analyzer the whole export never ran (builtNotRun) has no Analyze
	// for a head to hold.
	notRun := builtNotRun(start, read.Events)
	need := map[string]bool{saturation.KindAnalyze: true}
	if start.SLO != nil && !slices.Contains(notRun, "slo") {
		need[slo.KindAnalyze] = true
	}
	if start.Throughput && !slices.Contains(notRun, "throughput") {
		need[throughput.KindAnalyze] = true
	}
	best, size := 0, 0
	for i, ev := range read.Events {
		size += len(ev.Line) + 1
		if size > budget {
			break
		}
		delete(need, ev.Kind)
		if strings.HasSuffix(ev.Kind, ".Analyze") && len(need) == 0 {
			best = i + 1
		}
	}
	return best
}

// recordedCommit is the commit a raw recording's start line names, "" when
// it names none or cannot be read.
func recordedCommit(raw []byte) string {
	line, _, _ := bytes.Cut(raw, []byte("\n"))
	n, err := parseNode(line)
	if err != nil || n.get("Kind").str() != learnrec.KindStart {
		return ""
	}
	return n.get("Data").get("Commit").str()
}

func knownCommit(c string) bool { return c != "" && c != "unknown" }

// removeStaleAdds deletes what an add of name that was killed midway left:
// the temporary directories it owns (".add-<name>-*").
func removeStaleAdds(dir, name string) error {
	stale, err := filepath.Glob(filepath.Join(dir, ".add-"+name+"-*"))
	if err != nil {
		return err
	}
	for _, s := range stale {
		if err := os.RemoveAll(s); err != nil {
			return fmt.Errorf("learnreplay: a killed add left %s, and it could not be removed: %w", s, err)
		}
	}
	return nil
}
