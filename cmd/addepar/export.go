package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(s string) error {
	*l = append(*l, s)
	return nil
}

// exportFlags are export's arguments.
type exportFlags struct {
	cfg, by, out, manifests, rewrite string
	records                          stringList
	approved                         bool
}

// parseExportFlags reads and checks export's arguments; false is a usage
// error, already reported.
func parseExportFlags(args []string, stderr io.Writer) (exportFlags, bool) {
	var f exportFlags
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.cfg, "config", "", "the standalone configuration (its store must not be in use)")
	fs.StringVar(&f.by, "by", "", "who exports")
	fs.StringVar(&f.out, "out", "", "the folder the CSV is written to")
	fs.StringVar(&f.manifests, "manifests", "", "the folder the evidence manifest is written to (created if missing)")
	fs.BoolVar(&f.approved, "approved", false, "export every approved record not yet exported")
	fs.Var(&f.records, "record", "a record to export, <sha256>/<NN> (repeatable); an exported one is exported again")
	fs.StringVar(&f.rewrite, "rewrite", "", "write the files of the stored export made by this run again, byte for byte")
	if err := fs.Parse(args); err != nil {
		return f, false
	}
	modes := 0
	for _, on := range []bool{f.approved, len(f.records) > 0, f.rewrite != ""} {
		if on {
			modes++
		}
	}
	switch {
	case f.cfg == "" || f.out == "" || f.manifests == "" || fs.NArg() != 0:
		say(stderr, "export: -config, -out and -manifests are required\n")
		return f, false
	case modes != 1:
		say(stderr, "export: give one of -approved, -record or -rewrite\n")
		return f, false
	case f.rewrite == "" && strings.TrimSpace(f.by) == "":
		say(stderr, "export: -by is required to export\n")
		return f, false
	}
	if fi, err := os.Stat(f.out); err != nil || !fi.IsDir() {
		say(stderr, "export: -out %s is not a folder\n", f.out)
		return f, false
	}
	if err := os.MkdirAll(f.manifests, 0o700); err != nil {
		say(stderr, "export: -manifests: %v\n", err)
		return f, false
	}
	return f, true
}

// exportCommand exports approved records (ADR 0021, items 9 and 11): an
// export run on its own instance, then the CSV into -out and the manifest
// into -manifests, both written from what the run committed. -rewrite
// writes a stored export's files again, for a write that failed after its
// commit.
func exportCommand(ctx context.Context, p standalone.Program, args []string, stdout, stderr io.Writer) int { //nolint:gocritic // hugeParam: called once
	f, ok := parseExportFlags(args, stderr)
	if !ok {
		return exitUsage
	}
	h, err := standalone.Open(ctx, p, f.cfg, stderr)
	if err != nil {
		say(stderr, "export: %v\n", err)
		return exitUsage
	}
	defer h.Close() //nolint:errcheck // Close logs its own failures
	objs := review.StoreObjects{Reader: h.Store(), Namespace: h.Namespace()}
	var exp *review.Exported
	switch {
	case f.rewrite != "":
		exp, err = review.StoredExport(ctx, objs, f.rewrite)
	case f.approved:
		var ids []string
		if ids, err = review.Unexported(ctx, objs); err == nil && len(ids) == 0 {
			say(stdout, "nothing to export: no approved record is waiting\n")
			return exitOK
		}
		if err == nil {
			exp, err = review.SubmitExport(ctx, h, objs, f.by, time.Now(), ids)
		}
	default:
		exp, err = review.SubmitExport(ctx, h, objs, f.by, time.Now(), f.records)
	}
	if err != nil {
		say(stderr, "export: %v\n", err)
		return exitFail
	}
	csvPath, manPath, err := exp.Export.Write(f.out, f.manifests)
	if err != nil {
		say(stderr, "export: the export is stored (run %s) but its files were not written: %v\n"+
			"write them with: export -config %s -out %s -manifests %s -rewrite %s\n",
			exp.Run, err, f.cfg, f.out, f.manifests, exp.Run)
		return exitFail
	}
	say(stdout, "exported %d record(s) (run %s)\ncsv: %s\nmanifest: %s\n", len(exp.Records), exp.Run, csvPath, manPath)
	return exitOK
}
