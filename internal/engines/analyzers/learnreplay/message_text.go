package learnreplay

import (
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
	// pseudonymSuffix is what may follow a pseudonym inside a token, as a
	// pod name follows its variant's: "-" and digits, hex, or the five to
	// ten characters of Kubernetes' random names (no vowels).
	pseudonymWithSuffix = regexp.MustCompile(`^([a-z]+-[0-9]+)((?:-[0-9a-z]+)*)$`)
	randomSegment       = regexp.MustCompile(`^([0-9]+|[0-9a-f]+|[bcdfghjklmnpqrstvwxz2456789]{4,10})$`)
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

// renameToken renames the names in one token, whatever their case: the
// whole token when it is a name; each part of a composite key; and a name
// the token starts with, when a separator or a digit follows it
// ("llm-inference-7f9c4" is the pod of the name llm-inference, and becomes
// "ns-01-7f9c4"). What follows a separator is renamed the same way.
func (x *exporter) renameToken(t string) string {
	if t == "" {
		return t
	}
	low := asciiLower(t)
	if p, ok := x.lower[low]; ok {
		return p
	}
	if strings.Contains(t, "|") {
		parts := strings.Split(t, "|")
		for i, p := range parts {
			parts[i] = x.renameToken(p)
		}
		return strings.Join(parts, "|")
	}
	for _, tok := range x.tokens {
		if len(tok) >= len(low) || !strings.HasPrefix(low, tok) {
			continue
		}
		next := low[len(tok)]
		switch {
		case strings.IndexByte("-_./:", next) >= 0:
			return x.lower[tok] + string(next) + x.renameToken(t[len(tok)+1:])
		case next >= '0' && next <= '9':
			return x.lower[tok] + t[len(tok):]
		}
	}
	if i := strings.IndexAny(t, "/:"); i > 0 {
		return x.renameToken(t[:i]) + t[i:i+1] + x.renameToken(t[i+1:])
	}
	return t
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

// sourceRefusals are the tokens of a text a person wrote to commit beside a
// fixture (the manifest's source) that are not ordinary words: a word of
// the code's messages or the committed source words (sourceWords), in any
// case; a number; a scenario name such as E1.
func sourceRefusals(text string) []string {
	var out []string
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return []string{"a non-ASCII character"}
		}
	}
	for _, t := range tokens(text) {
		low := asciiLower(t)
		if messageWords[t] || messageWords[low] || sourceWords[low] || isNumber(t) || scenarioID.MatchString(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}
