package document

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Info is what a probe reports about a PDF.
type Info struct {
	Restriction Restriction
	Pages       int
}

// InfoProbe reports a PDF's page count and encryption.
type InfoProbe interface {
	Probe(ctx context.Context, data []byte) (Info, error)
}

// ErrPdfinfoUnavailable reports that the pdfinfo binary was not found.
var ErrPdfinfoUnavailable = errors.New("document: pdfinfo not found")

// ErrInfoOutput reports pdfinfo output Load cannot read.
var ErrInfoOutput = errors.New("document: unreadable pdfinfo output")

// Pdfinfo probes a PDF with Poppler's pdfinfo, the PDF on stdin. maestro-cms
// runs pdfinfo too but does not report encryption, so the probe is here. The
// zero value is usable: Probe applies the defaults.
type Pdfinfo struct {
	// Binary is the executable, looked up on PATH; default "pdfinfo".
	Binary string
	// Timeout bounds one run; default 30 seconds.
	Timeout time.Duration
}

// The probe's defaults.
const (
	defaultPdfinfo        = "pdfinfo"
	defaultPdfinfoTimeout = 30 * time.Second
)

// NewPdfinfo returns the default probe.
func NewPdfinfo() *Pdfinfo { return &Pdfinfo{Binary: defaultPdfinfo, Timeout: defaultPdfinfoTimeout} }

// maxInfoOutput caps pdfinfo's stdout; its report is a few hundred bytes.
const maxInfoOutput = 64 << 10

// Probe runs pdfinfo and parses its report.
func (p *Pdfinfo) Probe(ctx context.Context, data []byte) (Info, error) {
	name, timeout := p.Binary, p.Timeout
	if name == "" {
		name = defaultPdfinfo
	}
	if timeout <= 0 {
		timeout = defaultPdfinfoTimeout
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %s", ErrPdfinfoUnavailable, name)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The binary is fixed configuration and the arguments are constant; the
	// PDF goes to stdin, never onto the command line.
	cmd := exec.CommandContext(ctx, bin, "-enc", "UTF-8", "-") //nolint:gosec // fixed binary and flags, PDF on stdin
	cmd.Stdin = bytes.NewReader(data)
	var stdout capped
	var stderr capped
	stdout.max, stderr.max = maxInfoOutput, maxInfoOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return Info{}, fmt.Errorf("document: pdfinfo: %w", ctx.Err())
		}
		return Info{}, fmt.Errorf("document: pdfinfo: %w: %s", err, firstLine(stderr.String()))
	}
	return ParseInfo(stdout.String())
}

// ParseInfo reads pdfinfo's report: the Pages line and the Encrypted line,
// "no" or "yes (print:yes copy:no change:no addNotes:no algorithm:AES)".
func ParseInfo(out string) (Info, error) {
	var inf Info
	sawPages, sawEnc := false, false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Pages":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 { // a PDF has at least one page
				return Info{}, fmt.Errorf("%w: pages %q", ErrInfoOutput, val)
			}
			inf.Pages, sawPages = n, true
		case "Encrypted":
			r, err := parseEncrypted(val)
			if err != nil {
				return Info{}, err
			}
			inf.Restriction, sawEnc = r, true
		}
	}
	if !sawPages || !sawEnc {
		return Info{}, fmt.Errorf("%w: missing Pages or Encrypted line", ErrInfoOutput)
	}
	return inf, nil
}

func parseEncrypted(val string) (Restriction, error) {
	r := Restriction{Report: val}
	if val == "no" {
		r.CopyAllowed = true
		return r, nil
	}
	rest, ok := strings.CutPrefix(val, "yes")
	if !ok {
		return Restriction{}, fmt.Errorf("%w: encrypted %q", ErrInfoOutput, val)
	}
	r.Encrypted = true
	rest = strings.Trim(strings.TrimSpace(rest), "()")
	copyFlag := ""
	for _, f := range strings.Fields(rest) {
		if v, ok := strings.CutPrefix(f, "copy:"); ok {
			copyFlag = v
		}
	}
	switch copyFlag {
	case "yes":
		r.CopyAllowed = true
	case "no":
		r.CopyAllowed = false
	default:
		// An encrypted PDF whose copy permission pdfinfo does not state is
		// treated as restricted: the safe reading of an unknown answer.
		r.CopyAllowed = false
	}
	return r, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// capped is a buffer that keeps at most max bytes and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

var _ io.Writer = (*capped)(nil)
