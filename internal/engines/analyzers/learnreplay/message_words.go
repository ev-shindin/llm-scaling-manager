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

// messageWords is every word of the code's own messages: the runs of
// letters in the Go string literals of the packages that write what a
// recording holds (MessageWordSources), lowercased. A message an analyzer
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
				for _, w := range letterRuns.FindAllString(s, -1) {
					words[asciiLower(w)] = true
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
