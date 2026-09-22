package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"tenant/internal/runtrace"
)

func cmdRuns(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return fmt.Errorf("usage: tenant runs export [--data DIR] [--out FILE]")
	}
	fs := flag.NewFlagSet("runs export", flag.ContinueOnError)
	c := bindCommon(fs)
	outPath := fs.String("out", "-", "JSONL output file ('-' for stdout)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	store, err := runtrace.Open(filepath.Join(c.dataDir, "runs.db"), effectiveRunTraceConfig(c.lc))
	if err != nil {
		return err
	}
	defer store.Close()
	var w io.Writer = os.Stdout
	var f *os.File
	if *outPath != "-" {
		f, err = os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("runs export: create output: %w", err)
		}
		defer f.Close()
		w = f
	}
	if err := store.ExportJSONL(ctx, w); err != nil {
		return err
	}
	if f != nil {
		if err := f.Sync(); err != nil {
			return fmt.Errorf("runs export: sync: %w", err)
		}
	}
	return nil
}
