// Command addepar is the capital-statement intake agent: one PDF statement
// in, a verified valuation record and an Addepar import row out (docs/MVP.md).
//
// This first slice has no model and no store. Its one command verifies a
// hand-written extraction record against its statement:
//
//	addepar verify -pdf statement.pdf -record record.json
//
// It prints the seven checks' result as JSON and exits 0 when the record has
// no error (warnings still need a person's acceptance), 1 when it has one,
// and 2 on a usage or input error. A copy-restricted PDF's text is read only
// with -override-by and -override-reason, the operator's recorded acceptance;
// without them the record fails grounding and goes to manual entry.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "verify" {
		say(stderr, "usage: addepar verify -pdf FILE -record FILE [-override-by NAME -override-reason TEXT]\n")
		return exitUsage
	}
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pdfPath := fs.String("pdf", "", "the statement PDF")
	recPath := fs.String("record", "", "the extraction record (JSON)")
	ovBy := fs.String("override-by", "", "the operator accepting reading a copy-restricted PDF's text")
	ovReason := fs.String("override-reason", "", "why the operator accepts it")
	if err := fs.Parse(args[1:]); err != nil {
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
		return nil, err
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

// say writes a message for the operator; a failed write to stderr has
// nowhere to be reported.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

var errTooLarge = errors.New("file too large")

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the file
	if err != nil {
		return nil, fmt.Errorf("open pdf: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read pdf: %w", err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: %s is over %d bytes", errTooLarge, path, limit)
	}
	return b, nil
}
