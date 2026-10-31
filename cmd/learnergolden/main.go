// Command learnergolden turns learner recordings into committed test
// fixtures (docs/developer-guide/testing.md, Learner Goldens):
//
//	learnergolden export -in RAW.jsonl -out EXPORT.jsonl[.gz]
//	learnergolden add    -in RAW.jsonl -name NAME -source TEXT [-head-lines N] [-replace] [-dir DIR]
//	learnergolden update [-dir DIR]
//	learnergolden check  [-dir DIR]
//
// export pseudonymises a recording. add exports it, holds the export to a
// replay that reproduces it byte for byte at this commit, and commits it as a
// golden fixture with its outputs. update rewrites every fixture's outputs
// from a replay at this commit. check is the golden check make test runs.
// Run from the module root; the make targets do, and set the commit the
// add compares with (learnrec.Commit).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/signals/learnrec"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "learnergolden:", err)
		os.Exit(1)
	}
}

const usage = "usage: learnergolden export|add|update|check [flags]; -h after a command lists its flags"

// run runs one command, writing what it reports to out. Asking for help is
// not an error.
func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("learnergolden "+args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	parse := func() (bool, error) {
		err := fs.Parse(args[1:])
		if errors.Is(err, flag.ErrHelp) {
			return false, nil
		}
		return err == nil, err
	}
	switch args[0] {
	case "export":
		in := fs.String("in", "", "the raw recording")
		dst := fs.String("out", "", "where to write the export; a name ending in .gz is gzipped")
		if ok, err := parse(); !ok {
			return err
		}
		if *in == "" || *dst == "" {
			return errors.New("export needs -in and -out")
		}
		raw, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		exported, rep, err := learnreplay.Export(raw, learnreplay.ExportOptions{})
		if err != nil {
			return err
		}
		if strings.HasSuffix(*dst, ".gz") {
			if exported, err = learnreplay.Gzip(exported); err != nil {
				return err
			}
		}
		if err := os.WriteFile(*dst, exported, 0o644); err != nil {
			return err
		}
		report(out, "exported "+*dst, rep)
	case "add":
		dir := fs.String("dir", learnreplay.GoldenDir, "the golden directory")
		in := fs.String("in", "", "the raw recording")
		name := fs.String("name", "", "the fixture's name: lowercase letters, digits and dashes")
		source := fs.String("source", "", "where the recording came from: scenario, build, policy; no cluster or namespace names")
		head := fs.Int("head-lines", 0, "keep only the recording's first N lines")
		replace := fs.Bool("replace", false, "replace a fixture of the same name")
		if ok, err := parse(); !ok {
			return err
		}
		if *in == "" || *name == "" || *source == "" {
			return errors.New("add needs -in, -name and -source")
		}
		raw, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		rep, err := learnreplay.AddGolden(ctx, *dir, *name, raw, learnreplay.AddOptions{
			Source: *source, HeadLines: *head, Replace: *replace, Commit: learnrec.Commit()})
		if err != nil {
			return err
		}
		report(out, "added "+*name, rep)
		return check(ctx, *dir, out, false)
	case "update":
		dir := fs.String("dir", learnreplay.GoldenDir, "the golden directory")
		if ok, err := parse(); !ok {
			return err
		}
		if err := learnreplay.UpdateGoldens(ctx, *dir); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, "rewrote the goldens")
		return check(ctx, *dir, out, false)
	case "check":
		dir := fs.String("dir", learnreplay.GoldenDir, "the golden directory")
		if ok, err := parse(); !ok {
			return err
		}
		return check(ctx, *dir, out, true)
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}
	return nil
}

// report prints an export's report, and what it kept as it is, for a
// person to read before committing.
func report(out io.Writer, what string, rep *learnreplay.ExportReport) {
	_, _ = fmt.Fprintf(out, "%s: %s\n", what, rep)
	_, _ = fmt.Fprintf(out, "kept as they are (read them before committing): %s\n", strings.Join(rep.Kept, ", "))
}

// check runs the golden check on dir and prints what it found. A learner
// path no golden runs fails it only when covered is set: the committed set
// must run every path between its goldens, and an add or an update reports
// the paths still uncovered without failing on them.
func check(ctx context.Context, dir string, out io.Writer, covered bool) error {
	c, err := learnreplay.CheckGoldens(ctx, dir)
	if err != nil {
		return err
	}
	rows := 0
	for _, n := range c.Rows {
		rows += n
	}
	_, _ = fmt.Fprintf(out, "golden check: %d fixtures replayed, %d golden rows compared\n", c.Fixtures, rows)
	_, _ = fmt.Fprintf(out, "learner paths run, over every fixture: %v\n", c.Paths)
	for _, p := range c.Problems {
		_, _ = fmt.Fprintln(out, "  "+p)
	}
	for _, p := range c.Uncovered {
		_, _ = fmt.Fprintln(out, "  "+p)
	}
	switch {
	case len(c.Problems) > 0:
		return fmt.Errorf("%d golden problems, %d learner paths no golden runs", len(c.Problems), len(c.Uncovered))
	case covered && len(c.Uncovered) > 0:
		return fmt.Errorf("%d learner paths no golden runs", len(c.Uncovered))
	}
	return nil
}
