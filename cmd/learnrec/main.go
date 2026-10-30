// Command learnrec turns learner recordings into committed test fixtures
// (docs/developer-guide/testing.md, Learner Goldens):
//
//	learnrec export -in RAW.jsonl -out EXPORT.jsonl[.gz]
//	learnrec add    -in RAW.jsonl -name NAME -source TEXT [-head-lines N] [-replace]
//	learnrec update
//	learnrec check
//
// export pseudonymises a recording. add exports it, holds the export to a
// replay that reproduces it byte for byte at this commit, and commits it as a
// golden fixture with its outputs. update rewrites every fixture's outputs
// from a replay at this commit. check is the golden check `make test` runs.
// Run from the module root; the make targets do.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/learnreplay"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "learnrec:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: learnrec export|add|update|check [flags]")
	}
	fs := flag.NewFlagSet("learnrec "+args[0], flag.ContinueOnError)
	dir := fs.String("dir", learnreplay.GoldenDir, "the golden directory")
	ctx := context.Background()
	switch args[0] {
	case "export":
		in := fs.String("in", "", "the raw recording")
		out := fs.String("out", "", "where to write the export; a name ending in .gz is gzipped")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *in == "" || *out == "" {
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
		if strings.HasSuffix(*out, ".gz") {
			if exported, err = learnreplay.Gzip(exported); err != nil {
				return err
			}
		}
		if err := os.WriteFile(*out, exported, 0o644); err != nil {
			return err
		}
		fmt.Printf("exported %s: %s\n", *out, rep)
	case "add":
		in := fs.String("in", "", "the raw recording")
		name := fs.String("name", "", "the fixture's name: lowercase letters, digits and dashes")
		source := fs.String("source", "", "where the recording came from: scenario, build, policy; no cluster or namespace names")
		head := fs.Int("head-lines", 0, "keep only the recording's first N lines")
		replace := fs.Bool("replace", false, "replace a fixture of the same name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *in == "" || *name == "" || *source == "" {
			return errors.New("add needs -in, -name and -source")
		}
		raw, err := os.ReadFile(*in)
		if err != nil {
			return err
		}
		rep, err := learnreplay.AddGolden(ctx, *dir, *name, raw,
			learnreplay.AddOptions{Source: *source, HeadLines: *head, Replace: *replace})
		if err != nil {
			return err
		}
		fmt.Printf("added %s: %s\n", *name, rep)
		return check(ctx, *dir)
	case "update":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := learnreplay.UpdateGoldens(ctx, *dir); err != nil {
			return err
		}
		fmt.Println("rewrote the goldens")
		return check(ctx, *dir)
	case "check":
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return check(ctx, *dir)
	default:
		return fmt.Errorf("unknown command %q: export, add, update or check", args[0])
	}
	return nil
}

func check(ctx context.Context, dir string) error {
	c, err := learnreplay.CheckGoldens(ctx, dir)
	if err != nil {
		return err
	}
	rows := 0
	for _, n := range c.Rows {
		rows += n
	}
	fmt.Printf("golden check: %d fixtures replayed, %d golden rows compared\n", c.Fixtures, rows)
	for _, p := range c.Problems {
		fmt.Println("  " + p)
	}
	if len(c.Problems) > 0 {
		return fmt.Errorf("%d golden problems", len(c.Problems))
	}
	return nil
}
