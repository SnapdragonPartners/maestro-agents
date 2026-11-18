package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
	"github.com/SnapdragonPartners/maestro-agents/store"
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
		exp, err = storedExport(ctx, objs, f.rewrite)
	case f.approved:
		var ids []string
		if ids, err = unexported(ctx, objs); err == nil && len(ids) == 0 {
			say(stdout, "nothing to export: no approved record is waiting\n")
			return exitOK
		}
		if err == nil {
			exp, err = submitExport(ctx, h, objs, f.by, ids)
		}
	default:
		exp, err = submitExport(ctx, h, objs, f.by, f.records)
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

// unexported lists the approved records whose current approval no export
// holds.
func unexported(ctx context.Context, objs review.StoreObjects) ([]string, error) {
	all, err := review.Records(ctx, objs)
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}
	exports, err := review.Exports(ctx, objs)
	if err != nil {
		return nil, fmt.Errorf("list exports: %w", err)
	}
	done := review.ExportedApprovals(exports)
	var ids []string
	for _, id := range all {
		s, err := review.Load(ctx, objs, id)
		if err != nil {
			return nil, fmt.Errorf("record %s: %w", id, err)
		}
		if s.Approved() && !done[id][s.Approval] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// errRefused is an action the review run refused: the message is the
// person's to act on.
var errRefused = errors.New("refused")

// submitExport runs the export and returns what it stored.
func submitExport(ctx context.Context, h *standalone.Host, objs review.StoreObjects, by string, ids []string) (*review.Exported, error) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	a := review.Action{Kind: review.Export, By: by, At: time.Now().UTC(), Records: ids}
	payload, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("encode the action: %w", err)
	}
	rep, err := h.Submit(ctx, standalone.Submission{
		AgentType: review.TypeName, Instance: review.ExportInstancePrefix + hex.EncodeToString(b[:]), Payload: payload,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // standalone names it
	}
	if err = outcome(ctx, objs, &rep); err != nil {
		return nil, err
	}
	return storedExport(ctx, objs, rep.Run)
}

// storedExport reads the export a run stored.
func storedExport(ctx context.Context, objs review.StoreObjects, run string) (*review.Exported, error) {
	var exp review.Exported
	o, ok, err := objs.Reader.Object(ctx, objs.Namespace, review.ExportKey(run))
	if err != nil || !ok {
		return nil, fmt.Errorf("read the export of run %s: %v", run, cmpErr(err, "no such export"))
	}
	if err := json.Unmarshal(o.JSON, &exp); err != nil || exp.Export == nil {
		return nil, fmt.Errorf("decode the export of run %s: %v", run, cmpErr(err, "no export"))
	}
	return &exp, nil
}

// outcome turns a review run's report into nil when it applied, the
// refusal when it refused, and an error otherwise.
func outcome(ctx context.Context, objs review.StoreObjects, rep *standalone.Report) error {
	switch {
	case rep.Error != "":
		return fmt.Errorf("run %s has no confirmed outcome: %s", rep.Run, rep.Error)
	case rep.Status == store.StatusSucceeded && rep.Reason.Code == agent.ReasonChanged:
		return nil
	case rep.Status == store.StatusSucceeded && rep.Reason.Code == agent.ReasonNoChange:
		var r review.Refusal
		o, ok, err := objs.Reader.Object(ctx, objs.Namespace, review.RefusalKey(rep.Run))
		if err != nil || !ok || json.Unmarshal(o.JSON, &r) != nil {
			return fmt.Errorf("%w (run %s; its refusal cannot be read)", errRefused, rep.Run)
		}
		return fmt.Errorf("%w: %s: %s", errRefused, r.Code, r.Message)
	}
	return fmt.Errorf("run %s ended %s (%s)", rep.Run, rep.Status, rep.Reason.Code)
}

func cmpErr(err error, otherwise string) any {
	if err != nil {
		return err
	}
	return otherwise
}
