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
// no error (warnings still need a person's acceptance), 1 when it has one
// or a signal interrupted it, and 2 on a usage or input error. A copy-restricted PDF's text is read only
// with -override-by and -override-reason, the operator's recorded acceptance;
// without them the record fails grounding and goes to manual entry.
//
// It exports approved records (ADR 0021), each approval's snapshot, as the
// Addepar CSV in -out and its evidence manifest in -manifests (the
// workspace's hidden folder), named by the CSV's SHA-256:
//
//	addepar export -config FILE -out DIR -manifests DIR (-by NAME (-approved | -record ID ...) | -rewrite RUN)
//
// -rewrite writes the files of an export a run already stored, for a write
// that failed after its commit. The configuration's store must not be in
// use by a running host. export exits 0 when it wrote the files or nothing
// was waiting, 1 when the export was refused or failed, and 2 on a usage
// error.
//
// It serves the review UI (ADR 0022), watching the workspace's inbox in the
// same process:
//
//	addepar serve [-workspace DIR] [-addr HOST:PORT] [-open=false]
//
// The workspace (default ~/Documents/Addepar Intake) holds Inbox/, where
// statements are dropped, Exports/, where the CSVs go, and the hidden
// .agent/: the store, the configuration and the manifests. Without a
// configuration the UI opens on its setup page, which writes one; the API
// key goes to the user's configuration folder, never the workspace. serve
// runs on macOS or Linux (the workspace's file store needs a Unix
// system), listens on a loopback address only and prints the link that opens the
// UI, with the session token from ADDEPAR_SESSION_TOKEN or a random one.
// It exits 0 after a signal, 1 when the watch or the server failed, and 2
// on a usage error.
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
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/review"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/regularfile"
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
// lifecycle: the first SIGINT or SIGTERM cancels the command's context (a
// score run shuts down at its next step boundary; verify stops reading the
// PDF and exits 1), a second ends the process (ADR 0017, item 21).
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
  addepar export -config FILE -out DIR -manifests DIR (-by NAME (-approved | -record ID ...) | -rewrite RUN)
  addepar serve [-workspace DIR] [-addr HOST:PORT] [-open=false]
`

// run runs addepar's own commands, verify, score, export and serve; p is
// the program they run their agents with.
func run(ctx context.Context, p standalone.Program, args []string, stdout, stderr io.Writer) int { //nolint:gocritic // hugeParam: called once
	switch {
	case len(args) > 0 && args[0] == "verify":
		return verifyCommand(ctx, args[1:], stdout, stderr)
	case len(args) > 0 && args[0] == "score":
		return scoreCommand(ctx, p, args[1:], stdout, stderr)
	case len(args) > 0 && args[0] == "export":
		return exportCommand(ctx, p, args[1:], stdout, stderr)
	case len(args) > 0 && args[0] == "serve":
		return serveCommand(ctx, p, args[1:], stdout, stderr, defaultServeEnv())
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
	if err != nil && ctx.Err() != nil {
		say(stderr, "verify: interrupted: %v\n", err)
		return exitFail
	}
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
	rf, err := regularfile.Open(recPath, false)
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
	if err := reg.Register(review.Type()); err != nil {
		panic(err) // likewise
	}
	return standalone.Program{Name: "addepar", Registry: reg, Providers: providers, Submitted: []string{review.TypeName}}
}

// say writes a message for the operator; a failed write to stderr has
// nowhere to be reported.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// readCapped reads an operator's file of at most limit bytes, refusing a
// FIFO, device or other non-regular file rather than hanging on it (ADR
// 0017, item 11).
func readCapped(path string, limit int64) ([]byte, error) {
	b, err := regularfile.ReadCapped(path, limit)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return b, nil
}
