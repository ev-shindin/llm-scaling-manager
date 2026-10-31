package learnreplay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// Export turns a recording into one that can be committed as a fixture
// (docs/proposals/analyzer-learner.md, 2.4, "before commit"), and that still
// replays byte for byte at the commit that recorded it.
//
// What it does, in order:
//
//   - It cuts the events after the last Analyze: a recording cut short (at
//     its cap, by a crash, or by a head) can end inside a cycle, and those
//     events have no Analyze to be checked against.
//   - It renames every name a cluster gives -- namespaces, models, variants,
//     pods, images, revisions, forget tokens, engine-argument text, label
//     and taint keys and values outside Kubernetes' and the GPU vendors'
//     own, resource names, priority and weight classes, limiter names --
//     wherever it stands: a field, a map key, a part of a composite key
//     ("ns|model|variant|..."), or a message. A pseudonym is the category
//     and the name's rank among the category's names ("variant-03"), so
//     names keep their order within a category; the same name gets the same
//     pseudonym everywhere. A map whose keys were renamed is re-sorted as
//     the recorder writes one.
//   - It replaces host names, IP addresses and URLs in messages, and
//     configuration text no analyzer reads (queries, decode errors).
//   - It recomputes every engine fingerprint from the renamed parameters
//     (a fingerprint hashes the model path and the image). A fingerprint
//     with no recorded parameters behind it is replaced by a hash under a
//     key derived from the raw recording, which is never committed.
//   - It then holds every string and map key to an allowlist: a pseudonym,
//     the code's own vocabulary, a well-known label, an accelerator name, a
//     time, a number, a fingerprint, or a composite or message made only of
//     these. Anything else stops the export, naming where it stands, never
//     the value.
//
// What it keeps: numbers and times (load, capacity, the shape of the run),
// accelerator product names, and the code's vocabulary.
func Export(recording []byte, opts ExportOptions) ([]byte, *ExportReport, error) {
	read, err := learnrec.Read(recording)
	if err != nil {
		return nil, nil, err
	}
	events := read.Events
	if len(events) == 0 || events[0].Kind != learnrec.KindStart {
		return nil, nil, errors.New("learnreplay: export: the recording has no start line")
	}
	last := lastAnalyze(events)
	if last < 0 {
		return nil, nil, errors.New("learnreplay: export: the recording has no Analyze event")
	}
	rep := &ExportReport{Trimmed: len(events) - last - 1, TornTail: read.TornTail != "", Names: map[string]int{}}
	events = events[:last+1]
	rep.Events = len(events)
	var start learnrec.Start
	if err := events[0].DecodeData(&start); err != nil {
		return nil, nil, fmt.Errorf("learnreplay: export: start: %w", err)
	}
	rep.Commit = start.Commit

	key := opts.Key
	if len(key) == 0 {
		sum := sha256.Sum256(append([]byte("learnreplay export key v1\x00"), recording...))
		key = sum[:]
	}
	x := &exporter{
		names:      map[string]string{},
		pseudonyms: map[string]bool{},
		byCategory: map[string]map[string]bool{},
		lower:      map[string]string{},
		kept:       map[string]bool{},
		keyed:      map[string]string{},
		mac:        hmac.New(sha256.New, key),
	}
	trees := make([]*node, len(events))
	for i, ev := range events {
		n, err := parseNode(ev.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("learnreplay: export: seq %d: %w", ev.Seq, err)
		}
		trees[i] = n
	}
	var composites []string
	for i, n := range trees {
		x.walk(n, events[i].Kind, func(at site, s *string) { x.harvest(at, *s, &composites) })
	}
	if len(x.problems) > 0 {
		return nil, nil, x.refusal()
	}
	x.harvestComposites(composites)
	if err := x.assign(); err != nil {
		return nil, nil, err
	}
	var params []paramsSite
	for _, n := range trees {
		findParams(n, "", &params)
	}
	for i := range params {
		if err := params[i].fingerprint(true); err != nil {
			return nil, nil, err
		}
	}
	for i, n := range trees {
		x.walk(n, events[i].Kind, func(at site, s *string) { *s = x.rename(at, *s) })
	}
	fps := map[string]string{}
	for i := range params {
		p := &params[i]
		if err := p.fingerprint(false); err != nil {
			return nil, nil, err
		}
		for _, pair := range [][2]string{{p.oldFP, p.newFP}, {p.oldArgs, p.newArgs}} {
			if prev, ok := fps[pair[0]]; ok && prev != pair[1] {
				return nil, nil, fmt.Errorf("learnreplay: export: fingerprint %s renames two ways", pair[0])
			}
			fps[pair[0]] = pair[1]
		}
	}
	x.fingerprints = fps
	for i, n := range trees {
		x.walk(n, events[i].Kind, func(_ site, s *string) { *s = x.replaceFingerprints(*s) })
	}
	for i, n := range trees {
		x.walk(n, events[i].Kind, x.check)
	}
	if len(x.problems) > 0 {
		return nil, nil, x.refusal()
	}
	if tok := x.nameIn(opts.Source); tok != "" {
		return nil, nil, fmt.Errorf("learnreplay: export: the source holds a name (the one behind %s)", x.lower[tok])
	}
	if x.holdsName(opts.Source) {
		return nil, nil, errors.New("learnreplay: export: the source holds a recorded name")
	}
	if bad := sourceRefusals(opts.Source); len(bad) > 0 {
		return nil, nil, fmt.Errorf("learnreplay: export: the source holds words that are not ordinary words (%s). Write it short, of ordinary words, such as %q; a word it needs can be added to source_words.txt", strings.Join(bad, ", "), SourceExample)
	}
	for _, t := range opts.Text {
		if err := x.checkFixtureName(t); err != nil {
			return nil, nil, err
		}
	}

	var out bytes.Buffer
	for i, ev := range events {
		line, err := learnrec.Line(ev.Seq, ev.Now, ev.Kind, trees[i].bytes())
		if err != nil {
			return nil, nil, err
		}
		out.Write(line)
	}
	if _, err := learnrec.Read(out.Bytes()); err != nil {
		return nil, nil, fmt.Errorf("learnreplay: export: the export does not read back: %w", err)
	}
	for c, set := range x.byCategory {
		rep.Names[c] = len(set)
	}
	rep.Fingerprints, rep.KeyedFingerprints = x.mapped, x.keyedCount
	rep.Kept = slices.Sorted(maps.Keys(x.kept))
	return out.Bytes(), rep, nil
}

// lastAnalyze is the index of the last Analyze event, -1 when there is none.
func lastAnalyze(events []learnrec.RawEvent) int {
	for i := len(events) - 1; i >= 0; i-- {
		if strings.HasSuffix(events[i].Kind, ".Analyze") {
			return i
		}
	}
	return -1
}

// ExportOptions configure Export.
type ExportOptions struct {
	// Key keys the hash that replaces a fingerprint with no recorded
	// parameters behind it. Empty derives one from the raw recording, so one
	// recording always exports to the same bytes, and the key, like the raw
	// recording, is never committed.
	Key []byte
	// Source is the manifest's account of the recording, committed beside
	// the export: it may hold no name, and only ordinary words
	// (sourceRefusals).
	Source string
	// Text is other text that will be committed beside the export (the
	// fixture's name): it may hold no name.
	Text []string
}

// ExportReport says what an export did, in counts and kept vocabulary:
// never a name.
type ExportReport struct {
	// Events is how many events were exported, and Trimmed how many after
	// the last Analyze were cut.
	Events, Trimmed int
	// Names is, per category, how many distinct names became pseudonyms.
	Names map[string]int
	// Fingerprints is how many fingerprint occurrences were recomputed from
	// renamed parameters, and KeyedFingerprints how many had none behind
	// them and were replaced by the keyed hash.
	Fingerprints, KeyedFingerprints int
	// TornTail is whether a last line cut off mid-write was dropped.
	TornTail bool
	// Commit is the commit the recording says it was built from.
	Commit string
	// Kept is every distinct string the export kept as it is because it is
	// vocabulary, an accelerator, a duration, a flag or a well-known label:
	// for a person to read before committing.
	Kept []string
}

// String is the report on one line, without Kept.
func (r *ExportReport) String() string {
	cats := slices.Sorted(maps.Keys(r.Names))
	parts := make([]string, 0, len(cats))
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s=%d", c, r.Names[c]))
	}
	return fmt.Sprintf("%d events (%d cut after the last Analyze); names renamed: %s; fingerprints recomputed %d, keyed %d; torn tail dropped: %t; recorded at commit %s",
		r.Events, r.Trimmed, strings.Join(parts, " "), r.Fingerprints, r.KeyedFingerprints, r.TornTail, r.Commit)
}

// site is where a string stands: the event kind, the Go field it sits under
// and that field's parent field, and for a map key the map's key rule name.
type site struct {
	kind, field, parent string
	// key is set for a map key: the keyRules name of its map.
	key   string
	isKey bool
}

// where describes a site for a refusal, without its value.
func (s site) where() string {
	field := s.field
	if s.parent != "" {
		field = s.parent + "." + s.field
	}
	if s.isKey {
		return fmt.Sprintf("a key of %s in %s", s.key, s.kind)
	}
	if field == "" {
		field = "(the event data)"
	}
	return fmt.Sprintf("%s in %s", field, s.kind)
}

// valueRule is the rule for the string value at s, and whether there is one.
func (s site) valueRule() (rule, bool) {
	if r, ok := valueRules[s.parent+"."+s.field]; ok && s.parent != "" {
		return r, true
	}
	r, ok := valueRules[s.field]
	return r, ok
}

// keyRule is the rule for a map key at s, and whether there is one.
func (s site) keyRule() (rule, bool) {
	r, ok := keyRules[s.key]
	return r, ok
}

// compositeSite reports whether the string at a site is a composite key, by its
// rule.
func (x *exporter) compositeSite(at site) bool {
	r, ok := at.valueRule()
	if at.isKey {
		r, ok = at.keyRule()
	}
	return ok && r.class == classComposite
}

// walk calls fn on every string value and every map key under n, with where
// it stands. A map whose keys fn changed is re-sorted.
func (x *exporter) walk(n *node, kind string, fn func(site, *string)) {
	x.walkAt(n, site{kind: kind}, 0, fn)
}

func (x *exporter) walkAt(n *node, at site, depth int, fn func(site, *string)) {
	switch n.kind {
	case nodeString:
		at.isKey = false
		fn(at, &n.text)
	case nodeList:
		for _, v := range n.vals {
			x.walkAt(v, at, depth, fn)
		}
	case nodeObject:
		keyName := at.field + strings.Repeat("{}", depth)
		if at.field == "" {
			keyName = at.kind
		}
		// A map-typed field is a map whatever its keys look like: a node
		// selector whose keys are spelled like Go fields is still a map.
		_, mapped := keyRules[keyName]
		isMap := mapped && !structSites[keyName]
		if !isMap && isStruct(n) {
			for i, k := range n.keys {
				child := site{kind: at.kind, field: k, parent: at.field}
				x.walkAt(n.vals[i], child, 0, fn)
			}
			return
		}
		keyAt := at
		keyAt.isKey = true
		keyAt.key = keyName
		changed := false
		for i := range n.keys {
			before := n.keys[i]
			fn(keyAt, &n.keys[i])
			changed = changed || n.keys[i] != before
			x.walkAt(n.vals[i], at, depth+1, fn)
		}
		if changed {
			n.sortKeys()
		}
	}
}

// isStruct reports an object written from a struct: every key a Go field
// name of the recorded types. Anything else is a map.
func isStruct(n *node) bool {
	if len(n.keys) == 0 {
		return false
	}
	for _, k := range n.keys {
		if !fieldNames[k] {
			return false
		}
	}
	return true
}

// exporter is one export's state.
type exporter struct {
	// byCategory is each category's names; names is every name's pseudonym,
	// and pseudonyms every pseudonym given.
	byCategory map[string]map[string]bool
	names      map[string]string
	pseudonyms map[string]bool
	// lower is every name, lowercased, with its pseudonym, for renaming
	// inside a message whatever its case; tokens is lower's keys, longest
	// first, so a pod name is replaced before the variant name it starts
	// with.
	lower map[string]string
	// longNames are the lowercased names of minBackstopName characters or
	// more, for the backstop (holdsName).
	longNames []string
	tokens    []string
	// kept is every string kept as vocabulary, for the report.
	kept         map[string]bool
	fingerprints map[string]string
	// keyed caches the keyed hash of each fingerprint without parameters.
	keyed      map[string]string
	mac        hashMAC
	mapped     int
	keyedCount int
	// problems are the sites that stopped the export.
	problems map[string]bool
}

// hashMAC is the part of hash.Hash the keyed fingerprint uses.
type hashMAC interface {
	Reset()
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func (x *exporter) problem(format string, args ...any) {
	if x.problems == nil {
		x.problems = map[string]bool{}
	}
	x.problems[fmt.Sprintf(format, args...)] = true
}

func (x *exporter) refusal() error {
	return fmt.Errorf("learnreplay: export: nothing was written: %s", strings.Join(slices.Sorted(maps.Keys(x.problems)), "; "))
}

func (x *exporter) add(category, name string) {
	if x.byCategory[category] == nil {
		x.byCategory[category] = map[string]bool{}
	}
	x.byCategory[category][name] = true
}

// harvest collects the names a string is, and refuses one under a field no
// table classifies.
func (x *exporter) harvest(at site, s string, composites *[]string) {
	if s == "" {
		return
	}
	var r rule
	var ok bool
	if at.isKey {
		if r, ok = at.keyRule(); !ok {
			return // checked as an atom after the export
		}
	} else if r, ok = at.valueRule(); !ok {
		if isTime(s) || isNumber(s) {
			return
		}
		x.problem("%s is a string field the export does not classify (add it to valueRules)", at.where())
		return
	}
	switch r.class {
	case classRename:
		// Digits alone are a revision number; renaming them would rename
		// every count that reads the same. Any other category's digits are
		// a name.
		if r.category == "revision" && strings.Trim(s, "0123456789") == "" {
			return
		}
		x.add(r.category, s)
	case classComposite:
		if r.category != "" && !strings.ContainsAny(s, "|/") {
			x.add(r.category, s)
			return
		}
		*composites = append(*composites, s)
	case classAccelerator:
		if !knownAccelerators[s] {
			x.add(r.category, s)
		}
	case classLabelKey, classResource:
		if !knownLabels[s] {
			x.add(r.category, s)
		}
	}
}

// keyPart is what one part of a composite key is, by its place.
type keyPart int

const (
	partName keyPart = iota
	partAccelerator
	partCount
	partFingerprint
	// partShape is a role, a length bucket or a queue threshold: the shape
	// parts of a k2 or throughput window key (shapePart).
	partShape
)

// compositeLayout is what each '|'-separated part of a composite key is,
// by the number of parts: the variant key (namespace|model|variant|
// accelerator|GPUs, saturation.itlWindowKey), the physics key
// (model|accelerator|GPUs|fingerprint, capacity.LearnedStateKey), the k2
// window key (namespace|variant|model|accelerator|GPUs|role|output
// bucket|qN, saturation's historyKey) and the throughput window key (the
// same with the input bucket before the output one, throughputKey), and a
// key of names alone otherwise (namespace/variant).
//
// Without the window keys' layouts every part of one was a name: the GPU
// count of a k2 window was renamed as a name, and every variant key
// holding the same count then failed its count part.
func compositeLayout(parts int) []keyPart {
	switch parts {
	case 5:
		return []keyPart{partName, partName, partName, partAccelerator, partCount}
	case 4:
		return []keyPart{partName, partAccelerator, partCount, partFingerprint}
	case 8:
		return []keyPart{partName, partName, partName, partAccelerator, partCount, partShape, partShape, partShape}
	case 9:
		return []keyPart{partName, partName, partName, partAccelerator, partCount, partShape, partShape, partShape, partShape}
	}
	out := make([]keyPart, parts)
	return out
}

// splitComposite splits a composite key into its parts and their places,
// and the stash tag a window put aside under other engine arguments carries
// after a '#' (saturation's stashKey: key + "#" + the arguments' tag), with
// whether it has one. The layout is the key's without the tag, so a stashed
// variant key (ITLBaselineStash) or window key has its own key's places.
func splitComposite(s string) (parts []string, layout []keyPart, tag string, tagged bool) {
	key, tag, tagged := strings.Cut(s, "#")
	parts = strings.Split(key, "|")
	return parts, compositeLayout(len(parts)), tag, tagged
}

// joinComposite puts a key split by splitComposite back together.
func joinComposite(parts []string, tag string, tagged bool) string {
	out := strings.Join(parts, "|")
	if tagged {
		out += "#" + tag
	}
	return out
}

// stashTag reports whether tag is a stash key's tag: an engine arguments'
// fingerprint, possibly of a configuration not read completely
// (capacity.IncompleteArgsPrefix), or the tag a restore gives a window of a
// document that recorded no arguments (saturation's legacyArgsPrefix,
// "legacy:", before such a fingerprint).
func stashTag(tag string) bool {
	return isFingerprint(strings.TrimPrefix(tag, legacyArgsPrefix))
}

// legacyArgsPrefix is saturation's legacyArgsPrefix, which it does not
// export.
const legacyArgsPrefix = "legacy:"

// harvestComposites names every name part of a composite key that is not a
// name already: a name only a key holds (a variant of another namespace in
// a restored snapshot) is still a name, whatever it looks like -- a number,
// a vocabulary word, a product name. A name part that is no name whole is
// split at '/', and each piece is a name.
func (x *exporter) harvestComposites(composites []string) {
	known := map[string]bool{}
	for _, set := range x.byCategory {
		for s := range set {
			known[s] = true
		}
	}
	for _, c := range composites {
		parts, layout, _, _ := splitComposite(c)
		for i, part := range parts {
			switch layout[i] {
			case partAccelerator:
				if !knownAccelerators[part] && !known[part] {
					x.add("accelerator", part)
					known[part] = true
				}
			case partCount, partFingerprint, partShape:
			default:
				if known[part] || part == "" {
					continue
				}
				for _, a := range strings.Split(part, "/") {
					if a != "" && !known[a] {
						x.add("name", a)
						known[a] = true
					}
				}
			}
		}
	}
}

// categoryOrder settles a name found under two categories: the first wins.
var categoryOrder = []string{"ns", "model", "variant", "pod", "image", "revision", "token", "arg", "label",
	"labelkey", "resource", "accelerator", "priority", "weightclass", "limiter", "name"}

// assign gives every name its pseudonym.
func (x *exporter) assign() error {
	// A label value that names a known accelerator is hardware, not a name.
	for s := range x.byCategory["label"] {
		if knownAccelerators[s] {
			delete(x.byCategory["label"], s)
		}
	}
	for _, cat := range categoryOrder {
		set := x.byCategory[cat]
		var names []string
		for s := range set {
			if _, taken := x.names[s]; taken {
				delete(set, s)
				continue
			}
			names = append(names, s)
		}
		slices.Sort(names)
		width := max(2, len(strconv.Itoa(len(names))))
		for i, s := range names {
			x.names[s] = fmt.Sprintf("%s-%0*d", cat, width, i+1)
		}
	}
	// A name that is its own pseudonym (an export exported again) is fine;
	// a name that is another name's pseudonym could not be told apart.
	for s, p := range x.names {
		if other, clash := x.names[p]; clash && other != p {
			return fmt.Errorf("learnreplay: export: the recording already holds a name shaped like a pseudonym (%s); it cannot be told apart after the export", p)
		}
		x.pseudonyms[p] = true
		x.lower[asciiLower(s)] = p
		if len(s) >= minBackstopName && s != p {
			x.longNames = append(x.longNames, asciiLower(s))
		}
	}
	x.tokens = slices.Collect(maps.Keys(x.lower))
	slices.SortFunc(x.tokens, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	return nil
}

// rename is the string at a site after the export.
func (x *exporter) rename(at site, s string) string {
	if s == "" {
		return s
	}
	var r rule
	var ok bool
	if at.isKey {
		r, ok = at.keyRule()
	} else {
		r, ok = at.valueRule()
	}
	if !ok {
		return s
	}
	switch r.class {
	case classRename, classLabelKey, classResource, classAccelerator:
		if p, ok := x.names[s]; ok {
			return p
		}
	case classComposite:
		return x.renameComposite(s)
	case classText:
		if x.namesOverlap(s) {
			x.problem("%s holds two recorded names that overlap", at.where())
		}
		return scrub(x.renameText(s))
	case classRedact:
		return "redacted"
	}
	return s
}

// renameComposite renames each name part of a composite key: whole when it
// is a name, else each piece between '/'. The accelerator part is renamed
// whole when it is a name. A count, a shape part, a fingerprint and the stash
// tag are never renamed: they are figures and code words, and a namespace
// that happens to be called "medium" or "decode" must not rename the bucket
// or the role of every key.
func (x *exporter) renameComposite(s string) string {
	parts, layout, tag, tagged := splitComposite(s)
	for i, part := range parts {
		if layout[i] != partName && layout[i] != partAccelerator {
			continue
		}
		if p, ok := x.names[part]; ok {
			parts[i] = p
			continue
		}
		if layout[i] != partName {
			continue
		}
		sub := strings.Split(part, "/")
		for j, a := range sub {
			if p, ok := x.names[a]; ok {
				sub[j] = p
			}
		}
		parts[i] = strings.Join(sub, "/")
	}
	return joinComposite(parts, tag, tagged)
}

// wordByte is a byte a name runs on through: a name in a message matches
// only where the bytes either side are not one of these, so "rec" is not
// found in "record".
func wordByte(c byte) bool {
	return alnum(c) || c == '-' || c == '_' || c == '.'
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// tokenAt is the longest name, lowercased, that low holds at i, ending
// where joined says no word goes on; "" for none.
func (x *exporter) tokenAt(low string, i int, joined func(byte) bool) string {
	for _, tok := range x.tokens {
		if strings.HasPrefix(low[i:], tok) && (i+len(tok) == len(low) || !joined(low[i+len(tok)])) {
			return tok
		}
	}
	return ""
}

// nameIn is the first name that text holds, "" for none. Case does not
// matter, and '-', '_' and '.' separate words: "Payroll-team" holds the
// name payroll.
func (x *exporter) nameIn(text string) string {
	low := asciiLower(text)
	for i := 0; i < len(low); i++ {
		if i == 0 || !alnum(low[i-1]) {
			if tok := x.tokenAt(low, i, alnum); tok != "" {
				return tok
			}
		}
	}
	return ""
}

var (
	urlPattern  = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	ip6Pattern  = regexp.MustCompile(`\[?[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}\]?(?::\d+)?`)
	ipPattern   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	hostPattern = regexp.MustCompile(`\b[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+){2,}\b`)
	// hostPortPattern is a host name of two labels or more with a port:
	// "gpu-node.local:8080".
	hostPortPattern = regexp.MustCompile(`\b[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+:[0-9]+\b`)
)

// scrub replaces URLs, IP addresses and host names in a message.
func scrub(s string) string {
	s = urlPattern.ReplaceAllString(s, "URL")
	s = ipPattern.ReplaceAllString(s, "IP")
	s = ip6Pattern.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Count(m, ":") >= 2 && strings.ContainsAny(m, "0123456789abcdefABCDEF") {
			return "IP"
		}
		return m
	})
	s = hostPortPattern.ReplaceAllString(s, "HOST")
	return hostPattern.ReplaceAllString(s, "HOST")
}

// paramsSite is one recorded set of engine parameters and its fingerprints
// before and after the export.
type paramsSite struct {
	n              *node
	field          string
	oldFP, oldArgs string
	newFP, newArgs string
}

func (p *paramsSite) fingerprint(before bool) error {
	var ep capacity.EngineParams
	if err := learnrec.Unmarshal(p.n.bytes(), &ep); err != nil {
		return fmt.Errorf("learnreplay: export: %s: %w", p.field, err)
	}
	if before {
		p.oldFP, p.oldArgs = ep.Fingerprint(), ep.ArgsFingerprint()
	} else {
		p.newFP, p.newArgs = ep.Fingerprint(), ep.ArgsFingerprint()
	}
	return nil
}

// paramsFields are the fields that hold a capacity.EngineParams: the filed
// parameters, and a capacity record's.
var paramsFields = setOf("Params", "EngineParams")

func findParams(n *node, field string, out *[]paramsSite) {
	switch n.kind {
	case nodeList:
		for _, v := range n.vals {
			findParams(v, field, out)
		}
	case nodeObject:
		if paramsFields[field] {
			*out = append(*out, paramsSite{n: n, field: field})
			return
		}
		for i, k := range n.keys {
			f := field
			if fieldNames[k] {
				f = k
			}
			findParams(n.vals[i], f, out)
		}
	}
}

// replaceFingerprints replaces every whole 16-hex-digit token in s: by the
// renamed parameters' fingerprint when it is a recorded one, by the keyed
// hash when not.
func (x *exporter) replaceFingerprints(s string) string {
	if len(s) < 16 {
		return s
	}
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		if (i == 0 || !wordByte(s[i-1])) && i+16 <= len(s) && isHex16(s[i:i+16]) && (i+16 == len(s) || !wordByte(s[i+16])) {
			old := s[i : i+16]
			if nw, ok := x.fingerprints[old]; ok {
				b.WriteString(nw)
				x.mapped++
			} else {
				nw, ok := x.keyed[old]
				if !ok {
					x.mac.Reset()
					_, _ = x.mac.Write([]byte(old))
					nw = hex.EncodeToString(x.mac.Sum(nil))[:16]
					x.keyed[old] = nw
				}
				b.WriteString(nw)
				x.keyedCount++
			}
			i += 16
			changed = true
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	if !changed {
		return s
	}
	return b.String()
}

// atomAllowed reports a string allowed as it is outside a composite key:
// a pseudonym, a number, a time, a fingerprint, vocabulary or a known
// accelerator.
func (x *exporter) atomAllowed(a string) bool {
	return a == "" || x.pseudonyms[a] || isNumber(a) || isTime(a) || isFingerprint(a) || vocabulary[a] || knownAccelerators[a]
}

// compositeAllowed reports a composite key whose every part is allowed for
// its place: a name part a pseudonym (or pseudonyms between '/'), an
// accelerator part a known accelerator or a pseudonym, a count digits, a
// fingerprint a fingerprint.
func (x *exporter) compositeAllowed(s string) bool {
	parts, layout, tag, tagged := splitComposite(s)
	if tagged && !stashTag(tag) {
		return false
	}
	for i, part := range parts {
		var ok bool
		switch layout[i] {
		case partAccelerator:
			ok = knownAccelerators[part] || x.pseudonyms[part]
		case partCount:
			ok = part != "" && strings.Trim(part, "0123456789") == "" && !x.figureIsAName(part, part)
		case partFingerprint:
			ok = isFingerprint(part)
		case partShape:
			ok = shapePart(part)
			if q, isQ := strings.CutPrefix(part, "q"); ok && isQ {
				ok = !x.figureIsAName(part, q)
			}
		default:
			ok = x.pseudonyms[part]
			if !ok {
				ok = true
				for _, a := range strings.Split(part, "/") {
					ok = ok && x.pseudonyms[a]
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// maxFigureDigits is the most digits a count or a queue threshold in a key
// may have and still pass as a figure when it is also a recorded name. The
// figures the controller writes there are small integers -- a replica's GPU
// count, the queue threshold (qN) -- and a name of three digits or fewer
// identifies nothing. A longer number that is a recorded name -- a
// namespace named after a phone number, say -- is the name, and is refused.
const maxFigureDigits = 3

// figureIsAName reports whether part, a count or a queue threshold whose
// number is n, is a recorded name the export would let through as a figure:
// a name of more than maxFigureDigits, or not a whole number.
func (x *exporter) figureIsAName(part, n string) bool {
	if _, named := x.names[part]; !named {
		return false
	}
	return n == "" || len(n) > maxFigureDigits || strings.Trim(n, "0123456789") != ""
}

// shapeBuckets are the length buckets a window key names
// (saturation.classifyOutputLength, classifyInputLength, and prefill's
// own output bucket).
var shapeBuckets = setOf("short", "medium", "long", "xlong", "xxlong", "huge", "noout")

// shapePart reports whether part is a shape part of a window key: a role, a
// length bucket (an input one prefixed i), or a queue threshold (q and a
// number). A stash key's '#' tag is split off before (splitComposite).
func shapePart(part string) bool {
	if q, ok := strings.CutPrefix(part, "q"); ok && isNumber(q) {
		return true
	}
	return vocabulary[part] || shapeBuckets[part] || shapeBuckets[strings.TrimPrefix(part, "i")]
}

// check holds the string at a site, after the export, to the allowlist.
func (x *exporter) check(at site, s *string) {
	if x.compositeSite(at) {
		// A composite key is held part by part: its name parts must hold no
		// name, and its other parts are figures and code words that
		// compositeAllowed holds to their own rules. Read whole, a bucket
		// named like a namespace ("medium") would read as that name.
		parts, layout, _, _ := splitComposite(*s)
		for i, part := range parts {
			if (layout[i] == partName || layout[i] == partAccelerator) && x.holdsName(part) {
				x.problem("%s holds a recorded name after the export", at.where())
				break
			}
		}
	} else if x.holdsName(*s) {
		x.problem("%s holds a recorded name after the export", at.where())
	}
	x.checkRule(at, s)
}

// checkRule holds the string at a site to its rule.
func (x *exporter) checkRule(at site, s *string) {
	v := *s
	if at.isKey {
		r, ok := at.keyRule()
		if !ok {
			if !x.atomAllowed(v) {
				x.problem("%s is not allowed as it is (add the map to keyRules)", at.where())
			}
			return
		}
		if !x.allowed(r, v) {
			x.problem("%s is not allowed after the export", at.where())
		}
		return
	}
	r, ok := at.valueRule()
	if !ok {
		if !isTime(v) && !isNumber(v) && v != "" {
			x.problem("%s is a string field the export does not classify (add it to valueRules)", at.where())
		}
		return
	}
	if !x.allowed(r, v) {
		x.problem("%s holds a value that is not allowed after the export", at.where())
	}
}

// allowed reports whether v, under rule r, may be committed.
func (x *exporter) allowed(r rule, v string) bool {
	if v == "" {
		return true
	}
	keep := func(ok bool) bool {
		if ok {
			x.kept[v] = true
		}
		return ok
	}
	switch r.class {
	case classRename:
		return x.pseudonyms[v] || r.category == "revision" && strings.Trim(v, "0123456789") == "" ||
			r.category == "label" && keep(knownAccelerators[v])
	case classVocab:
		return keep(vocabulary[v])
	case classAccelerator:
		return x.pseudonyms[v] || keep(knownAccelerators[v])
	case classDuration:
		_, err := time.ParseDuration(v)
		return keep(err == nil)
	case classFingerprint:
		return isFingerprint(v)
	case classTypeName:
		return keep(knownConfigTypes[v])
	case classLabelKey, classResource:
		return x.pseudonyms[v] || keep(knownLabels[v] || vocabulary[v])
	case classComposite:
		return x.compositeAllowed(v)
	case classText:
		return x.textAllowed(v)
	case classCommit:
		return keep(commitShape.MatchString(v))
	case classRedact:
		return v == "redacted"
	}
	return false
}

// asciiLower lowercases ASCII letters only, so every byte keeps its place:
// a message is matched lowercased and rewritten at the same offsets.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
