// Command addepar is the capital-statement intake agent: one PDF statement
// in, a verified valuation record and an Addepar import row out (docs/MVP.md).
//
// It runs the intake agent (ADR 0018) on the standalone host (ADR 0017),
// one run per statement:
//
//	addepar run -config FILE [-input FILE]
//	addepar config validate -config FILE
//
// and verifies a hand-written extraction record against its statement,
// with no model and no store:
//
//	addepar verify -pdf statement.pdf -record record.json
//
// verify prints the seven checks' result as JSON and exits 0 when the record has
// no error (warnings still need a person's acceptance), 1 when it has one,
// and 2 on a usage or input error. A copy-restricted PDF's text is read only
// with -override-by and -override-reason, the operator's recorded acceptance;
// without them the record fails grounding and goes to manual entry.
//
// It also scores the agent against an answer key (ADR 0020):
//
//	addepar score -config FILE -inputs DIR -key FILE -map FILE [-json]
//
// runs every PDF in DIR, one at a time, on the configuration (a one_shot
// trigger; use a fresh store to score a model or prompt change), and
// compares each statement's verified records with the answer key, the
// upload saved as CSV in the exporter's columns, by the mapping of each
// statement's positions to answer rows. It prints the report, which holds
// values and names, and exits 0 when nothing wrong came out, 1 on a
// mismatch, a hallucination, an error or an input not run, and 2 on a usage
// error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"

	"github.com/SnapdragonPartners/maestro-agents/agent"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/standalone"
)

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// maxPDFBytes bounds the statement read into memory.
const maxPDFBytes = 64 << 20

func main() {
	p := program(nil)
	if hostCommand(os.Args[1:]) {
		standalone.Main(p)
		return
	}
	os.Exit(command(p, os.Args[1:]))
}

// command runs one of addepar's own commands under the host's signal
// lifecycle: the first SIGINT or SIGTERM shuts a score run down at its next
// step boundary, a second ends the process (ADR 0017, item 21).
func command(p standalone.Program, args []string) int { //nolint:gocritic // hugeParam: called once
	ctx, stop := standalone.SignalContext(context.Background())
	defer stop()
	return run(ctx, p, args, os.Stdout, os.Stderr)
}

// hostCommand reports whether args name one of the standalone host's
// commands, run and config validate; every other invocation, a bad one
// included, is addepar's own, so its usage lists every command.
func hostCommand(args []string) bool {
	switch {
	case len(args) >= 1 && args[0] == "run":
		return true
	case len(args) >= 2 && args[0] == "config" && args[1] == "validate":
		return true
	}
	return false
}

// usage lists every addepar command.
const usage = `usage:
  addepar run -config FILE [-input FILE]
  addepar config validate -config FILE
  addepar verify -pdf FILE -record FILE [-override-by NAME -override-reason TEXT]
  addepar score -config FILE -inputs DIR -key FILE -map FILE [-json]
`

// run runs addepar's own commands, verify and score; p is the program
// score runs the agent with.
func run(ctx context.Context, p standalone.Program, args []string, stdout, stderr io.Writer) int { //nolint:gocritic // hugeParam: called once
	switch {
	case len(args) > 0 && args[0] == "verify":
		return verifyCommand(ctx, args[1:], stdout, stderr)
	case len(args) > 0 && args[0] == "score":
		return scoreCommand(ctx, p, args[1:], stdout, stderr)
	}
	say(stderr, usage)
	return exitUsage
}

func verifyCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pdfPath := fs.String("pdf", "", "the statement PDF")
	recPath := fs.String("record", "", "the extraction record (JSON)")
	ovBy := fs.String("override-by", "", "the operator accepting reading a copy-restricted PDF's text")
	ovReason := fs.String("override-reason", "", "why the operator accepts it")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *pdfPath == "" || *recPath == "" || fs.NArg() != 0 {
		say(stderr, "verify: -pdf and -record are required\n")
		return exitUsage
	}
	var ov *document.Override
	if *ovBy != "" || *ovReason != "" {
		ov = &document.Override{By: *ovBy, Reason: *ovReason, At: time.Now().UTC()}
	}
	res, err := verifyFiles(ctx, *pdfPath, *recPath, ov)
	if err != nil {
		say(stderr, "verify: %v\n", err)
		return exitUsage
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		say(stderr, "verify: %v\n", err)
		return exitUsage
	}
	if res.Verdict == verify.Fail {
		return exitFail
	}
	return exitOK
}

func verifyFiles(ctx context.Context, pdfPath, recPath string, ov *document.Override) (*verify.Result, error) {
	pdf, err := readCapped(pdfPath, maxPDFBytes)
	if err != nil {
		return nil, fmt.Errorf("pdf: %w", err)
	}
	rf, err := os.Open(recPath) //nolint:gosec // the operator names the file
	if err != nil {
		return nil, fmt.Errorf("open record: %w", err)
	}
	defer rf.Close() //nolint:errcheck // read-only
	rec, err := statement.Decode(io.LimitReader(rf, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	doc, err := document.Load(ctx, filepath.Base(pdfPath), pdf, document.Options{Override: ov})
	if err != nil {
		return nil, fmt.Errorf("pdf: %w", err)
	}
	res := verify.Verify(doc, rec)
	return &res, nil
}

// program is the intake agent's standalone program; providers adds model
// providers to the host's own (tests add a fake).
func program(providers map[llms.ProviderID]standalone.Provider) standalone.Program {
	reg := agent.NewRegistry()
	if err := reg.Register(intake.Type(intake.Options{})); err != nil {
		panic(err) // the agent type's declaration is fixed; a test registers it
	}
	return standalone.Program{Name: "addepar", Registry: reg, Providers: providers}
}

// say writes a message for the operator; a failed write to stderr has
// nowhere to be reported.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

var errTooLarge = errors.New("file too large")

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the file
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s is over %d bytes", errTooLarge, path, limit)
	}
	return b, nil
}
