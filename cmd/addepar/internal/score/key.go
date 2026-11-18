// Package score scores the intake agent against an answer key (ADR 0020):
// the operators' upload for a quarter, as a CSV in the exporter's columns,
// and a hand-written mapping from each statement's positions to the answer
// rows. Its Comparator plugs into the library's scoring harness.
package score

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/export"
)

// ErrInvalid wraps every answer-key and mapping error.
var ErrInvalid = errors.New("score: invalid")

// The answer key's columns, by index into export.Columns().
const (
	colOwnerID   = 0
	colOwnerName = 1
	colAssetID   = 3
	colAssetName = 4
	colDate      = 11
	colValue     = 12
	colCurrency  = 17
)

// Row is one answer: a position's value on a date.
type Row struct {
	Value     decimal.Decimal
	OwnerID   string
	OwnerName string
	AssetID   string
	AssetName string
	// Date is the trade date, YYYY-MM-DD.
	Date     string
	Currency string
	// Line is the line in the CSV where the row starts, counted from 1.
	Line int
}

// String is the row's answer as the report shows it.
func (r *Row) String() string { return fmt.Sprintf("%s %s %s", r.Date, r.Value, r.Currency) }

// Name is the row's position as the report names it.
func (r *Row) Name() string { return r.OwnerName + " / " + r.AssetName }

// Key is an answer key: its rows, by owner and asset entity IDs.
type Key struct {
	rows map[[2]string][]*Row
}

// Lookup returns the one row for the owner and asset IDs.
func (k *Key) Lookup(ownerID, assetID string) (*Row, error) {
	rows := k.rows[[2]string{ownerID, assetID}]
	switch len(rows) {
	case 0:
		return nil, fmt.Errorf("%w: the answer key has no row for owner %q, asset %q", ErrInvalid, ownerID, assetID)
	case 1:
		return rows[0], nil
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = strconv.Itoa(r.Line)
	}
	return nil, fmt.Errorf("%w: the answer key has %d rows for owner %q, asset %q (lines %s)",
		ErrInvalid, len(rows), ownerID, assetID, strings.Join(lines, ", "))
}

// groupedRE is a decimal with thousands commas.
var groupedRE = regexp.MustCompile(`^-?\d{1,3}(,\d{3})+(\.\d+)?$`)

// LoadKey reads an answer key: a CSV in the exporter's 19 columns, whose
// header row is the first row equal to export.Columns(); rows before it
// (the template's title and help rows) are ignored, and so are rows after
// it whose every cell is blank (ADR 0020, item 7).
func LoadKey(r io.Reader) (*Key, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header := export.Columns()
	k := &Key{rows: make(map[[2]string][]*Row)}
	found := false
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: answer key: %w", ErrInvalid, err)
		}
		if !found {
			found = slices.Equal(trimAll(rec), header)
			continue
		}
		if blank(rec) {
			continue
		}
		line, _ := cr.FieldPos(0)
		row, err := parseRow(rec, line)
		if err != nil {
			return nil, fmt.Errorf("%w: answer key line %d: %w", ErrInvalid, line, err)
		}
		id := [2]string{row.OwnerID, row.AssetID}
		k.rows[id] = append(k.rows[id], row)
	}
	if !found {
		return nil, fmt.Errorf("%w: answer key has no header row with the template's 19 columns", ErrInvalid)
	}
	return k, nil
}

func parseRow(rec []string, line int) (*Row, error) {
	if len(rec) != len(export.Columns()) {
		return nil, fmt.Errorf("%d cells, want %d", len(rec), len(export.Columns()))
	}
	rec = trimAll(rec)
	row := &Row{
		OwnerID: rec[colOwnerID], OwnerName: rec[colOwnerName],
		AssetID: rec[colAssetID], AssetName: rec[colAssetName],
		Currency: rec[colCurrency], Line: line,
	}
	date, err := parseDate(rec[colDate])
	if err != nil {
		return nil, err
	}
	row.Date = date
	v := rec[colValue]
	if groupedRE.MatchString(v) {
		v = strings.ReplaceAll(v, ",", "")
	}
	if row.Value, err = decimal.Parse(v); err != nil {
		return nil, fmt.Errorf("value %q: %w", rec[colValue], err)
	}
	return row, nil
}

// parseDate reads a trade date as YYYY-MM-DD, or as M/D/YYYY, which is
// how Excel writes a date cell to CSV.
func parseDate(s string) (string, error) {
	for _, layout := range []string{time.DateOnly, "1/2/2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(time.DateOnly), nil
		}
	}
	return "", fmt.Errorf("trade date %q is neither YYYY-MM-DD nor M/D/YYYY", s)
}

func trimAll(rec []string) []string {
	out := make([]string, len(rec))
	for i, c := range rec {
		out[i] = strings.TrimSpace(strings.TrimPrefix(c, "\xef\xbb\xbf"))
	}
	return out
}

func blank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
