package learnreplay

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/capacity"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

// Export turns a recording into one that can be committed as a fixture
// (docs/proposals/analyzer-learner.md, 2.4, "before commit"): every name a
// cluster gave -- namespace, model, variant, pod, image, revision, forget
// token, the free-text engine arguments, placement label values -- becomes a
// pseudonym, and the result still replays byte for byte at the commit that
// recorded it.
//
// How it stays replayable:
//
//   - A pseudonym is the category and the name's rank among the category's
//     names, sorted ("variant-03"), zero-padded to one width. Names keep
//     their order within a category, so a list an analyzer sorts by name
//     sorts the same way after.
//   - The same name gets the same pseudonym everywhere in the recording,
//     including inside composite keys and messages ("ns|model|variant|...").
//   - A map whose keys were renamed is re-sorted, as the recorder writes a
//     map; a struct keeps its field order.
//   - An engine fingerprint hashes the model path and the image, so every
//     fingerprint of the recorded engine parameters is recomputed from the
//     renamed parameters and replaced. A fingerprint the recording holds with
//     no parameters behind it (one only a restored snapshot names) matched
//     nothing in the run; it is replaced by a keyed hash, so it still
//     matches nothing and does not publish the original digest. The key is
//     per export and never written.
//
// It refuses rather than writes a fixture that could leak: a string field it
// has not classified, or an original name that survives anywhere, is an
// error. The pseudonym table itself is never returned.
func Export(recording []byte, opts ExportOptions) ([]byte, *ExportReport, error) {
	read, err := learnrec.Read(recording)
	if err != nil {
		return nil, nil, err
	}
	if len(read.Events) == 0 || read.Events[0].Kind != learnrec.KindStart {
		return nil, nil, errors.New("learnreplay: export: the recording has no start line")
	}
	x := &exporter{names: map[string]string{}, pseudonyms: map[string]bool{}, byCategory: map[string]map[string]bool{}}
	x.key = opts.Key
	if len(x.key) == 0 {
		x.key = make([]byte, 32)
		if _, err := rand.Read(x.key); err != nil {
			return nil, nil, err
		}
	}
	trees := make([]*node, len(read.Events))
	for i, ev := range read.Events {
		n, err := parseNode(ev.Data)
		if err != nil {
			return nil, nil, fmt.Errorf("learnreplay: export: seq %d: %w", ev.Seq, err)
		}
		trees[i] = n
		if err := x.harvest(n, "", ev.Kind); err != nil {
			return nil, nil, fmt.Errorf("learnreplay: export: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
	}
	if err := x.assign(); err != nil {
		return nil, nil, err
	}
	// The fingerprints of every recorded engine configuration, before.
	var params []paramsSite
	for _, n := range trees {
		x.findParams(n, "", &params)
	}
	for i := range params {
		if err := params[i].fingerprint(true); err != nil {
			return nil, nil, err
		}
	}
	for _, n := range trees {
		x.rename(n, x.replaceNames)
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
	for _, n := range trees {
		x.rename(n, x.replaceFingerprints)
	}

	var out bytes.Buffer
	for i, ev := range read.Events {
		out.WriteString(`{"Seq":`)
		out.WriteString(strconv.FormatUint(ev.Seq, 10))
		out.WriteString(`,"Now":`)
		writeJSONString(&out, ev.Now.UTC().Format(time.RFC3339Nano))
		out.WriteString(`,"Kind":`)
		writeJSONString(&out, ev.Kind)
		out.WriteString(`,"Data":`)
		trees[i].encode(&out)
		out.WriteString("}\n")
	}
	if err := x.leaks(out.Bytes(), read.Events, trees); err != nil {
		return nil, nil, err
	}
	if _, err := learnrec.Read(out.Bytes()); err != nil {
		return nil, nil, fmt.Errorf("learnreplay: export: the export does not read back: %w", err)
	}
	rep := &ExportReport{Events: len(read.Events), Names: map[string]int{}, TornTail: read.TornTail != ""}
	for c, set := range x.byCategory {
		rep.Names[c] = len(set)
	}
	rep.Fingerprints, rep.KeyedFingerprints = x.mapped, x.keyed
	return out.Bytes(), rep, nil
}

// ExportOptions configure Export.
type ExportOptions struct {
	// Key keys the hash that replaces a fingerprint with no recorded
	// parameters behind it. Empty draws a random one, which is what a
	// committed fixture wants: the key is never written.
	Key []byte
}

// ExportReport says what an export renamed, in counts: never the names.
type ExportReport struct {
	// Events is how many events were exported.
	Events int
	// Names is, per category, how many distinct names became pseudonyms.
	Names map[string]int
	// Fingerprints is how many fingerprint occurrences were recomputed from
	// renamed parameters, and KeyedFingerprints how many had none behind
	// them and were replaced by the keyed hash.
	Fingerprints, KeyedFingerprints int
	// TornTail is whether a last line cut off mid-write was dropped.
	TornTail bool
}

// String is the report on one line.
func (r *ExportReport) String() string {
	cats := make([]string, 0, len(r.Names))
	for c := range r.Names {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	parts := make([]string, 0, len(cats))
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s=%d", c, r.Names[c]))
	}
	return fmt.Sprintf("%d events; names renamed: %s; fingerprints recomputed %d, keyed %d; torn tail dropped: %t",
		r.Events, strings.Join(parts, " "), r.Fingerprints, r.KeyedFingerprints, r.TornTail)
}

// The string fields a recording holds, by Go field name (the recorder writes
// a struct's fields under their Go names). A string the export meets under a
// field in none of these tables stops it: classify the field first.
// TestExport_EveryRecordedStringFieldIsClassified holds the tables to every
// type a recorded event can reach.
var (
	// renamedFields are the names a cluster gives, by category: each value
	// becomes a pseudonym. Variant and Donor hold a composite key in some
	// events (with '|'); only a plain value is a name.
	renamedFields = map[string]string{
		"Namespace":           "ns",
		"DonorNamespace":      "ns",
		"ControllerNamespace": "ns",
		"BorrowNamespaces":    "ns",
		"ClusterNamespaces":   "ns",
		"Exclude":             "ns",
		"ModelID":             "model",
		"ModelName":           "model",
		"ModelPath":           "model",
		"VariantName":         "variant",
		"Variant":             "variant",
		"Variants":            "variant",
		"Donor":               "variant",
		"PodName":             "pod",
		"UnplacedPods":        "pod",
		"AffectedPods":        "pod",
		"Image":               "image",
		"Revision":            "revision",
		"ModelRevision":       "revision",
		"Token":               "token",
		"SpeculativeConfig":   "arg",
		"SharingUnresolved":   "arg",
		"NodeSelector":        "label",
		"Values":              "label",
		"Value":               "label",
	}
	// keptFields are vocabulary: values the code defines (roles, reasons,
	// sources, dtypes, durations), the accelerator product names, analyzer
	// and policy-tier names. Kept as they are.
	keptFields = setOf("Accelerator", "AcceleratorName", "Answer", "AnalyzerName", "BorrowCooldown", "ConfigType",
		"DefaultPolicy", "Effect", "Engine", "Excluded", "FlappingWindow", "FreshnessStatus", "K2Source", "KVConnector",
		"KVOffloadingBackend", "Key", "KvCacheDtype", "LearnedFrom", "LenderDebounce", "MemoryLayout", "Name",
		"Objective", "Operator", "PlacementTimeout", "PriorityClass", "Quantity", "Quantization", "Resources", "RateSource", "Reason",
		"RefreshInterval", "RetentionPeriod", "Role", "Scope", "Source", "Statistic", "Threshold", "TopologyKey", "Type",
		"Unresolved", "WaitBeforeReverseMove", "WeightClass", "WeightDtype")
	// textFields are messages and composite keys built from names: every
	// name inside them is replaced, and the leak check holds them to it.
	textFields = setOf("Args", "Config", "DecodeErr", "Err", "Error", "From", "FromVariant", "Issues", "K2Args",
		"PhysicsKey", "Query", "ThroughputArgs")
)

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// exporter is one export's state.
type exporter struct {
	// byCategory is each category's names; names is every name's pseudonym.
	byCategory map[string]map[string]bool
	names      map[string]string
	// pseudonyms is every pseudonym given.
	pseudonyms map[string]bool
	// tokens is names' keys, longest first, so a pod name is replaced
	// before the variant name it starts with.
	tokens []string
	// accelerators are the values of the accelerator fields: a placement
	// label with one of these values names the hardware and is kept.
	accelerators map[string]bool
	fingerprints map[string]string
	key          []byte
	// mapped and keyed count the fingerprints replaced.
	mapped, keyed int
}

// isFieldName reports whether an object key is a Go field name, so the
// object is a struct; any other key is a map's.
func isFieldName(k string) bool {
	if k == "" || k[0] < 'A' || k[0] > 'Z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if !alnum(c) {
			return false
		}
	}
	return true
}

// harvest collects the names under renamed fields and refuses a string under
// a field no table classifies. field is the Go field the value sits under: a
// map's values inherit the map's field.
func (x *exporter) harvest(n *node, field, kind string) error {
	switch n.kind {
	case nodeString:
		return x.harvestString(n.text, field, kind)
	case nodeList:
		for _, v := range n.vals {
			if err := x.harvest(v, field, kind); err != nil {
				return err
			}
		}
	case nodeObject:
		for i, k := range n.keys {
			f := field
			if isFieldName(k) {
				f = k
			}
			if err := x.harvest(n.vals[i], f, kind); err != nil {
				return err
			}
		}
	}
	return nil
}

func (x *exporter) harvestString(s, field, kind string) error {
	if s == "" || isTimeOrSpecial(s) {
		return nil
	}
	if keptFields[field] {
		if field == "Accelerator" || field == "AcceleratorName" || field == "Answer" {
			if x.accelerators == nil {
				x.accelerators = map[string]bool{}
			}
			x.accelerators[s] = true
		}
		return nil
	}
	if textFields[field] {
		return nil
	}
	cat, ok := renamedFields[field]
	if !ok {
		if field == "" {
			field = "(top level)"
		}
		return fmt.Errorf("the string field %s of %s is not classified: add it to renamedFields, keptFields or textFields", field, kind)
	}
	if (field == "Variant" || field == "Donor") && strings.Contains(s, "|") {
		return nil
	}
	// Digits alone name nothing (a revision number), and renaming them would
	// rename every count that reads the same inside a composite key.
	if strings.Trim(s, "0123456789") == "" {
		return nil
	}
	if x.byCategory[cat] == nil {
		x.byCategory[cat] = map[string]bool{}
	}
	x.byCategory[cat][s] = true
	return nil
}

// isTimeOrSpecial reports a string that is a recorded time or a float the
// recorder writes as a string.
func isTimeOrSpecial(s string) bool {
	switch s {
	case "NaN", "+Inf", "-Inf":
		return true
	}
	if len(s) >= 20 && s[4] == '-' && s[7] == '-' && s[10] == 'T' {
		_, err := time.Parse(time.RFC3339Nano, s)
		return err == nil
	}
	return false
}

// categoryOrder settles a name found under two categories: the first wins.
var categoryOrder = []string{"ns", "model", "variant", "pod", "image", "revision", "token", "arg", "label"}

// assign gives every name its pseudonym.
func (x *exporter) assign() error {
	// A label value that names an accelerator is hardware, not a name.
	if set := x.byCategory["label"]; set != nil {
		for s := range set {
			if x.accelerators[s] {
				delete(set, s)
			}
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
		sort.Strings(names)
		width := max(2, len(strconv.Itoa(len(names))))
		for i, s := range names {
			x.names[s] = fmt.Sprintf("%s-%0*d", cat, width, i+1)
		}
	}
	for s, p := range x.names {
		if _, clash := x.names[p]; clash {
			return fmt.Errorf("learnreplay: export: the recording already holds a name shaped like a pseudonym (%s); it cannot be told apart after the export", p)
		}
		x.tokens = append(x.tokens, s)
		x.pseudonyms[p] = true
	}
	sort.Slice(x.tokens, func(i, j int) bool {
		if len(x.tokens[i]) != len(x.tokens[j]) {
			return len(x.tokens[i]) > len(x.tokens[j])
		}
		return x.tokens[i] < x.tokens[j]
	})
	return nil
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

func (x *exporter) findParams(n *node, field string, out *[]paramsSite) {
	switch n.kind {
	case nodeList:
		for _, v := range n.vals {
			x.findParams(v, field, out)
		}
	case nodeObject:
		if paramsFields[field] {
			*out = append(*out, paramsSite{n: n, field: field})
			return
		}
		for i, k := range n.keys {
			f := field
			if isFieldName(k) {
				f = k
			}
			x.findParams(n.vals[i], f, out)
		}
	}
}

// rename applies replace to every string and map key under n, and re-sorts
// a map whose keys it changed.
func (x *exporter) rename(n *node, replace func(string) string) {
	switch n.kind {
	case nodeString:
		n.text = replace(n.text)
	case nodeList:
		for _, v := range n.vals {
			x.rename(v, replace)
		}
	case nodeObject:
		changed := false
		for i, k := range n.keys {
			if !isFieldName(k) {
				if nk := replace(k); nk != k {
					n.keys[i] = nk
					changed = true
				}
			}
			x.rename(n.vals[i], replace)
		}
		if changed {
			n.sortKeys()
		}
	}
}

// wordByte is a byte a name runs on through: a name matches only where the
// bytes either side are not one of these, so "rec" is not found in "record".
func wordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

// replaceNames replaces every whole name in s with its pseudonym.
func (x *exporter) replaceNames(s string) string {
	if p, ok := x.names[s]; ok {
		return p
	}
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		if i == 0 || !wordByte(s[i-1]) {
			if tok := x.tokenAt(s, i); tok != "" {
				b.WriteString(x.names[tok])
				i += len(tok)
				changed = true
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	if !changed {
		return s
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

// replaceFingerprints replaces every whole 16-hex-digit token in s: by the
// renamed parameters' fingerprint when it is a recorded one, by the keyed
// hash when not.
func (x *exporter) replaceFingerprints(s string) string {
	var b strings.Builder
	changed := false
	for i := 0; i < len(s); {
		if (i == 0 || !wordByte(s[i-1])) && i+16 <= len(s) && isHex16(s[i:i+16]) && (i+16 == len(s) || !wordByte(s[i+16])) {
			old := s[i : i+16]
			if nw, ok := x.fingerprints[old]; ok {
				b.WriteString(nw)
				x.mapped++
			} else {
				m := hmac.New(sha256.New, x.key)
				m.Write([]byte(old))
				b.WriteString(hex.EncodeToString(m.Sum(nil))[:16])
				x.keyed++
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

func isHex16(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// leaks fails when any original name survives in the export, wherever a
// word could start or end: the check is looser than the replacement (only
// letters and digits join a name to its neighbours), so a name the
// replacement passed over inside a longer word ("rec" in "svc.rec.local")
// is still found.
//
// The error names each surviving name by its pseudonym and where it was
// found (the event kind and the field), never by the name itself.
func (x *exporter) leaks(out []byte, events []learnrec.RawEvent, trees []*node) error {
	var found []string
	for _, tok := range x.tokens {
		if x.containsBounded(out, tok) {
			found = append(found, tok)
		}
	}
	if len(found) == 0 {
		return nil
	}
	where := map[string]bool{}
	for i, n := range trees {
		x.locate(n, "", events[i].Kind, found, where)
	}
	var msgs []string
	for _, tok := range found {
		msgs = append(msgs, x.names[tok])
	}
	sort.Strings(msgs)
	places := make([]string, 0, len(where))
	for w := range where {
		places = append(places, w)
	}
	sort.Strings(places)
	return fmt.Errorf("learnreplay: export: the names behind %s survive in the export (in %s); it was not written",
		strings.Join(msgs, ", "), strings.Join(places, "; "))
}

// locate notes, for each token in found that a string or map key under n
// holds, the event kind and field it sits in.
func (x *exporter) locate(n *node, field, kind string, found []string, where map[string]bool) {
	check := func(s, what string) {
		for _, tok := range found {
			if x.containsBounded([]byte(s), tok) {
				where[fmt.Sprintf("%s of %s: %s", what, kind, x.names[tok])] = true
			}
		}
	}
	switch n.kind {
	case nodeString:
		check(n.text, field)
	case nodeList:
		for _, v := range n.vals {
			x.locate(v, field, kind, found, where)
		}
	case nodeObject:
		for i, k := range n.keys {
			f := field
			if isFieldName(k) {
				f = k
			} else {
				check(k, "a key of "+field)
			}
			x.locate(n.vals[i], f, kind, found, where)
		}
	}
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// containsBounded reports whether tok occurs in b with no letter or digit
// directly before or after it, other than as the category word a pseudonym
// starts with (a namespace named "ns" is not found in "ns-01").
func (x *exporter) containsBounded(b []byte, tok string) bool {
	t := []byte(tok)
	for off := 0; ; {
		i := bytes.Index(b[off:], t)
		if i < 0 {
			return false
		}
		i += off
		end := i + len(t)
		if (i == 0 || !alnum(b[i-1])) && (end == len(b) || !alnum(b[end])) && !x.pseudonymAt(b, i) {
			return true
		}
		off = i + 1
	}
}

// pseudonymAt reports whether a pseudonym starts at b[i]: a category, a
// dash, digits, and then no letter or digit.
func (x *exporter) pseudonymAt(b []byte, i int) bool {
	j := i
	for j < len(b) && b[j] >= 'a' && b[j] <= 'z' {
		j++
	}
	if j == i || j >= len(b) || b[j] != '-' {
		return false
	}
	k := j + 1
	for k < len(b) && b[k] >= '0' && b[k] <= '9' {
		k++
	}
	if k == j+1 || k < len(b) && alnum(b[k]) {
		return false
	}
	return x.pseudonyms[string(b[i:k])]
}
