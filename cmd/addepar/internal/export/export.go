// Package export writes approved valuation records as an Addepar
// position-valuation CSV, into the operator's exports folder, and its
// evidence manifest, into the workspace's hidden manifests folder, named by
// the CSV's SHA-256 (intake X-1, X-2; ADR 0021, item 11).
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
// person accepted since the latest correction or manual entry.
package export

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// ManifestVersion is the manifest's schema version: 2 adds each row's
// origin and snapshot hash (ADR 0021).
const ManifestVersion = 2

// Entity is an Addepar owner or asset as a person entered it (R-3): its
// entity ID, its exact Addepar name, or both.
type Entity struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ReviewKind is the kind of a review action.
type ReviewKind string

// The review kinds (ADR 0021, item 7). Every one counts for the
// approval's date (R-4); an approval is not a review action but the
// record's Approval.
const (
	// AcceptWarning accepts one warning, named by its Finding ID.
	AcceptWarning ReviewKind = "accept_warning"
	// Correct replaces the extraction record with a person's correction,
	// verified again. Acceptances before it no longer count.
	Correct ReviewKind = "correct"
	// Manual replaces it with a record a person entered (Origin manual).
	// Acceptances before it no longer count.
	Manual ReviewKind = "manual"
	// SetEntities sets or clears the Addepar owner and asset (R-3).
	SetEntities ReviewKind = "set_entities"
)

// The origins of a record's extraction (ADR 0021, items 7 and 10).
const (
	// OriginExtracted: the extractor's record, as submitted or corrected.
	// Empty means extracted.
	OriginExtracted = "extracted"
	// OriginManual: a record a person entered; its failed checks are
	// downgraded to warnings.
	OriginManual = "manual"
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
	ID string `json:"id"`
	// Origin is OriginManual for a record a person entered; empty or
	// OriginExtracted otherwise.
	Origin  string         `json:"origin,omitempty"`
	Source  Source         `json:"source"`
	Result  verify.Result  `json:"result"`
	Reviews []ReviewAction `json:"reviews,omitempty"`
}

// Export is a written CSV and its manifest.
type Export struct {
	// At is when it was exported, as the manifest records it.
	At             time.Time `json:"at"`
	CSVSHA256      string    `json:"csv_sha256"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	CSV            []byte    `json:"csv"`
	Manifest       []byte    `json:"manifest"`
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
	return &Export{At: at.UTC(), CSV: csvBytes, CSVSHA256: csvHash, Manifest: man, ManifestSHA256: hex.EncodeToString(msum[:])}, nil
}

// Gate returns every reason the exporter would refuse r, or nothing: the
// one gate, which an approval also passes (ADR 0021, item 7).
func Gate(r *Record) []string { return gate(r) }

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
	switch r.Origin {
	case "", OriginExtracted, OriginManual:
	default:
		out = append(out, fmt.Sprintf("has unknown origin %q", r.Origin))
	}
	out = append(out, gateReviews(r)...)
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
	// An acceptance counts only after the latest correction or manual
	// entry: those rerun verification, and their warnings are new findings
	// even when an ID repeats (ADR 0021, item 8).
	accepted := map[string]bool{}
	for _, a := range r.Reviews {
		switch {
		case a.Kind == Correct || a.Kind == Manual:
			clear(accepted)
		case a.Kind == AcceptWarning && reviewFault(a) == "":
			accepted[a.Finding] = true
		}
	}
	for _, f := range r.Result.Findings(verify.Warning) {
		if f.Downgraded && r.Origin != OriginManual {
			// Only a person's record has its errors downgraded (ADR 0021,
			// item 10); on any other it is an error.
			out = append(out, fmt.Sprintf("verification error %s is downgraded on a record a person did not enter", f.ID()))
			continue
		}
		if !accepted[f.ID()] {
			out = append(out, fmt.Sprintf("warning %s not accepted", f.ID()))
		}
	}
	return out
}

// gateReviews refuses a review action that does not record who, when and
// why, as an override must (document.Override.Validate): one with no time
// cannot be ordered against the approval (R-4), and one with no reason
// leaves the manifest with no evidence. An acceptance must name its warning.
func gateReviews(r *Record) []string {
	var out []string
	for i, a := range r.Reviews {
		if fault := reviewFault(a); fault != "" {
			out = append(out, fmt.Sprintf("review action %d %s", i+1, fault))
		}
	}
	return out
}

// reviewFault is the one rule for a complete review action: "" when it
// records its kind, who, when and why, and an acceptance names its warning.
func reviewFault(a ReviewAction) string { //nolint:gocritic // hugeParam: called per action, rarely
	switch {
	case strings.TrimSpace(a.By) == "", a.At.IsZero(), strings.TrimSpace(a.Reason) == "", a.Kind == "":
		return "does not record its kind, who, when and why"
	case a.Kind == AcceptWarning && strings.TrimSpace(a.Finding) == "":
		return "accepts no named warning"
	}
	return ""
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

// Manifest is the evidence manifest of a CSV, written to the hidden
// manifests folder under the CSV's SHA-256 (ADR 0021, item 11).
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
	ExtractionSHA256 string `json:"extraction_sha256"`
	// SnapshotSHA256 is the SHA-256 of the record exported, as JSON: the
	// approval's snapshot, whose hash the review log records (ADR 0021,
	// item 7).
	SnapshotSHA256 string `json:"snapshot_sha256"`
	// Origin is extracted or manual (ADR 0021, item 10).
	Origin    string               `json:"origin"`
	TradeDate string               `json:"trade_date"`
	Value     string               `json:"value"`
	Currency  string               `json:"currency"`
	Source    Source               `json:"source"`
	Checks    []verify.CheckResult `json:"checks"`
	Reviews   []ReviewAction       `json:"reviews"`
	Row       int                  `json:"row"`
	Page      int                  `json:"page"`
	NamesOnly bool                 `json:"names_only"`
}

func writeManifest(recs []*Record, csvHash string, at time.Time) ([]byte, error) {
	m := Manifest{Version: ManifestVersion, ExportedAt: at.UTC(), CSVSHA256: csvHash, Rows: make([]ManifestRow, len(recs))}
	for i, r := range recs {
		ext, err := json.Marshal(r.Record)
		if err != nil {
			return nil, fmt.Errorf("export: manifest: %w", err)
		}
		sum := sha256.Sum256(ext)
		snap, err := SnapshotSHA256(r)
		if err != nil {
			return nil, err
		}
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
			SnapshotSHA256:   snap,
			Origin:           cmp.Or(r.Origin, OriginExtracted),
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

// SnapshotSHA256 is the SHA-256 of r's JSON: an approval's snapshot hash
// (ADR 0021, item 7), which the manifest repeats.
func SnapshotSHA256(r *Record) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("export: snapshot: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// CSVName is the CSV's file name: addepar-<export date>-<first 8 of its
// SHA-256>.csv (ADR 0021, item 11).
func (e *Export) CSVName() string {
	return fmt.Sprintf("addepar-%s-%s.csv", e.At.UTC().Format(time.DateOnly), e.CSVSHA256[:8])
}

// manifestTimeLayout is the export time in a manifest's name, to the
// nanosecond, so two exports of the same CSV bytes in one second still
// get two manifests.
const manifestTimeLayout = "20060102T150405.000000000Z"

// ManifestName is the manifest's file name: <the CSV's SHA-256>-<export
// time, UTC>.json, so a CSV leads to its evidence by its hash alone, and
// a deliberate re-export of the same CSV bytes gets its own manifest (ADR
// 0021, item 11).
func (e *Export) ManifestName() string {
	return e.CSVSHA256 + "-" + e.At.UTC().Format(manifestTimeLayout) + ".json"
}

// Write writes the CSV to csvDir and the manifest to manifestDir, each to
// a temporary file and then linked into place. It never replaces a file:
// an existing one is an error unless it holds exactly these bytes, which
// is kept (a re-export of the same records gives the same CSV, N-3, and
// writing a stored export again after a failed write finds what the
// first attempt left, PR #22). The manifest goes first: a manifest without its CSV is
// harmless, a CSV without its evidence is not.
func (e *Export) Write(csvDir, manifestDir string) (csvPath, manifestPath string, err error) {
	if len(e.CSVSHA256) != sha256.Size*2 {
		return "", "", errors.New("export: the export has no CSV hash")
	}
	csvPath = filepath.Join(csvDir, e.CSVName())
	manifestPath = filepath.Join(manifestDir, e.ManifestName())
	sameMan, err := sameFile(manifestPath, e.ManifestSHA256)
	if err != nil {
		return "", "", err
	}
	sameCSV, err := sameFile(csvPath, e.CSVSHA256)
	if err != nil {
		return "", "", err
	}
	if !sameMan {
		if err := writeAtomic(manifestDir, manifestPath, e.Manifest); err != nil {
			return "", "", err
		}
	}
	if !sameCSV {
		if err := writeAtomic(csvDir, csvPath, e.CSV); err != nil {
			return "", "", err
		}
	}
	return csvPath, manifestPath, nil
}

// sameFile reports whether path is a regular file whose SHA-256 is sum; a
// missing file is false, and any other file there an error.
func sameFile(path, sum string) (bool, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("export: %w", err)
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("export: %s already exists and is not a regular file", path)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the operator's export folder
	if err != nil {
		return false, fmt.Errorf("export: %w", err)
	}
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != sum {
		return false, fmt.Errorf("export: %s already exists with other content", path)
	}
	return true, nil
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
