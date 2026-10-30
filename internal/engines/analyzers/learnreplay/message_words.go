package learnreplay

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// messageWords is every token of the code's own messages: the whole
// tokens (tokens) of the Go string literals, fmt verbs removed, of the
// packages that write what a
// recording holds (MessageWordSources), as written. A message an analyzer
// returns is built from these words, names and numbers, so a word that is
// none of them -- a team, a cluster -- is not the code's, and the export
// refuses it. Generated: `make learner-message-words` writes the file, and
// a test fails when it is stale.
//
//go:embed message_words.txt
var messageWordsText string

var messageWords = func() map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(messageWordsText) {
		out[w] = true
	}
	return out
}()

// MessageWordSources are the directories, from the module root, whose
// string literals make messageWords: the analyzers, the engine that calls
// them, the signals and capacity store they read, the policy, and the
// domain types. The analyzers wrap no client library's errors (router
// latency is served from the recording), so no library's texts are needed.
var MessageWordSources = []string{
	"internal/engines", "internal/signals", "internal/config", "internal/domain",
	"internal/inferenceengine", "internal/accelerator",
}

// MessageWords reads the words of every string literal in the non-test Go
// files under root's MessageWordSources, sorted, one per line.
func MessageWords(root string) (string, error) {
	words := map[string]bool{}
	fset := token.NewFileSet()
	for _, src := range MessageWordSources {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(src)), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && filepath.ToSlash(p) == filepath.ToSlash(filepath.Join(root, "internal/engines/analyzers/learnreplay")) {
				return filepath.SkipDir // its own literals are its tables, not messages
			}
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				for _, w := range tokens(fmtVerb.ReplaceAllString(s, " ")) {
					if isASCII(w) {
						words[w] = true
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	list := make([]string, 0, len(words))
	for w := range words {
		list = append(list, w)
	}
	slices.Sort(list)
	return strings.Join(list, "\n") + "\n", nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// SourceExample is a source as the manifest wants one: short, of ordinary
// words.
const SourceExample = "E1 benchmark scenario, recorder on, default analyzerTuning"

// sourceWords are the ordinary words a person may write in a source beyond
// the code's own (messageWords): lowercase, one per line. A word added here
// must be ordinary English, never a name; a test holds the file to
// lowercase, and the export refuses a source that holds a name the
// recording holds whatever this list says.
//
//go:embed source_words.txt
var sourceWordsText string

var sourceWords = func() map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(sourceWordsText) {
		out[w] = true
	}
	return out
}()

// IsSourceWord reports whether w (lowercased) is one of the committed source
// words: for a test outside this package to hold the list to names it must
// never hold.
func IsSourceWord(w string) bool { return sourceWords[asciiLower(w)] }
