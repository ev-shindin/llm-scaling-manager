package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Reasons the sweep gives that no rule row does.
const (
	reasonTool    = "the tool itself"
	reasonBinary  = "binary or compressed, never edited"
	reasonPartial = "not the name: a capital inside a word, as in newValue"
	reasonNone    = "UNEXPLAINED"
)

// SweepHit is one remaining occurrence.
type SweepHit struct {
	Path   string
	Line   int
	Reason string
	Text   string
}

// Sweep is the final list of what is left. It looks in every tracked file and at
// every tracked path, in any case, and leaves nothing out: a hit carries the rule
// that keeps it, or says it has none.
type Sweep struct {
	Hits        []SweepHit
	Counts      map[string]map[string]int
	Unexplained int
}

func newSweep() *Sweep { return &Sweep{Counts: map[string]map[string]int{}} }

func (s *Sweep) add(h SweepHit) {
	s.Hits = append(s.Hits, h)
	kind := fileType(h.Path)
	if s.Counts[kind] == nil {
		s.Counts[kind] = map[string]int{}
	}
	s.Counts[kind][h.Reason]++
	if h.Reason == reasonNone {
		s.Unexplained++
	}
}

// fileType is a file's type for the report: its extension, or its name.
func fileType(p string) string {
	if ext := path.Ext(p); ext != "" {
		return ext
	}
	return path.Base(p)
}

// FinalSweep lists every remaining hit in the tree as it stands.
func (e *Engine) FinalSweep(root string, files []tracked) (*Sweep, error) {
	re := regexp.MustCompile(sweepPattern)
	out := newSweep()
	for _, f := range files {
		for _, loc := range re.FindAllStringIndex(f.path, -1) {
			out.add(SweepHit{f.path, 0, e.reason(f.path, f.path, loc[0], loc[1]), "(the path)"})
		}
		if f.mode != "100644" && f.mode != "100755" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.path)))
		if err != nil {
			return nil, err
		}
		if isBinary(f.path, b) {
			if strings.HasSuffix(f.path, ".gz") {
				if zr, err := gzip.NewReader(bytes.NewReader(b)); err == nil {
					if plain, err := io.ReadAll(zr); err == nil {
						b = plain
					}
				}
			}
			if n := len(re.FindAll(b, -1)); n > 0 {
				out.add(SweepHit{f.path, 0, reasonBinary, fmt.Sprintf("(%d mentions)", n)})
			}
			continue
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, loc := range re.FindAllStringIndex(line, -1) {
				out.add(SweepHit{f.path, i + 1, e.reason(f.path, line, loc[0], loc[1]), strings.TrimSpace(strings.TrimSuffix(line, "\r"))})
			}
		}
	}
	return out, nil
}

// reason says why the hit at line[start:end] is still there.
func (e *Engine) reason(p, line string, start, end int) string {
	if classify(p) == ClassTool {
		return reasonTool
	}
	if hit := line[start:end]; strings.EqualFold(hit, "wva") && hit != "wva" && hit != "Wva" && hit != "WVA" {
		return reasonPartial
	}
	for i := range e.Rules {
		r := &e.Rules[i]
		if e.effective(r) == Rename || !r.pathApplies(p) {
			continue
		}
		for _, loc := range r.re.FindAllStringSubmatchIndex(line, -1) {
			from, to := loc[0], loc[1]
			if r.m > 0 {
				from, to = loc[2*r.m], loc[2*r.m+1]
			}
			if from >= 0 && from <= start && end <= to {
				return r.Name
			}
		}
	}
	return reasonNone
}

// write adds the sweep to the report.
func (s *Sweep) write(b *bytes.Buffer) {
	fmt.Fprintf(b, "\n## Final sweep\n\n%d hits, %d with no reason.\n\n| file type | reason | hits |\n|---|---|---:|\n", len(s.Hits), s.Unexplained)
	kinds := make([]string, 0, len(s.Counts))
	for k := range s.Counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		reasons := make([]string, 0, len(s.Counts[k]))
		for r := range s.Counts[k] {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		for _, r := range reasons {
			fmt.Fprintf(b, "| %s | %s | %d |\n", k, r, s.Counts[k][r])
		}
	}
	b.WriteString("\n### Every hit\n\n")
	for _, h := range s.Hits {
		text := h.Text
		if len(text) > 160 {
			text = text[:160]
		}
		fmt.Fprintf(b, "- %s:%d [%s] %s\n", h.Path, h.Line, h.Reason, text)
	}
}
