package ui

import (
	"container/list"
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/SnapdragonPartners/maestro-cms/extract/pdf"
	"github.com/SnapdragonPartners/maestro-cms/extract/pdf/pdftoppm"
	"github.com/SnapdragonPartners/maestro-cms/extract/pdf/pdftotext"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/intake"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

// Pages renders a PDF's pages and reads its words' boxes (ADR 0022, item
// 11). key identifies the PDF (its SHA-256) for caching.
type Pages interface {
	Render(ctx context.Context, key string, data []byte, page int) (png []byte, err error)
	Layout(ctx context.Context, key string, data []byte) (*pdf.Layout, error)
}

// The caches' bounds: page images by count (a page is a few hundred KB),
// layouts by documents.
const (
	maxCachedImages  = 64
	maxCachedLayouts = 16
	renderDPI        = 110
	renderMaxPixels  = 1800
)

// popplerPages renders with pdftoppm and reads words with pdftotext,
// through maestro-cms, caching both.
type popplerPages struct {
	renderer *pdftoppm.Renderer
	text     *pdftotext.Engine
	images   *lru[[]byte]
	layouts  *lru[*pdf.Layout]
}

// NewPopplerPages returns Poppler's Pages, with bounded caches.
func NewPopplerPages() Pages {
	return &popplerPages{
		renderer: pdftoppm.New(), text: pdftotext.New(),
		images: newLRU[[]byte](maxCachedImages), layouts: newLRU[*pdf.Layout](maxCachedLayouts),
	}
}

func (p *popplerPages) Render(ctx context.Context, key string, data []byte, page int) ([]byte, error) {
	ck := fmt.Sprintf("%s/%d", key, page)
	if b, ok := p.images.get(ck); ok {
		return b, nil
	}
	img, err := p.renderer.Render(ctx, data, pdftoppm.RenderOptions{Page: page, Format: pdftoppm.PNG, DPI: renderDPI, MaxPixels: renderMaxPixels})
	if err != nil {
		return nil, err //nolint:wrapcheck // maestro-cms names it
	}
	p.images.put(ck, img.Data)
	return img.Data, nil
}

func (p *popplerPages) Layout(ctx context.Context, key string, data []byte) (*pdf.Layout, error) {
	if l, ok := p.layouts.get(key); ok {
		return l, nil
	}
	l, err := p.text.Layout(ctx, data)
	if err != nil {
		return nil, err //nolint:wrapcheck // maestro-cms names it
	}
	p.layouts.put(key, l)
	return l, nil
}

// lru is a small bounded cache, safe for concurrent use.
type lru[V any] struct {
	order *list.List
	items map[string]*list.Element
	mu    sync.Mutex
	max   int
}

type lruItem[V any] struct {
	v   V
	key string
}

func newLRU[V any](n int) *lru[V] {
	return &lru[V]{max: n, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru[V]) get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*lruItem[V]).v, true //nolint:forcetypeassert // only lruItem[V] is stored
	}
	var zero V
	return zero, false
}

func (c *lru[V]) put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		e.Value.(*lruItem[V]).v = v //nolint:forcetypeassert // only lruItem[V] is stored
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&lruItem[V]{key: k, v: v})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*lruItem[V]).key) //nolint:forcetypeassert // only lruItem[V] is stored
	}
}

// FieldView is one of the record's claims with its page: what the record
// page lists and highlights.
type FieldView struct {
	// amount is set for an amount; text is matched otherwise.
	amount decimal.Decimal
	// Key names the field ("ending", "lines.2"), Name says it for a person.
	Key, Name string
	// Value is the claim as recorded, Label an amount's label.
	Value, Label string
	text         string
	Page         int
}

// IsAmount reports whether the field is an amount.
func (f *FieldView) IsAmount() bool { return f.amount.IsSet() }

// fields lists the record's claims that cite a page.
func fields(r *statement.Record) []FieldView {
	if r == nil {
		return nil
	}
	var out []FieldView
	text := func(key, name, v string, page int) {
		if v != "" && page > 0 {
			out = append(out, FieldView{Key: key, Name: name, Value: v, Page: page, text: v})
		}
	}
	amount := func(key, name string, a *statement.Amount) {
		if a == nil {
			return
		}
		f := FieldView{Key: key, Name: name, Label: a.Label, Page: a.Page, amount: a.Amount, text: a.Label}
		if a.Amount.IsSet() {
			f.Value = formatAmount(a.Amount.String())
		}
		out = append(out, f)
	}
	text("investor", "Investor", r.Investor.Text, r.Investor.Page)
	text("fund", "Fund", r.Fund.Text, r.Fund.Page)
	text("period", "Period ("+r.Period.End+")", r.Period.Label, r.Period.Page)
	text("currency", "Currency ("+r.Currency.Code+")", r.Currency.Evidence, r.Currency.Page)
	text("column", "Column", r.Column.Label, r.Column.Page)
	amount("beginning", "Beginning balance", r.Beginning)
	for i := range r.Lines {
		amount(fmt.Sprintf("lines.%d", i+1), "Line "+fmt.Sprint(i+1)+" ("+string(r.Lines[i].Kind)+")", &r.Lines[i].Amount)
	}
	amount("ending", "Ending balance", &r.Ending)
	amount("fund_total", "Fund total", r.FundTotal)
	for i := range r.Repeats {
		amount(fmt.Sprintf("repeats.%d", i+1), "Repeat "+fmt.Sprint(i+1), &r.Repeats[i])
	}
	return out
}

// PageView is one cited page: its image's route and size in points, and
// the boxes to draw on it.
type PageView struct {
	// NoBoxes is why a page without boxes has none: its text was not
	// read, the page has no words (a scanned page), or its words could not
	// be read (logged).
	NoBoxes       string
	Marks         []Mark
	Number        int
	Width, Height float64
	// Boxes reports whether words were read for the page.
	Boxes bool
}

// Why a page shows no boxes.
const (
	noBoxesNotRead = "the document's text was not read"
	noBoxesNoWords = "the page has no text"
	noBoxesFailed  = "the page's words could not be read; the serve log says why"
)

// Mark is a box on a page, in points from its top-left: a field's value
// or label.
type Mark struct {
	Field      string
	Kind       string // "value" or "label"
	X, Y, W, H float64
}

// withoutBoxes gives every page why it has no boxes.
func withoutBoxes(pages []PageView, why string) []PageView {
	for i := range pages {
		pages[i].NoBoxes = why
	}
	return pages
}

// evidencePages returns each page the fields cite, in order, with every
// copy of each field's value, and its label's words, boxed (ADR 0022,
// item 11). Words are read only for a document whose text was read.
func (s *Server) evidencePages(ctx context.Context, sha string, d *intake.Document, fs []FieldView) []PageView {
	nums := map[int]bool{}
	for i := range fs {
		if fs[i].Page > 0 && fs[i].Page <= d.Document.PageCount { // record() loads only a document that is there
			nums[fs[i].Page] = true
		}
	}
	pages := make([]PageView, 0, len(nums))
	for n := range nums {
		pages = append(pages, PageView{Number: n, NoBoxes: noBoxesNoWords})
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].Number < pages[j].Number })
	if len(pages) == 0 {
		return pages
	}
	if len(d.Text) == 0 {
		return withoutBoxes(pages, noBoxesNotRead)
	}
	data, err := s.pdfBytes(ctx, sha)
	if err != nil {
		s.opts.Logger.Warn("ui: cannot read the PDF", "document", sha, "error", err.Error())
		return withoutBoxes(pages, noBoxesFailed)
	}
	layout, err := s.opts.Pages.Layout(ctx, sha, data)
	if err != nil {
		s.opts.Logger.Warn("ui: cannot read the words' boxes", "document", sha, "error", err.Error())
		return withoutBoxes(pages, noBoxesFailed)
	}
	for i := range pages {
		n := pages[i].Number
		if n > len(layout.Pages) {
			continue
		}
		pl := &layout.Pages[n-1]
		pages[i].Width, pages[i].Height = pl.Width, pl.Height
		if len(pl.Words) == 0 {
			continue
		}
		pages[i].Boxes, pages[i].NoBoxes = true, ""
		for j := range fs {
			if fs[j].Page == n {
				pages[i].Marks = append(pages[i].Marks, marks(pl, &fs[j])...)
			}
		}
	}
	return pages
}

// marks boxes a field on its page: an amount's every copy and its label's
// words, or a text's words.
func marks(pl *pdf.PageLayout, f *FieldView) []Mark {
	out := make([]Mark, 0, 4)
	if f.IsAmount() {
		want := f.amount.Abs()
		for _, w := range pl.Words {
			if v, ok := verify.ParseAmount(w.Text); ok && v.Abs().Equal(want) {
				out = append(out, box(f.Key, "value", w.Box))
			}
		}
		for _, r := range phrase(pl.Words, f.text) {
			out = append(out, box(f.Key, "label", r))
		}
		return out
	}
	for _, r := range phrase(pl.Words, f.text) {
		out = append(out, box(f.Key, "value", r))
	}
	return out
}

func box(field, kind string, r pdf.Rect) Mark {
	return Mark{Field: field, Kind: kind, X: r.X0, Y: r.Y0, W: r.X1 - r.X0, H: r.Y1 - r.Y0}
}

// phrase returns the boxes of every run of words that spells text, its
// words compared as the verifier compares them.
func phrase(words []pdf.Word, text string) []pdf.Rect {
	want := verify.Words(text)
	if len(want) == 0 {
		return nil
	}
	norm := make([][]string, len(words))
	for i, w := range words {
		norm[i] = verify.Words(w.Text)
	}
	var out []pdf.Rect
	for i := range words {
		if j, ok := spells(norm, i, want); ok {
			for k := i; k < j; k++ {
				if len(norm[k]) > 0 {
					out = append(out, words[k].Box)
				}
			}
		}
	}
	return out
}

// spells reports whether the words from i on spell want, skipping words
// that normalize to nothing, and where the run ends.
func spells(norm [][]string, i int, want []string) (end int, ok bool) {
	if len(norm[i]) == 0 {
		return 0, false
	}
	k := 0
	j := i
	for ; j < len(norm) && k < len(want); j++ {
		for _, n := range norm[j] {
			if k >= len(want) || n != want[k] {
				return 0, false
			}
			k++
		}
	}
	return j, k == len(want)
}

// pdfBytes reads the document's stored PDF (ADR 0019, item 1).
func (s *Server) pdfBytes(ctx context.Context, sha string) ([]byte, error) {
	b := s.current()
	o, ok, err := b.Store().Object(ctx, b.Namespace(), intake.PDFKey(sha))
	switch {
	case err != nil:
		return nil, err //nolint:wrapcheck // the store names it
	case !ok || o.Bytes == nil:
		return nil, errNotFound
	}
	return o.Bytes, nil
}
