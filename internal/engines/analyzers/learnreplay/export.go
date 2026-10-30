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
		names:        map[string]string{},
		pseudonyms:   map[string]bool{},
		byCategory:   map[string]map[string]bool{},
		accelerators: map[string]bool{},
		kept:         map[string]bool{},
		keyed:        map[string]string{},
		mac:          hmac.New(sha256.New, key),
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
	for _, t := range opts.Text {
		if tok := x.nameIn(t); tok != "" {
			return nil, nil, fmt.Errorf("learnreplay: export: the text to commit beside the export holds a name (the one behind %s)", x.names[tok])
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
	// Text is text that will be committed beside the export (the manifest's
	// source): the export refuses when it holds a name.
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
		if isStruct(n) {
			for i, k := range n.keys {
				child := site{kind: at.kind, field: k, parent: at.field}
				x.walkAt(n.vals[i], child, 0, fn)
			}
			return
		}
		keyAt := at
		keyAt.isKey = true
		keyAt.key = at.field + strings.Repeat("{}", depth)
		if at.field == "" {
			keyAt.key = at.kind
		}
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
	// tokens is names' keys that are not digits alone, longest first, for
	// renaming inside a message: a pod name is replaced before the variant
	// name it starts with.
	tokens []string
	// accelerators are the values of the accelerator fields, allowed
	// wherever they stand.
	accelerators map[string]bool
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
		x.accelerators[s] = true
	case classLabelKey, classResource:
		if !wellKnownLabel(s) && !vocabulary[s] {
			x.add(r.category, s)
		}
	}
}

// harvestComposites names each part of a composite key that is not a name
// already, nor allowed as it is: a name only a key holds (a variant of
// another namespace in a restored snapshot) is still a name.
func (x *exporter) harvestComposites(composites []string) {
	known := map[string]bool{}
	for _, set := range x.byCategory {
		for s := range set {
			known[s] = true
		}
	}
	allowed := func(a string) bool {
		return a == "" || known[a] || isNumber(a) || isTime(a) || isHex16(a) || vocabulary[a] || x.accelerators[a] ||
			strings.HasPrefix(a, capacity.IncompleteArgsPrefix) && isHex16(strings.TrimPrefix(a, capacity.IncompleteArgsPrefix))
	}
	for _, c := range composites {
		for _, part := range strings.Split(c, "|") {
			if allowed(part) {
				continue
			}
			for _, a := range strings.Split(part, "/") {
				if !allowed(a) {
					x.add("name", a)
					known[a] = true
				}
			}
		}
	}
}

// categoryOrder settles a name found under two categories: the first wins.
var categoryOrder = []string{"ns", "model", "variant", "pod", "image", "revision", "token", "arg", "label",
	"labelkey", "resource", "priority", "weightclass", "limiter", "name"}

// assign gives every name its pseudonym.
func (x *exporter) assign() error {
	// A label value that names an accelerator is hardware, not a name.
	for s := range x.byCategory["label"] {
		if x.accelerators[s] {
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
		if strings.Trim(s, "0123456789") != "" {
			x.tokens = append(x.tokens, s)
		}
	}
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
	case classRename, classLabelKey, classResource:
		if p, ok := x.names[s]; ok {
			return p
		}
	case classComposite:
		return x.renameComposite(s)
	case classText:
		return scrub(x.renameText(s))
	case classRedact:
		return "redacted"
	}
	return s
}

// renameComposite renames each part of a key joined by '|', and each part
// of a part joined by '/' when the whole part is not a name (a model ID has
// a '/' of its own).
func (x *exporter) renameComposite(s string) string {
	parts := strings.Split(s, "|")
	for i, part := range parts {
		if p, ok := x.names[part]; ok {
			parts[i] = p
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
	return strings.Join(parts, "|")
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

// renameText replaces every whole name in a message with its pseudonym.
func (x *exporter) renameText(s string) string {
	if p, ok := x.names[s]; ok {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if i == 0 || !wordByte(s[i-1]) {
			if tok := x.tokenAt(s, i); tok != "" {
				b.WriteString(x.names[tok])
				i += len(tok)
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func (x *exporter) tokenAt(s string, i int) string {
	for _, tok := range x.tokens {
		if strings.HasPrefix(s[i:], tok) && (i+len(tok) == len(s) || !wordByte(s[i+len(tok)])) {
			return tok
		}
	}
	return ""
}

// nameIn is the first name that text holds as a whole word, "" for none.
func (x *exporter) nameIn(text string) string {
	for i := 0; i < len(text); i++ {
		if i == 0 || !wordByte(text[i-1]) {
			if tok := x.tokenAt(text, i); tok != "" {
				return tok
			}
		}
	}
	return ""
}

var (
	urlPattern  = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	ipPattern   = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	hostPattern = regexp.MustCompile(`\b[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+){2,}\b`)
	wordSplit   = regexp.MustCompile(`[\s,;()\[\]{}"'=<>]+`)
	alphaWord   = regexp.MustCompile(`^[A-Za-z]+$`)
)

// scrub replaces URLs, IP addresses and host names in a message.
func scrub(s string) string {
	s = urlPattern.ReplaceAllString(s, "URL")
	s = ipPattern.ReplaceAllString(s, "IP")
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

// atomAllowed reports a string allowed as it is, wherever it stands.
func (x *exporter) atomAllowed(a string) bool {
	return a == "" || x.pseudonyms[a] || isNumber(a) || isTime(a) || isHex16(a) || vocabulary[a] || x.accelerators[a] ||
		strings.HasPrefix(a, capacity.IncompleteArgsPrefix) && isHex16(strings.TrimPrefix(a, capacity.IncompleteArgsPrefix))
}

// compositeAllowed reports a composite key whose every part is allowed.
func (x *exporter) compositeAllowed(s string) bool {
	for _, part := range strings.Split(s, "|") {
		if x.atomAllowed(part) {
			continue
		}
		for _, a := range strings.Split(part, "/") {
			if !x.atomAllowed(a) {
				return false
			}
		}
	}
	return true
}

// textAllowed reports a message whose every word is a plain word or allowed.
func (x *exporter) textAllowed(s string) bool {
	for _, w := range wordSplit.Split(s, -1) {
		w = strings.TrimRight(w, ".:")
		if w == "" || alphaWord.MatchString(w) || x.compositeAllowed(w) {
			continue
		}
		if _, err := time.ParseDuration(w); err == nil {
			continue
		}
		return false
	}
	return true
}

// check holds the string at a site, after the export, to the allowlist.
func (x *exporter) check(at site, s *string) {
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
	switch r.class {
	case classRename:
		return x.pseudonyms[v] || r.category == "revision" && strings.Trim(v, "0123456789") == "" ||
			r.category == "label" && x.accelerators[v]
	case classVocab:
		x.kept[v] = vocabulary[v]
		return vocabulary[v]
	case classAccelerator:
		x.kept[v] = true
		return acceleratorShape.MatchString(v)
	case classDuration:
		_, err := time.ParseDuration(v)
		x.kept[v] = err == nil
		return err == nil
	case classFlag:
		x.kept[v] = true
		return flagShape.MatchString(v)
	case classTypeName:
		x.kept[v] = true
		return typeNameShape.MatchString(v)
	case classLabelKey, classResource:
		if wellKnownLabel(v) || vocabulary[v] {
			x.kept[v] = true
			return true
		}
		return x.pseudonyms[v]
	case classComposite:
		return x.compositeAllowed(v)
	case classText:
		return x.textAllowed(v)
	case classCommit:
		x.kept[v] = true
		return commitShape.MatchString(v)
	case classRedact:
		return v == "redacted"
	}
	return false
}
