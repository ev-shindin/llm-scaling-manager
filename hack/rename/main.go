// Command rename renames the project throughout the tree. It is a dry run
// unless given --apply, and it is safe to run again on any later main: a second
// run over a renamed tree changes nothing.
//
// The rules live in rules.go. docs/developer-guide/renaming.md says how to use
// it and what each class of name does on upgrade.
package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "rename:", err)
		os.Exit(1)
	}
}

// Options are the command line.
type Options struct {
	Root              string
	Report            string
	Apply             bool
	AllowUnclassified bool
	ExpectNoop        bool
	Sweep             bool
	Targets           Targets
}

func parseOptions(args []string) (Options, error) {
	opts := Options{Targets: DefaultTargets}
	fs := flag.NewFlagSet("rename", flag.ContinueOnError)
	fs.StringVar(&opts.Root, "root", ".", "repository root")
	fs.StringVar(&opts.Report, "report", "", "report path (default <root>/bin/rename-report.md)")
	fs.Bool("dry-run", true, "write the report and change nothing (the default; --apply overrides it)")
	fs.BoolVar(&opts.Apply, "apply", false, "rewrite the tree; refused while anything is unclassified")
	fs.BoolVar(&opts.AllowUnclassified, "allow-unclassified", false, "let --apply run with unclassified occurrences left")
	fs.BoolVar(&opts.ExpectNoop, "expect-noop", false, "exit non-zero if the run would change any file")
	fs.BoolVar(&opts.Targets.CodeMoved, "code-moved", DefaultTargets.CodeMoved,
		"the code, issues and images now live in the new repository: rename the held names too")
	fs.StringVar(&opts.Targets.Module, "module", DefaultTargets.Module, "target Go module path")
	fs.BoolVar(&opts.Sweep, "sweep", false, "exit non-zero if the final sweep finds a hit with no reason; the report always holds it")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if opts.Report == "" {
		opts.Report = filepath.Join(opts.Root, "bin", "rename-report.md")
	}
	return opts, nil
}

func run(args []string, stdout io.Writer) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	metrics, err := metricNames(opts.Root)
	if err != nil {
		return err
	}
	e, err := NewEngine(opts.Targets, metrics)
	if err != nil {
		return err
	}
	files, err := trackedFiles(opts.Root)
	if err != nil {
		return err
	}
	res, err := e.Plan(opts.Root, files)
	if err != nil {
		return err
	}
	res.Metrics = metrics
	res.Sweep, err = e.FinalSweep(opts.Root, files)
	if err != nil {
		return err
	}

	if err := writeReport(opts, e, res); err != nil {
		return err
	}
	unclassified := e.Stats.UnclassifiedCount()
	fmt.Fprintf(stdout, "files changed: %d, paths renamed: %d, unclassified: %d, report: %s\n",
		len(res.Changed), len(res.Renames), unclassified, opts.Report)

	if opts.Sweep && res.Sweep.Unexplained > 0 {
		return fmt.Errorf("the sweep found %d hits with no reason: see the report", res.Sweep.Unexplained)
	}
	if opts.ExpectNoop && (len(res.Changed) > 0 || len(res.Renames) > 0) {
		return fmt.Errorf("expected no change, the run would change %d files and rename %d paths",
			len(res.Changed), len(res.Renames))
	}
	if !opts.Apply {
		return nil
	}
	if unclassified > 0 && !opts.AllowUnclassified {
		return fmt.Errorf("refusing --apply: %d occurrences are unclassified (see the report), or pass --allow-unclassified", unclassified)
	}
	return res.Apply(opts.Root)
}

// tracked is one file git knows about.
type tracked struct {
	path string
	mode string
}

// trackedFiles lists what git tracks. Untracked files are not the tool's to
// touch, and neither are symlinks or submodules.
func trackedFiles(root string) ([]tracked, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-s", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var files []tracked
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if rec == "" {
			continue
		}
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-files: bad record %q", rec)
		}
		mode, _, _ := strings.Cut(meta, " ")
		files = append(files, tracked{path: p, mode: mode})
	}
	if len(files) == 0 {
		return nil, errors.New("git tracks no files here: an empty list is not a pass")
	}
	return files, nil
}

// Changed is a file with new content.
type Changed struct {
	Path    string
	Content []byte
}

// Move is a path to move.
type Move struct {
	From, To string
}

// Result is what a run would do.
type Result struct {
	Files    int
	Binary   []BinaryFile
	Changed  []Changed
	Renames  []Move
	Metrics  []string
	Skipped  []string
	Sweep    *Sweep
	LongName map[string]int
}

// BinaryFile is a file the tool will not edit, with how often the old name
// turns up in it. A compressed file is counted after decompressing.
type BinaryFile struct {
	Path     string
	Mentions int
}

// Plan works out every change without making one.
func (e *Engine) Plan(root string, files []tracked) (*Result, error) {
	res := &Result{Files: len(files), LongName: map[string]int{}}
	for _, f := range files {
		class := classify(f.path)
		if class == ClassTool {
			continue
		}
		if f.mode != "100644" && f.mode != "100755" {
			res.Skipped = append(res.Skipped, f.path+" (mode "+f.mode+")")
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.path)))
		if err != nil {
			return nil, err
		}
		if isBinary(f.path, b) {
			res.Binary = append(res.Binary, BinaryFile{f.path, e.countInBinary(f.path, b)})
		} else {
			out, err := e.rewriteFile(f.path, class, b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.path, err)
			}
			if !bytes.Equal(out, b) {
				res.Changed = append(res.Changed, Changed{f.path, out})
				collectLongNames(res.LongName, out)
			}
		}
		np := string(e.RewriteText(f.path, class, CtxPath, []byte(f.path), 1))
		if np != f.path {
			res.Renames = append(res.Renames, Move{f.path, np})
		}
	}
	return res, res.checkRenames(files)
}

// checkRenames refuses a rename onto a path something else holds.
func (r *Result) checkRenames(files []tracked) error {
	taken := map[string]string{}
	for _, f := range files {
		taken[f.path] = f.path
	}
	for _, m := range r.Renames {
		if by, ok := taken[m.To]; ok {
			return fmt.Errorf("rename of %s lands on %s, which %s holds", m.From, m.To, by)
		}
		taken[m.To] = m.From
	}
	return nil
}

// rewriteFile returns the new content of one text file, with the line endings
// it came with.
func (e *Engine) rewriteFile(p, class string, b []byte) ([]byte, error) {
	if !strings.HasSuffix(p, ".go") {
		return e.RewriteText(p, class, CtxText, b, 0), nil
	}
	if !e.mentionsOld(b) && !e.mentionsIdent(b) {
		return b, nil
	}
	endings := detectEndings(b)
	if endings == endMixed {
		return nil, errors.New("mixed line endings: gofmt would change them, fix the file first")
	}
	src := b
	if endings == endCRLF {
		src = toLF(b)
	}
	out, err := e.RewriteGo(p, class, src)
	if err != nil {
		return nil, err
	}
	if endings == endCRLF {
		out = toCRLF(out)
	}
	return out, nil
}

// mentionsIdent catches a file whose only old name is an identifier the
// residual pattern does not see.
func (e *Engine) mentionsIdent(b []byte) bool {
	return bytes.Contains(bytes.ToLower(b), []byte("wva"))
}

// isBinary is true for a file the tool must never edit: a known binary
// extension, or a NUL byte near the start.
func isBinary(p string, b []byte) bool {
	if slices.Contains(binaryExtensions, strings.ToLower(path.Ext(p))) {
		return true
	}
	return bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0
}

// countInBinary counts the old name in a binary file, for the report only.
func (e *Engine) countInBinary(p string, b []byte) int {
	if strings.HasSuffix(p, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err == nil {
			if plain, err := io.ReadAll(zr); err == nil {
				b = plain
			}
		}
	}
	return len(e.residualRe.FindAllIndex(b, -1))
}

// Apply writes the plan. Writing into the open file keeps its mode, and
// git mv keeps the index in step and the mode with it.
func (r *Result) Apply(root string) error {
	for _, c := range r.Changed {
		f, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(c.Path)), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return err
		}
		_, werr := f.Write(c.Content)
		if err := errors.Join(werr, f.Close()); err != nil {
			return err
		}
	}
	sort.Slice(r.Renames, func(i, j int) bool { return r.Renames[i].From < r.Renames[j].From })
	for _, m := range r.Renames {
		dst := filepath.Join(root, filepath.FromSlash(m.To))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		cmd := exec.Command("git", "-C", root, "mv", m.From, m.To)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git mv %s %s: %w: %s", m.From, m.To, err, out)
		}
	}
	return nil
}
