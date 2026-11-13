package document_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-cms/extract/pdf"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
)

func TestParseInfo(t *testing.T) {
	cases := []struct {
		name, out  string
		pages      int
		restricted bool
		encrypted  bool
	}{
		{name: "plain", out: "Title: x\nPages:           1\nEncrypted:       no\nPage size: 612 x 792 pts", pages: 1},
		{name: "copy forbidden", out: "Pages: 1\nEncrypted:       yes (print:yes copy:no change:no addNotes:no algorithm:AES)\n", pages: 1, restricted: true, encrypted: true},
		{name: "encrypted, copy allowed", out: "Pages: 3\nEncrypted: yes (print:yes copy:yes change:no addNotes:yes algorithm:AES-256)\n", pages: 3, encrypted: true},
		{name: "encrypted, permissions unstated", out: "Pages: 2\nEncrypted: yes\n", pages: 2, restricted: true, encrypted: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inf, err := document.ParseInfo(c.out)
			if err != nil {
				t.Fatal(err)
			}
			if inf.Pages != c.pages || inf.Restriction.CopyRestricted() != c.restricted || inf.Restriction.Encrypted != c.encrypted {
				t.Fatalf("%+v", inf)
			}
		})
	}
	for _, bad := range []string{"", "Pages: 1\n", "Encrypted: no\n", "Pages: x\nEncrypted: no", "Pages: 0\nEncrypted: no", "Pages: -1\nEncrypted: no", "Pages: 1\nEncrypted: maybe"} {
		if _, err := document.ParseInfo(bad); !errors.Is(err, document.ErrInfoOutput) {
			t.Errorf("ParseInfo(%q) err = %v", bad, err)
		}
	}
}

type fakeInfo struct{ info document.Info }

func (f fakeInfo) Probe(context.Context, []byte) (document.Info, error) { return f.info, nil }

type fakeText struct {
	pages []pdf.Page
	calls *int
}

func (f fakeText) Pages(context.Context, []byte) ([]pdf.Page, error) {
	*f.calls++
	return f.pages, nil
}

var statementText = strings.Repeat("Opening Balance 1,000 Ending Balance 2,000 ", 3)

func load(t *testing.T, info document.Info, pages []string, ov *document.Override) (*document.Document, int, error) {
	t.Helper()
	calls := 0
	ps := make([]pdf.Page, len(pages))
	for i, p := range pages {
		ps[i] = pdf.Page{Number: i + 1, Text: p}
	}
	doc, err := document.Load(context.Background(), "s.pdf", []byte("%PDF-fake"), document.Options{
		Info: fakeInfo{info}, Text: fakeText{pages: ps, calls: &calls}, Override: ov,
	})
	return doc, calls, err
}

func TestLoadTextLayer(t *testing.T) {
	doc, _, err := load(t, document.Info{Pages: 2, Restriction: document.Restriction{CopyAllowed: true}}, []string{statementText, statementText}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc.TextLayer != document.TextUsable || doc.SHA256 != document.Hash([]byte("%PDF-fake")) || len(doc.SHA256) != 64 {
		t.Fatalf("%+v", doc)
	}
	doc, _, _ = load(t, document.Info{Pages: 2, Restriction: document.Restriction{CopyAllowed: true}}, []string{statementText, " 2 "}, nil)
	if doc.TextLayer != document.TextPartial || doc.Pages[1].Usable {
		t.Fatalf("partial: %+v", doc)
	}
	doc, _, _ = load(t, document.Info{Pages: 1, Restriction: document.Restriction{CopyAllowed: true}}, []string{"  \n 1 \n"}, nil)
	if doc.TextLayer != document.TextNone {
		t.Fatalf("scan: %+v", doc)
	}
	if _, _, err := load(t, document.Info{Pages: 3, Restriction: document.Restriction{CopyAllowed: true}}, []string{statementText}, nil); !errors.Is(err, document.ErrPageCount) {
		t.Fatalf("page count mismatch: %v", err)
	}
}

func TestLoadCopyRestricted(t *testing.T) {
	restricted := document.Info{Pages: 1, Restriction: document.Restriction{Encrypted: true, Report: "yes (copy:no)"}}
	doc, calls, err := load(t, restricted, []string{statementText}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || doc.Pages != nil || doc.TextLayer != document.TextNotRead {
		t.Fatalf("text read without an override: calls %d, %+v", calls, doc)
	}
	ov := &document.Override{By: "operator one", At: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), Reason: "our own statement"}
	doc, calls, err = load(t, restricted, []string{statementText}, ov)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || doc.TextLayer != document.TextUsable || doc.Override == nil || doc.Override == ov {
		t.Fatalf("override: calls %d, %+v", calls, doc)
	}
	for _, bad := range []*document.Override{
		{At: ov.At, Reason: "r"},
		{By: "x", Reason: "r"},
		{By: "x", At: ov.At, Reason: "  "},
	} {
		if _, _, err := load(t, restricted, []string{statementText}, bad); err == nil {
			t.Errorf("incomplete override %+v accepted", bad)
		}
	}
	// An override on an unrestricted document is not recorded: nothing was
	// overridden.
	doc, _, _ = load(t, document.Info{Pages: 1, Restriction: document.Restriction{CopyAllowed: true}}, []string{statementText}, ov)
	if doc.Override != nil {
		t.Fatal("override recorded on an unrestricted document")
	}
}

func TestPage(t *testing.T) {
	doc, _, _ := load(t, document.Info{Pages: 1, Restriction: document.Restriction{CopyAllowed: true}}, []string{statementText}, nil)
	if _, ok := doc.Page(0); ok {
		t.Fatal("page 0")
	}
	if _, ok := doc.Page(2); ok {
		t.Fatal("page 2 of 1")
	}
	if p, ok := doc.Page(1); !ok || p.Number != 1 {
		t.Fatal("page 1")
	}
}

func TestPdfinfoUnavailable(t *testing.T) {
	p := &document.Pdfinfo{Binary: "no-such-pdfinfo-binary", Timeout: time.Second}
	if _, err := p.Probe(context.Background(), []byte("%PDF")); !errors.Is(err, document.ErrPdfinfoUnavailable) {
		t.Fatalf("err = %v", err)
	}
	_, err := document.Load(context.Background(), "x.pdf", []byte("%PDF"), document.Options{Info: p})
	if !errors.Is(err, document.ErrPdfinfoUnavailable) {
		t.Fatalf("Load err = %v", err)
	}
}

// TestPdfinfoZeroValue pins the documented defaults: a zero Pdfinfo looks up
// "pdfinfo" on PATH and runs it with the default timeout, not an expired one.
func TestPdfinfoZeroValue(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf 'Pages: 1\\nEncrypted: no\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "pdfinfo"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	inf, err := (&document.Pdfinfo{}).Probe(context.Background(), []byte("%PDF"))
	if err != nil || inf.Pages != 1 || inf.Restriction.Encrypted {
		t.Fatalf("zero-value probe: %+v %v", inf, err)
	}
	t.Setenv("PATH", t.TempDir())
	_, err = (&document.Pdfinfo{}).Probe(context.Background(), []byte("%PDF"))
	if !errors.Is(err, document.ErrPdfinfoUnavailable) || !strings.Contains(err.Error(), "pdfinfo") {
		t.Fatalf("missing binary: %v", err)
	}
}
