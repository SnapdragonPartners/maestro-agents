// Package document is the intake agent's document service: it hashes a
// statement PDF, probes its page count and copy restrictions, and reads its
// text layer one page at a time in pdftotext's layout mode, which keeps each
// label on the same line as its numbers (the verification checks depend on
// that pairing).
//
// A copy-restricted PDF's text is read only when an operator has accepted
// reading it (Override, intake I-3 as amended by ADR 0015); without that
// acceptance Load returns the document with no pages and the statement goes
// to manual entry. The service never removes a restriction or re-saves the
// file, and it does no OCR: a PDF with no usable text layer is reported as
// such (TextNone) and goes to manual entry too.
package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/SnapdragonPartners/maestro-cms/extract/pdf"
	"github.com/SnapdragonPartners/maestro-cms/extract/pdf/pdftotext"
)

// TextMode is the text mode every page is read in.
const TextMode = pdf.ModeLayout

// minPageChars is the fewest letters and digits a page needs for its text
// layer to count as usable. A scanned page often carries a few stray glyphs
// (a page number, a stamp) and nothing a check could ground against.
const minPageChars = 40

// TextLayer reports whether the PDF's text can be verified against.
type TextLayer string

// The text-layer states.
const (
	// TextUsable means every page has a usable text layer.
	TextUsable TextLayer = "usable"
	// TextPartial means some pages have one and some do not; a record may
	// cite only the pages that do.
	TextPartial TextLayer = "partial"
	// TextNone means no page has one (a scan): the document goes to manual
	// entry, never to OCR.
	TextNone TextLayer = "none"
	// TextNotRead means the text was not read, because the PDF is
	// copy-restricted and no operator accepted reading it.
	TextNotRead TextLayer = "not_read"
)

// Restriction is what pdfinfo reports about the PDF's encryption.
type Restriction struct {
	// Report is pdfinfo's Encrypted line as printed, for the record.
	Report string `json:"report"`
	// Encrypted is true for a PDF with a security handler, even one that
	// permits everything.
	Encrypted bool `json:"encrypted"`
	// CopyAllowed is false when the PDF forbids copying its text.
	CopyAllowed bool `json:"copy_allowed"`
}

// CopyRestricted reports whether reading the text needs an Override.
func (r Restriction) CopyRestricted() bool { return r.Encrypted && !r.CopyAllowed }

// Override is an operator's recorded acceptance to read a copy-restricted
// PDF's text layer for verification. It is asked for at intake, before any
// run starts; declining sends the document to manual entry.
type Override struct {
	// At is when the operator accepted.
	At time.Time `json:"at"`
	// By names the operator.
	By string `json:"by"`
	// Reason is the operator's stated reason.
	Reason string `json:"reason"`
}

// Validate refuses an override that does not say who, when and why.
func (o *Override) Validate() error {
	switch {
	case strings.TrimSpace(o.By) == "":
		return errors.New("document: override has no operator")
	case o.At.IsZero():
		return errors.New("document: override has no time")
	case strings.TrimSpace(o.Reason) == "":
		return errors.New("document: override has no reason")
	}
	return nil
}

// Page is one page's text in layout mode.
type Page struct {
	// Text is the page's text, with its physical layout kept in spaces.
	Text string `json:"-"`
	// Number is the page's 1-based number.
	Number int `json:"number"`
	// Usable is true when the page has a usable text layer.
	Usable bool `json:"usable"`
}

// Document is a statement PDF as the verifier sees it.
type Document struct {
	// Override is the acceptance Load read a restricted PDF under, or nil.
	Override *Override `json:"override,omitempty"`
	// SHA256 is the hex SHA-256 of the file's bytes.
	SHA256 string `json:"sha256"`
	// Name is the file's display name; never an identifier.
	Name string `json:"name"`
	// TextLayer summarizes the pages' text layers.
	TextLayer TextLayer `json:"text_layer"`
	// Restriction is the PDF's encryption as pdfinfo reports it.
	Restriction Restriction `json:"restriction"`
	// Pages holds each page's text; nil when the text was not read.
	Pages []Page `json:"pages,omitempty"`
	// PageCount is the number of pages pdfinfo reports.
	PageCount int `json:"page_count"`
}

// Page returns page n (1-based), or false when it is out of range or the
// text was not read.
func (d *Document) Page(n int) (Page, bool) {
	if n < 1 || n > len(d.Pages) {
		return Page{}, false
	}
	return d.Pages[n-1], true
}

// TextEngine reads a PDF's text per page. pdftotext's engine with
// WithLayout is the one Load uses by default.
type TextEngine interface {
	Pages(ctx context.Context, data []byte) ([]pdf.Page, error)
}

// Options configures Load.
type Options struct {
	// Override, when set, lets Load read a copy-restricted PDF's text.
	Override *Override
	// Text reads the pages; nil means pdftotext in layout mode.
	Text TextEngine
	// Info probes the PDF; nil means Poppler's pdfinfo.
	Info InfoProbe
}

// ErrPageCount reports pdftotext and pdfinfo disagreeing on the pages.
var ErrPageCount = errors.New("document: page count mismatch")

// Hash returns the hex SHA-256 of data.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Load hashes data, probes it and, unless it is copy-restricted without an
// override, reads its text layer. name is the file's display name.
func Load(ctx context.Context, name string, data []byte, opts Options) (*Document, error) {
	if opts.Override != nil {
		if err := opts.Override.Validate(); err != nil {
			return nil, err
		}
	}
	info := opts.Info
	if info == nil {
		info = NewPdfinfo()
	}
	text := opts.Text
	if text == nil {
		text = pdftotext.New(pdftotext.WithLayout())
	}
	sum := Hash(data)
	inf, err := info.Probe(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("document: probe %s: %w", sum[:12], err)
	}
	doc := &Document{
		SHA256:      sum,
		Name:        name,
		PageCount:   inf.Pages,
		Restriction: inf.Restriction,
	}
	if doc.Restriction.CopyRestricted() {
		if opts.Override == nil {
			doc.TextLayer = TextNotRead
			return doc, nil
		}
		ov := *opts.Override
		doc.Override = &ov
	}
	pages, err := text.Pages(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("document: read text %s: %w", doc.SHA256[:12], err)
	}
	if len(pages) != doc.PageCount {
		return nil, fmt.Errorf("%w: pdftotext read %d, pdfinfo reports %d", ErrPageCount, len(pages), doc.PageCount)
	}
	doc.Pages = make([]Page, len(pages))
	usable := 0
	for i, p := range pages {
		ok := usableText(p.Text)
		if ok {
			usable++
		}
		doc.Pages[i] = Page{Number: i + 1, Text: p.Text, Usable: ok}
	}
	switch {
	case usable == 0:
		doc.TextLayer = TextNone
	case usable < len(pages):
		doc.TextLayer = TextPartial
	default:
		doc.TextLayer = TextUsable
	}
	return doc, nil
}

func usableText(s string) bool {
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
			if n >= minPageChars {
				return true
			}
		}
	}
	return false
}
