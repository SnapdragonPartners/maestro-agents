// Package export writes approved valuation records as an Addepar
// position-valuation CSV and, beside it, the evidence manifest (intake X-1,
// X-2).
//
// The CSV is the import template's 19 columns in order, UTF-8, plain
// decimals and no formulas; the same records always give the same bytes
// (N-3), whatever order they are passed in. The manifest maps each row to
// its document's hash, the lines used, the checks run and every review
// action, and records the CSV's SHA-256, so an exported value can be traced
// from the manifest alone (N-1).
//
// Export is the last gate before a value leaves the system: it refuses a
// record that is not approved, whose approval predates a later review action
// (R-4), that has a verification error, or that has a warning no named
// person accepted.
package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/document"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/statement"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
)

// Columns are the Addepar valuation template's 19 columns, in order (the
// 3/31/2026 upload's "New Import Template", header row 3).
func Columns() []string {
	return []string{
		"Direct Owner Entity ID", "Direct Owner Name", "Direct Owner Model Type",
		"Asset Entity ID", "Asset Name", "Asset Model Type",
		"Asset CUSIP", "Asset ISIN", "Asset SEDOL", "Asset Ticker Symbol",
		"Asset Ownership Type", "Trade Date", "Value", "Units", "Cost Basis",
		"Principal Factor", "Cash Position Name", "Currency", "Comment",
	}
}

// OwnershipType is the template's value for every position in this
// workflow: valuations are imported on value-based assets.
const OwnershipType = "Value Based"

// TradeDateLayout is how Trade Date is written. ISO 8601 pending
// confirmation of the firm's Addepar date setting (an open question in the
// intake requirements); one constant to change.
const TradeDateLayout = time.DateOnly

// ManifestVersion is the manifest's schema version.
const ManifestVersion = 1

// Entity is an Addepar owner or asset as a person entered it (R-3): its
// entity ID, its exact Addepar name, or both.
type Entity struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ReviewKind is the kind of a review action.
type ReviewKind string

// The review kinds the exporter reads. The review UI records others
// (corrections, manual entry); every one counts for the approval's date.
const (
	// AcceptWarning accepts one warning, named by its Finding ID.
	AcceptWarning ReviewKind = "accept_warning"
)

// ReviewAction is one human action on a record: who, when, what and why.
type ReviewAction struct {
	At     time.Time  `json:"at"`
	Kind   ReviewKind `json:"kind"`
	By     string     `json:"by"`
	Reason string     `json:"reason"`
	// Finding is the accepted warning's ID, for AcceptWarning.
	Finding string `json:"finding,omitempty"`
	// Change describes what changed, for a correction or manual entry.
	Change string `json:"change,omitempty"`
}

// Approval is a named person's approval of a record (R-4).
type Approval struct {
	At time.Time `json:"at"`
	By string    `json:"by"`
}

// Source is the document a record came from, as the manifest records it.
type Source struct {
	Override    *document.Override   `json:"override,omitempty"`
	SHA256      string               `json:"sha256"`
	Name        string               `json:"name"`
	Restriction document.Restriction `json:"restriction"`
}

// SourceOf summarizes a loaded document.
func SourceOf(d *document.Document) Source {
	return Source{SHA256: d.SHA256, Name: d.Name, Restriction: d.Restriction, Override: d.Override}
}

// Record is an approved valuation record as the store holds it.
type Record struct {
	Owner    *Entity           `json:"owner,omitempty"`
	Asset    *Entity           `json:"asset,omitempty"`
	Approval *Approval         `json:"approval,omitempty"`
	Record   *statement.Record `json:"record"`
	// ID is the store's identifier for the record.
	ID      string         `json:"id"`
	Source  Source         `json:"source"`
	Result  verify.Result  `json:"result"`
	Reviews []ReviewAction `json:"reviews,omitempty"`
}

// Export is a written CSV and its manifest.
type Export struct {
	CSVSHA256      string
	ManifestSHA256 string
	CSV            []byte
	Manifest       []byte
}

// ErrRefused reports records the exporter will not export; the message
// names each record and why.
var ErrRefused = errors.New("export: refused")

// Build writes the CSV and manifest for recs, exported at at.
func Build(recs []Record, at time.Time) (*Export, error) {
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: no records", ErrRefused)
	}
	var problems []string
	seen := map[string]bool{}
	for i := range recs {
		r := &recs[i]
		if seen[r.ID] {
			problems = append(problems, fmt.Sprintf("record %q: listed twice", r.ID))
			continue
		}
		seen[r.ID] = true
		for _, p := range gate(r) {
			problems = append(problems, fmt.Sprintf("record %q: %s", r.ID, p))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrRefused, strings.Join(problems, "; "))
	}

	ordered := make([]*Record, len(recs))
	for i := range recs {
		ordered[i] = &recs[i]
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := rowOf(ordered[i]), rowOf(ordered[j])
		return less(&a, &b, ordered[i].ID, ordered[j].ID)
	})

	csvBytes, err := writeCSV(ordered)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(csvBytes)
	csvHash := hex.EncodeToString(sum[:])
	man, err := writeManifest(ordered, csvHash, at)
	if err != nil {
		return nil, err
	}
	msum := sha256.Sum256(man)
	return &Export{CSV: csvBytes, CSVSHA256: csvHash, Manifest: man, ManifestSHA256: hex.EncodeToString(msum[:])}, nil
}

// gate returns why a record cannot be exported, or nothing.
func gate(r *Record) []string {
	var out []string
	if r.ID == "" {
		out = append(out, "has no ID")
	}
	if r.Record == nil {
		return append(out, "has no extraction record")
	}
	if err := r.Record.Validate(); err != nil {
		return append(out, err.Error())
	}
	out = append(out, gateVerified(r)...)
	out = append(out, gateApproved(r)...)
	for _, cell := range textCells(r) {
		if unsafeCell(cell) {
			out = append(out, fmt.Sprintf("cell %q would be read as a formula", cell))
		}
	}
	return out
}

// gateVerified requires a verification with no error and every warning
// accepted by a named person. It reads the recorded outcome as well as the
// findings: a result whose verdict or any check's status is not pass or warn,
// or that does not hold each of the seven checks once in order, is refused
// even when its findings are empty.
func gateVerified(r *Record) []string {
	if len(r.Result.Checks) == 0 {
		return []string{"was never verified"}
	}
	out := gateOutcome(&r.Result)
	errs := r.Result.Findings(verify.Error)
	for _, f := range errs {
		out = append(out, fmt.Sprintf("verification error %s", f.ID()))
	}
	accepted := map[string]bool{}
	for _, a := range r.Reviews {
		if a.Kind == AcceptWarning && strings.TrimSpace(a.By) != "" {
			accepted[a.Finding] = true
		}
	}
	for _, f := range r.Result.Findings(verify.Warning) {
		if !accepted[f.ID()] {
			out = append(out, fmt.Sprintf("warning %s not accepted", f.ID()))
		}
	}
	return out
}

// gateOutcome checks the recorded verdict and check statuses.
func gateOutcome(res *verify.Result) []string {
	var out []string
	if res.Verdict != verify.Pass && res.Verdict != verify.Warn {
		out = append(out, fmt.Sprintf("verification verdict is %q", res.Verdict))
	}
	if len(res.Checks) != verify.CheckCount {
		return append(out, fmt.Sprintf("verification holds %d checks, not %d", len(res.Checks), verify.CheckCount))
	}
	for i, c := range res.Checks {
		if c.Check != i+1 {
			out = append(out, fmt.Sprintf("verification check %d is out of place", c.Check))
		}
		if c.Status != verify.Pass && c.Status != verify.Warn {
			out = append(out, fmt.Sprintf("check %d %s is %q", c.Check, c.Name, c.Status))
		}
	}
	return out
}

// gateApproved requires a named person's approval that no later review
// action voided (R-4).
func gateApproved(r *Record) []string {
	if r.Approval == nil || strings.TrimSpace(r.Approval.By) == "" || r.Approval.At.IsZero() {
		return []string{"not approved by a named person"}
	}
	for _, a := range r.Reviews {
		if a.At.After(r.Approval.At) {
			return []string{fmt.Sprintf("approval at %s is void: a %s action followed at %s",
				r.Approval.At.UTC().Format(time.RFC3339), a.Kind, a.At.UTC().Format(time.RFC3339))}
		}
	}
	return nil
}

// row is a record's CSV cells.
type row [19]string

func rowOf(r *Record) row {
	var c row
	owner, asset := r.Record.Investor.Text, r.Record.Fund.Text
	if r.Owner != nil {
		c[0] = r.Owner.ID
		if r.Owner.Name != "" {
			owner = r.Owner.Name
		}
	}
	if r.Asset != nil {
		c[3] = r.Asset.ID
		if r.Asset.Name != "" {
			asset = r.Asset.Name
		}
	}
	c[1], c[4] = owner, asset
	c[10] = OwnershipType
	end, _ := r.Record.Period.EndDate() // validated by gate
	c[11] = end.Format(TradeDateLayout)
	c[12] = r.Record.Ending.Amount.String()
	c[17] = r.Record.Currency.Code
	return c
}

func textCells(r *Record) []string {
	c := rowOf(r)
	return []string{c[0], c[1], c[3], c[4], c[17]}
}

// unsafeCell reports a text cell a spreadsheet would evaluate. The value
// cell is a number and may be negative; text cells may not start with a
// formula character (X-1: no formulas).
func unsafeCell(s string) bool {
	return s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0]))
}

// namesOnly reports whether a row identifies its owner or asset by name only:
// a names-only asset may create a new asset on upload (R-3).
func namesOnly(r *Record) bool {
	return r.Owner == nil || r.Owner.ID == "" || r.Asset == nil || r.Asset.ID == ""
}

func less(a, b *row, ida, idb string) bool {
	for _, k := range []int{1, 0, 4, 3, 11, 17} {
		if a[k] != b[k] {
			return a[k] < b[k]
		}
	}
	return ida < idb
}

func writeCSV(recs []*Record) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	w.UseCRLF = true
	if err := w.Write(Columns()); err != nil {
		return nil, fmt.Errorf("export: csv: %w", err)
	}
	for _, r := range recs {
		c := rowOf(r)
		if err := w.Write(c[:]); err != nil {
			return nil, fmt.Errorf("export: csv: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return nil, fmt.Errorf("export: csv: %w", err)
	}
	return b.Bytes(), nil
}

// Manifest is the evidence manifest written beside a CSV.
type Manifest struct {
	ExportedAt time.Time     `json:"exported_at"`
	CSVSHA256  string        `json:"csv_sha256"`
	Rows       []ManifestRow `json:"rows"`
	Version    int           `json:"version"`
}

// ManifestRow traces one CSV row to its evidence.
type ManifestRow struct {
	Approval *Approval `json:"approval"`
	Owner    *Entity   `json:"owner,omitempty"`
	Asset    *Entity   `json:"asset,omitempty"`
	// Evidence is the extraction record exported: the lines used, each
	// with its label, amount and page.
	Evidence *statement.Record `json:"evidence"`
	RecordID string            `json:"record_id"`
	// ExtractionSHA256 is the SHA-256 of Evidence's canonical JSON.
	ExtractionSHA256 string               `json:"extraction_sha256"`
	TradeDate        string               `json:"trade_date"`
	Value            string               `json:"value"`
	Currency         string               `json:"currency"`
	Source           Source               `json:"source"`
	Checks           []verify.CheckResult `json:"checks"`
	Reviews          []ReviewAction       `json:"reviews"`
	Row              int                  `json:"row"`
	Page             int                  `json:"page"`
	NamesOnly        bool                 `json:"names_only"`
}

func writeManifest(recs []*Record, csvHash string, at time.Time) ([]byte, error) {
	m := Manifest{Version: ManifestVersion, ExportedAt: at.UTC(), CSVSHA256: csvHash, Rows: make([]ManifestRow, len(recs))}
	for i, r := range recs {
		ext, err := json.Marshal(r.Record)
		if err != nil {
			return nil, fmt.Errorf("export: manifest: %w", err)
		}
		sum := sha256.Sum256(ext)
		c := rowOf(r)
		reviews := r.Reviews
		if reviews == nil {
			reviews = []ReviewAction{}
		}
		m.Rows[i] = ManifestRow{
			Row:              i + 1,
			RecordID:         r.ID,
			Source:           r.Source,
			TradeDate:        c[11],
			Value:            c[12],
			Currency:         c[17],
			Owner:            r.Owner,
			Asset:            r.Asset,
			NamesOnly:        namesOnly(r),
			Page:             r.Record.Ending.Page,
			Evidence:         r.Record,
			ExtractionSHA256: hex.EncodeToString(sum[:]),
			Checks:           r.Result.Checks,
			Reviews:          reviews,
			Approval:         r.Approval,
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("export: manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// Write writes the CSV as <name>.csv and the manifest as
// <name>.manifest.json in dir, each written to a temporary file and then
// linked into place, and refuses to replace an existing export.
func (e *Export) Write(dir, name string) (csvPath, manifestPath string, err error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", "", fmt.Errorf("export: bad export name %q", name)
	}
	csvPath = filepath.Join(dir, name+".csv")
	manifestPath = filepath.Join(dir, name+".manifest.json")
	for _, p := range []string{csvPath, manifestPath} {
		if _, err := os.Lstat(p); err == nil {
			return "", "", fmt.Errorf("export: %s already exists", p)
		}
	}
	// The manifest goes first: a manifest without its CSV is harmless, a CSV
	// without its evidence is not.
	if err := writeAtomic(dir, manifestPath, e.Manifest); err != nil {
		return "", "", err
	}
	if err := writeAtomic(dir, csvPath, e.CSV); err != nil {
		return "", "", err
	}
	return csvPath, manifestPath, nil
}

func writeAtomic(dir, path string, data []byte) error {
	f, err := os.CreateTemp(dir, ".export-*")
	if err != nil {
		return fmt.Errorf("export: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) //nolint:errcheck // gone after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck,gosec // the write error is the one reported
		return fmt.Errorf("export: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck,gosec // the sync error is the one reported
		return fmt.Errorf("export: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	// Link rather than rename, so an export that appeared meanwhile is never
	// replaced.
	if err := os.Link(tmp, path); err != nil {
		return fmt.Errorf("export: %w", err)
	}
	return nil
}
