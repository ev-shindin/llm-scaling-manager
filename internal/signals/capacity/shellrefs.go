package capacity

import (
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Getting a flag's VALUE out of a pod template, before anything interprets it
// as a setting.
//
// Two jobs, both of which the parsers need and neither of which is about what
// a flag MEANS:
//
//  1. tokenising. An engine is usually launched through `/bin/sh -c "..."`, so
//     the flags are one string and not an argv (collectArgs, splitShellString).
//  2. resolving references. llm-d puts the values in the container env and
//     writes `--block-size $VLLM_BLOCK_SIZE` on the command line, in any of
//     the three forms that reach a pod template (resolveRefs, varNameAt,
//     startsDelimitedRef, containsVarRef, envValues).
//
// Split out of deployment_parser.go, which had reached 939 lines and 25
// top-level declarations holding a tokeniser, a reference resolver, a flag
// table and the EngineParams type. Nothing changed in the move: each
// declaration is the same text in a different file, so the two halves diff
// against the original.
//
// NO SHELL SEMANTICS BEYOND THAT. This is deliberately not a shell: no
// operators, no quoting rules past what splitShellString documents, no
// defaults. `${VAR:-default}` was supported once and fabricated values three
// ways; see the note in resolveRefs.

// envValues is one container's literal environment, as a name -> value map for
// resolving variable references in its own args.
//
// Entries using valueFrom are deliberately ABSENT rather than recorded as
// empty: their value lives in a ConfigMap, Secret, field or resource reference
// that this package has no client to read, so a reference to one comes out of
// the resolver unresolved rather than as a guess. envFrom is invisible here
// for the same reason.
//
// IT IS DEFENCE IN DEPTH, NOT AN OBSERVABLE BEHAVIOUR TODAY, and a review
// proved that by reinstating the bug: recording a valueFrom entry as its empty
// .Value left every test green. The reason is that an empty substitution fails
// downstream anyway -- strconv rejects "" for the numeric keys and usableWord
// rejects it for the string ones -- so the key is recorded either way. Two
// tests claimed to pin this exclusion and neither could; what they actually
// prove is that the key lands in Unresolved, which is true with or without the
// exclusion. The exclusion stays because it is correct at the point it is
// written, and the first mapped field that tolerates an empty value would make
// it load-bearing with no warning.
func envValues(container *corev1.Container) map[string]string {
	env := make(map[string]string, len(container.Env))
	for _, e := range container.Env {
		if e.ValueFrom != nil {
			continue
		}
		env[e.Name] = e.Value
	}
	return env
}

// resolveRefs substitutes variable references in an argument value against the
// container's own environment, returning the resolved string and whether every
// reference in it could be resolved.
//
// Three forms, because two different things do the expanding and a manifest
// may use either:
//
//   - $(VAR) is Kubernetes' own syntax. The kubelet expands it in command and
//     args from the container's env before the process starts, so the string
//     in the manifest is not what the engine receives.
//   - $VAR and ${VAR} are the shell's. They survive into the manifest whenever
//     the command is `/bin/sh -c "... --block-size $VLLM_BLOCK_SIZE ..."`,
//     which is the shape this parser actually meets in the field, and the
//     shell expands them from the same env.
//
// $$ is a literal dollar: Kubernetes' escape for a reference it should not
// expand, and the sequence a manifest uses when it wants the shell to see a
// single $. It is emitted as one "$" and consumes no name.
//
// NO OPERATOR IS INTERPRETED. `${VAR:-default}`, `${VAR%%suffix}`,
// `${VAR:+x}` and the rest all resolve only if a variable of that whole body
// happens to exist, which it will not -- so they come out unresolved. That is
// deliberate and was once otherwise; see the note at the lookup below for the
// three ways honouring `:-` fabricated values and reported them verified.
//
// A reference to a name the container does not define is NOT substituted with
// an empty string. The whole point is to be able to say "unknown"; silently
// reading an unset variable as zero-length is how `--block-size` became 16.
func resolveRefs(s string, env map[string]string) (string, bool) {
	if !strings.Contains(s, "$") {
		return s, true
	}
	var out strings.Builder
	resolved := true
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			out.WriteByte(s[i])
			continue
		}
		// "$$" -> a literal dollar, consuming both bytes.
		if i+1 < len(s) && s[i+1] == '$' {
			out.WriteByte('$')
			i++
			continue
		}
		name, next, ok := varNameAt(s, i)
		if !ok {
			// FOUR DIFFERENT FAILURES REACH HERE, and they do not mean the
			// same thing.
			//
			// A trailing or isolated "$", and a "$" before a byte that cannot
			// start a name, are not references at all. The shell would leave
			// them alone and so do we: keep the byte, and do NOT call the
			// value unresolved on their account.
			//
			// But "${FOO" with no closing brace, and "${}" with no name, DID
			// start a reference and could not finish it. Those used to take
			// the same path, which reported the whole value as resolved --
			// so `--dtype=${FOO` set WeightDtype to the literal "${FOO" with
			// Unresolved empty and Complete() true, and that garbage was
			// hashed into a confident fingerprint. Measured, not argued: a
			// probe printed `dtype="${FOO" unresolved=[] complete=true`.
			// Numeric flags were saved only by ParseInt failing afterwards;
			// the string flags (--dtype, --quantization, --kv-cache-dtype)
			// have nothing but usableWord, which accepts any non-empty
			// string. That is the same false-equality this field exists to
			// prevent, reached by a malformed reference instead of an
			// unreadable one.
			if startsDelimitedRef(s, i) {
				resolved = false
			}
			out.WriteByte('$')
			continue
		}
		// NO SHELL-OPERATOR HANDLING. A brace body is taken as a NAME, and
		// anything that is not one simply does not resolve.
		//
		// A previous revision honoured `${VAR:-default}`, to spare llmdbench
		// fleets a permanent incompleteness. It introduced three ways to
		// fabricate a value and report it verified, which is worse than the
		// incompleteness it avoided, and a review measured all three:
		//
		//   - `env` cannot tell "unset" from "set by something this package
		//     cannot read". So `${VLLM_MAX_MODEL_LEN:-16384}` with the real
		//     value in a ConfigMap resolved to 16384 and reported
		//     Complete() == true -- a fabricated value handed a sharing key,
		//     on the exact manifest shape that motivated the feature, and
		//     flatly against the invariant envValues documents.
		//   - the fallback text was written out without being resolved, so
		//     `${A:-$B}` yielded the literal "$B" as a dtype, complete; and
		//     because varNameAt stops at the first `}`, `${A:-${B}}` yielded
		//     "fp8}".
		//   - the operator was detected by searching the body for "-", so
		//     every hyphen-bearing body was mis-split: `${A:+--quantization}`
		//     became "-quantization" and `${A%-suf}` became "suf".
		//
		// Each is the same defect as the one this file exists to fix, reached
		// through a convenience. An unresolvable reference stays unresolved;
		// the fleet then learns its own ITL line instead of borrowing one,
		// which is exactly what it did before any of this work.
		v, found := env[name]
		// A SUBSTITUTED VALUE THAT IS ITSELF A REFERENCE. The kubelet expands
		// `$(OTHER)` between env vars, so `--dtype $D` with `D="$(E)"`
		// resolves every lookup successfully and still yields "$(E)" -- which
		// was then hashed as a verified dtype.
		//
		// Caught here, at substitution, and not by re-scanning the output:
		// after `a$$b` becomes `a$b` the output is indistinguishable from an
		// unexpanded `$b`, and an output scan rejected the legitimate escape.
		// At this point the two are still separable, because an escape never
		// reaches this branch at all.
		if found && containsVarRef(v) {
			resolved = false
			out.WriteString(v)
			i = next - 1
			continue
		}
		if !found {
			resolved = false
			// Keep the reference in the output so a log or a label shows what
			// could not be read, rather than a misleading blank.
			out.WriteString(s[i:next])
			i = next - 1
			continue
		}
		out.WriteString(v)
		i = next - 1
	}
	return out.String(), resolved
}

// startsDelimitedRef reports whether the "$" at i opens a `${` or `$(` form,
// whether or not that form turns out to be well-formed.
//
// It is the difference between "this was never a reference" and "this was a
// reference I could not read", which varNameAt collapses into one ok=false and
// which resolveRefs has to tell apart: only the second may leave a value
// looking verified when it is not.
//
// DELIMITED, not "braced": it is true for `$(` as well, and a reader checking
// whether `$(FOO` with no closer gets recorded would read "braced" and
// conclude it does not. That is exactly the case this exists for.
func startsDelimitedRef(s string, i int) bool {
	return i+1 < len(s) && (s[i+1] == '{' || s[i+1] == '(')
}

// varNameAt reads the variable name of a reference beginning at the "$" at
// position i, returning the name and the index just past the reference.
// It recognises ${NAME}, $(NAME) and bare $NAME.
func varNameAt(s string, i int) (name string, next int, ok bool) {
	if i+1 >= len(s) {
		return "", 0, false
	}
	switch s[i+1] {
	case '{', '(':
		closer := byte('}')
		if s[i+1] == '(' {
			closer = ')'
		}
		for j := i + 2; j < len(s); j++ {
			if s[j] == closer {
				if j == i+2 {
					return "", 0, false // "${}" is not a reference
				}
				return s[i+2 : j], j + 1, true
			}
		}
		return "", 0, false // unterminated
	}
	// Bare $NAME: the shell's name charset, which is what the shell would use
	// to decide where the name ends.
	j := i + 1
	for j < len(s) && (s[j] == '_' ||
		(s[j] >= 'A' && s[j] <= 'Z') ||
		(s[j] >= 'a' && s[j] <= 'z') ||
		(j > i+1 && s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j == i+1 {
		return "", 0, false
	}
	return s[i+1 : j], j, true
}

// containsVarRef reports whether a value still holds something resolveRefs
// would treat as a variable reference.
//
// Reached only AFTER resolution, so a reference here means the substituted
// text itself contained one -- the kubelet expands `$(OTHER)` between env
// vars, so `--dtype $D` with `D="$(E)"` yields `"$(E)"` with every lookup
// having succeeded. It shares varNameAt with the resolver on purpose: a
// second, independently-written notion of "looks like a reference" is how the
// two would drift.
func containsVarRef(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			i++ // an escaped literal dollar, not a reference
			continue
		}
		if _, _, ok := varNameAt(s, i); ok {
			return true
		}
	}
	return false
}

// collectArgs merges container Command and Args, expanding shell commands.
// If the command is a shell invocation (e.g. ["/bin/sh", "-c", "..."]),
// the shell string is split into tokens.
func collectArgs(command, args []string) []string {
	all := make([]string, 0, len(command)+len(args))
	all = append(all, command...)
	all = append(all, args...)

	// Detect shell invocation: ["/bin/sh", "-c", "cmd ..."] or similar
	for i := 0; i < len(all)-1; i++ {
		base := all[i]
		if (base == "/bin/sh" || base == "/bin/bash" || base == "sh" || base == "bash") && i+1 < len(all) && all[i+1] == "-c" && i+2 < len(all) {
			// Split the shell command string
			shellTokens := splitShellString(all[i+2])
			return shellTokens
		}
	}

	return all
}

// splitShellString performs basic shell-like splitting on a command string.
// It handles simple single/double quoting, and shell line-continuation
// (a backslash immediately followed by a newline, the idiomatic way to write
// a long `vllm serve ...` invocation across multiple lines -- used by every
// scenario in this repo and, in practice, by real deployments generally). It
// is not a full shell parser: escape sequences (\"), variable expansion
// ($VAR), and command substitution are not supported.
//
// Line continuation matters more than it looks: left unhandled, the
// backslash and newline at the end of one line get glued onto the front of
// the next line's first token (e.g. "\\\n--max-num-batched-tokens" instead
// of "--max-num-batched-tokens"). That fails the "--" prefix check in
// parseArgsWith, so every flag after the first physical line of a multi-line
// command was silently skipped -- not a rare edge case, but the normal shape
// of a customCommand block. A bare newline (no preceding backslash, as
// between this function's non-flag preamble lines) is treated the same as a
// space: it ends a token, but isn't itself content.
func splitShellString(s string) []string {
	var tokens []string
	var current strings.Builder
	inSingleQuote := false
	inDoubleQuote := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && !inSingleQuote && !inDoubleQuote && isLineBreakAt(s, i+1):
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
			if s[i+1] == '\r' {
				i++ // CRLF: consume the carriage return too
			}
			i++ // consume the newline
		case ch == '\'' && !inDoubleQuote:
			inSingleQuote = !inSingleQuote
		case ch == '"' && !inSingleQuote:
			inDoubleQuote = !inDoubleQuote
		case isSpace(ch) && !inSingleQuote && !inDoubleQuote:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// isSpace reports whether ch separates tokens. Tabs and carriage returns
// count: a YAML block scalar indented with tabs, or a manifest authored on
// Windows, otherwise glues the whitespace onto the next token, which then
// fails the "--" prefix check in parseArgsWith and is silently skipped --
// the same failure mode as an unhandled line continuation.
func isSpace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r'
}

// isLineBreakAt reports whether position i begins a newline, LF or CRLF, so a
// trailing backslash is recognised as a line continuation in both.
func isLineBreakAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	return s[i] == '\n' || (s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n')
}
