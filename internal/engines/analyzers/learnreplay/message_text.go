package learnreplay

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// A message is checked whole token by whole token. A token is a run of
// ASCII letters, digits and - _ . / : |; anything else separates tokens.
// A run of letters inside a token is never judged alone: "tenants-production"
// is one token, and it is either allowed whole or refused.

// tokenPattern finds the tokens of a message; tokenSplit is what separates
// them. A byte of a non-ASCII character separates too, and the message
// check refuses any message that holds one.
var (
	tokenPattern = regexp.MustCompile(`[A-Za-z0-9_.:/|-]+`)
	tokenSplit   = regexp.MustCompile(`[^A-Za-z0-9_.:/|-]+`)
	// fmtVerb is a fmt verb, removed from a format string before its tokens
	// are read: "variant %s has no line" gives variant, has, no, line.
	fmtVerb = regexp.MustCompile(`%[-+# 0-9.*\[\]]*[a-zA-Z%]`)
	// pseudonymWithSuffix is a pseudonym and what may follow it inside a
	// token, as a pod name follows its owner's: segments of randomSegment.
	pseudonymWithSuffix = regexp.MustCompile(`^([a-z]+-[0-9]+)((?:-[0-9a-z]+)*)$`)
	// randomSegment is what a controller appends to a pod's name: a
	// StatefulSet or LeaderWorkerSet ordinal (digits), the five random
	// characters of a generated name, or a ReplicaSet's pod-template hash
	// (a uint32 written in decimal, each digit mapped into the same
	// alphabet, so up to ten). The alphabet is k8s.io/apimachinery's
	// util/rand: no vowels, no 0, 1 or 3.
	randomSegment = regexp.MustCompile(`^([0-9]+|[bcdfghjklmnpqrstvwxz2456789]{5}|[bcdfghjklmnpqrstvwxz2456789]{8,10})$`)
	// scenarioID is a short identifier a person writes in a source: E1.
	scenarioID = regexp.MustCompile(`^[A-Za-z][0-9]{1,3}$`)
)

// tokens is a message's tokens, each with trailing sentence punctuation
// (".", ":") trimmed.
func tokens(s string) []string {
	var out []string
	for _, t := range tokenSplit.Split(s, -1) {
		if t = strings.TrimRight(t, ".:"); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// renameText renames every name in a message, token by token (renameToken),
// and leaves every other byte as it was.
func (x *exporter) renameText(s string) string {
	return tokenPattern.ReplaceAllStringFunc(s, x.renameToken)
}

// renameToken renames the names in one token, whatever their case. The
// token is split at its separators (- _ . / : |), a leading or trailing one
// included, and every run of segments that is a recorded name, longest
// first, is renamed wherever it stands: "llm-inference-7f9c4-dbrx" holds the
// names llm-inference and dbrx, ":demand" the name demand. A segment that
// is a name glued to digits at either end ("payroll7", "2payroll") has its
// name part renamed. Separators and every other segment stay as they were;
// the message check then judges what is left.
func (x *exporter) renameToken(t string) string {
	if t == "" {
		return t
	}
	if p, ok := x.lower[asciiLower(t)]; ok {
		return p
	}
	var segs, seps []string // seps[i] follows segs[i]
	start := 0
	for i := 0; i < len(t); i++ {
		if strings.IndexByte("-_./:|", t[i]) >= 0 {
			segs = append(segs, t[start:i])
			seps = append(seps, t[i:i+1])
			start = i + 1
		}
	}
	segs = append(segs, t[start:])
	var b strings.Builder
	for i := 0; i < len(segs); {
		matched := false
		for j := len(segs) - 1; j >= i && !matched; j-- {
			var w strings.Builder
			for k := i; k <= j; k++ {
				w.WriteString(segs[k])
				if k < j {
					w.WriteString(seps[k])
				}
			}
			if w.Len() == 0 {
				continue
			}
			if p, ok := x.lower[asciiLower(w.String())]; ok {
				b.WriteString(p)
				if j < len(seps) {
					b.WriteString(seps[j])
				}
				i, matched = j+1, true
			}
		}
		if matched {
			continue
		}
		b.WriteString(x.renameGlued(segs[i]))
		if i < len(seps) {
			b.WriteString(seps[i])
		}
		i++
	}
	return b.String()
}

// renameGlued renames the name in a segment glued to digits at either end:
// "payroll7" becomes "ns-017", which the message check then refuses.
func (x *exporter) renameGlued(s string) string {
	core := strings.TrimRight(strings.TrimLeft(s, "0123456789"), "0123456789")
	if core == "" || core == s {
		return s
	}
	p, ok := x.lower[asciiLower(core)]
	if !ok {
		return s
	}
	i := strings.Index(s, core)
	return s[:i] + p + s[i+len(core):]
}

// textAllowed reports a message every token of which, after renaming, is
// allowed whole (tokenAllowed). A message with a non-ASCII character is
// refused: a name in another script cannot be told from a word.
func (x *exporter) textAllowed(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	for _, t := range tokens(s) {
		if !x.tokenAllowed(t) {
			return false
		}
	}
	return true
}

// tokenAllowed reports a token that is, whole: a word of the code's own
// messages or vocabulary; HOST, IP or URL; a number, time, duration or
// fingerprint; a composite key whose every part is allowed (no fallback);
// a pseudonym, alone or with a pod-like suffix; or pseudonyms joined by '/'.
func (x *exporter) tokenAllowed(t string) bool {
	switch {
	case messageWords[t], vocabulary[t], knownLabels[t], knownAccelerators[t]:
		return true
	case t == "HOST", t == "IP", t == "URL":
		return true
	case isNumber(t), isTime(t), isFingerprint(t):
		return true
	case strings.Contains(t, "|"):
		return x.compositeAllowed(t)
	case x.pseudonymToken(t):
		return true
	case strings.Contains(t, "/"):
		for _, p := range strings.Split(t, "/") {
			if !x.tokenAllowed(p) {
				return false
			}
		}
		return true
	}
	_, err := time.ParseDuration(t)
	return err == nil
}

// pseudonymToken reports a pseudonym, alone or followed by segments a pod
// name adds: digits, hex, or a Kubernetes random suffix.
func (x *exporter) pseudonymToken(t string) bool {
	m := pseudonymWithSuffix.FindStringSubmatch(t)
	if m == nil || !x.pseudonyms[m[1]] {
		return false
	}
	for _, seg := range strings.Split(m[2], "-") {
		if seg != "" && !randomSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// sourceRefusals says which tokens of a text a person wrote to commit
// beside a fixture (the manifest's source) are not ordinary words -- a word
// of the code's messages or the committed source words (sourceWords), in
// any case; a number; a scenario name such as E1 -- by position, never by
// the word: a word that is not allowed may be a name.
func sourceRefusals(text string) []string {
	var out []string
	if !isASCII(text) {
		return []string{"a non-ASCII character"}
	}
	for i, t := range tokens(text) {
		low := asciiLower(t)
		if messageWords[t] || messageWords[low] || sourceWords[low] || isNumber(t) || scenarioID.MatchString(t) {
			continue
		}
		out = append(out, fmt.Sprintf("word %d", i+1))
	}
	return out
}

// checkFixtureName holds a fixture's name to what a source may hold: no
// recorded name in it, whole or inside a part ("scale-test-e1",
// "2payroll"); each part between dashes a number, a scenario name (e1) or
// an ordinary word. The error says where, never what.
func (x *exporter) checkFixtureName(name string) error {
	if tok := x.nameIn(name); tok != "" || x.holdsName(name) {
		return errors.New("learnreplay: export: the fixture name holds a recorded name")
	}
	for i, part := range strings.Split(name, "-") {
		low := asciiLower(part)
		for _, tok := range x.tokens {
			if strings.Contains(low, tok) {
				return fmt.Errorf("learnreplay: export: the fixture name's part %d holds a recorded name", i+1)
			}
		}
		if !isNumber(part) && !scenarioID.MatchString(part) && !messageWords[low] && !sourceWords[low] {
			return fmt.Errorf("learnreplay: export: the fixture name's part %d is not a number, a scenario name such as e1, or an ordinary word", i+1)
		}
	}
	return nil
}

// minBackstopName is the shortest recorded name the backstop looks for: a
// shorter one is too likely to be part of an ordinary word, and stays on
// the token rules.
const minBackstopName = 4

// holdsName is the backstop behind every token rule: whether s, lowercased,
// holds a recorded name of minBackstopName characters or more anywhere, as
// plain text -- glued to letters ("payroll-kpmgx"), split by a character
// no rule splits at ("scale@demand"), or overlapping another name. One
// occurrence is let through: one that is a whole token of s, equal to the
// name, that is also the code's own word -- a role "decode" in a recording
// whose namespace is decode is the code's word, not the name. A code word
// that merely contains a name is not let through.
func (x *exporter) holdsName(s string) bool {
	low := asciiLower(s)
	for _, n := range x.longNames {
		for off := 0; ; {
			i := strings.Index(low[off:], n)
			if i < 0 {
				break
			}
			i += off
			end := i + len(n)
			whole := (i == 0 || !tokenByte(low[i-1])) && (end == len(low) || !tokenByte(low[end]))
			if !whole || !isCodeToken(s[i:end]) {
				return true
			}
			off = i + 1
		}
	}
	return false
}

// tokenByte is a byte a message token holds (tokenPattern).
func tokenByte(c byte) bool {
	return alnum(c) || strings.IndexByte("_.:/|-", c) >= 0
}

// isCodeToken reports a string that is, whole, a word of the code's own.
func isCodeToken(t string) bool {
	return messageWords[t] || vocabulary[t] || knownLabels[t] || knownAccelerators[t] || knownConfigTypes[t]
}

// namesOverlap reports a message in which two recorded names of
// minBackstopName characters or more overlap, neither inside the other
// ("payroll-team-x7k2p" holding payroll-team and team-x7k2p): renaming one
// leaves a piece of the other, so the message is refused.
func (x *exporter) namesOverlap(s string) bool {
	low := asciiLower(s)
	type span struct{ from, to int }
	var spans []span
	for _, n := range x.longNames {
		for off := 0; ; {
			i := strings.Index(low[off:], n)
			if i < 0 {
				break
			}
			spans = append(spans, span{off + i, off + i + len(n)})
			off += i + 1
		}
	}
	for _, a := range spans {
		for _, b := range spans {
			if a.from < b.from && b.from < a.to && a.to < b.to {
				return true
			}
		}
	}
	return false
}
