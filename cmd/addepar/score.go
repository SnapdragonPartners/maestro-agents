package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/score"
	"github.com/SnapdragonPartners/maestro-agents/scoring"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

// maxKeyBytes bounds the answer key and the mapping read into memory.
const maxKeyBytes = 16 << 20

// scoreCommand runs the agent on every PDF in a folder, one at a time, and
// scores each statement's records against the answer key (ADR 0020, item
// 9). It exits 0 when nothing wrong came out, 1 on a mismatch, a
// hallucination, an error or an input not run, and 2 on a usage error.
func scoreCommand(ctx context.Context, p standalone.Program, args []string, stdout, stderr io.Writer) int { //nolint:gocritic // hugeParam: called once
	fs := flag.NewFlagSet("score", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "the standalone configuration (one_shot trigger); use a fresh store to score a change")
	inputs := fs.String("inputs", "", "the folder of statement PDFs")
	keyPath := fs.String("key", "", "the answer key: the upload as CSV in the exporter's columns")
	mapPath := fs.String("map", "", "the mapping of each statement's positions to answer rows (JSON)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *cfgPath == "" || *inputs == "" || *keyPath == "" || *mapPath == "" || fs.NArg() != 0 {
		say(stderr, "score: -config, -inputs, -key and -map are required\n")
		return exitUsage
	}
	cmp, err := loadComparator(*keyPath, *mapPath)
	if err != nil {
		say(stderr, "score: %v\n", err)
		return exitUsage
	}
	in, err := pdfInputs(*inputs)
	if err != nil {
		say(stderr, "score: %v\n", err)
		return exitUsage
	}
	h, err := standalone.Open(ctx, p, *cfgPath, stderr)
	if err != nil {
		say(stderr, "score: %v\n", err)
		return exitUsage
	}
	defer h.Close() //nolint:errcheck // Close logs its own failures
	rep := scoring.Run(ctx, h, in, cmp, nil)
	write := rep.WriteText
	if *asJSON {
		write = rep.WriteJSON
	}
	if err := write(stdout); err != nil {
		say(stderr, "score: %v\n", err)
		return exitFail
	}
	if rep.Failed() {
		return exitFail
	}
	return exitOK
}

func loadComparator(keyPath, mapPath string) (*score.Comparator, error) {
	kb, err := readCapped(keyPath, maxKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("answer key: %w", err)
	}
	k, err := score.LoadKey(strings.NewReader(string(kb)))
	if err != nil {
		return nil, err //nolint:wrapcheck // names the answer key
	}
	mb, err := readCapped(mapPath, maxKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("mapping: %w", err)
	}
	m, err := score.LoadMapping(strings.NewReader(string(mb)))
	if err != nil {
		return nil, err //nolint:wrapcheck // names the mapping
	}
	return score.NewComparator(k, m) //nolint:wrapcheck // names the statement
}

// pdfInputs lists the regular .pdf files directly in dir, sorted by name:
// no subfolders, dotfiles or symlinks, as the folder watch skips them.
func pdfInputs(dir string) ([]scoring.Input, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("inputs: %w", err)
	}
	in := make([]scoring.Input, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !e.Type().IsRegular() || !strings.EqualFold(filepath.Ext(name), ".pdf") {
			continue
		}
		in = append(in, scoring.Input{Path: filepath.Join(dir, name)})
	}
	if len(in) == 0 {
		return nil, fmt.Errorf("inputs: no PDF in %s", dir)
	}
	sort.Slice(in, func(i, j int) bool { return in[i].Path < in[j].Path })
	return in, nil
}
