package score

import (
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/verify"
	"github.com/SnapdragonPartners/maestro-agents/internal/strictjson"
)

// Mapping pairs each statement's positions with answer rows. It is written
// by hand, a stopgap until an LLM judge pairs records with rows (ADR 0020,
// items 4 and 7).
type Mapping struct {
	Statements []Statement `json:"statements"`
}

// Statement is one statement's positions, by the SHA-256 of its bytes.
type Statement struct {
	SHA256 string `json:"sha256"`
	// Name is a label for the person reading the file; it is not used.
	Name      string     `json:"name,omitempty"`
	Positions []Position `json:"positions"`
}

// Position is an investor's interest in a fund as the statement prints
// it, and the answer row for it.
type Position struct {
	Investor string `json:"investor"`
	Fund     string `json:"fund"`
	OwnerID  string `json:"owner_id"`
	AssetID  string `json:"asset_id"`
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadMapping reads a mapping strictly: no unknown or case-mismatched
// field, no key repeated in any object (encoding/json keeps the last, so a
// repeated "statements" could erase the mapping), no trailing data, no statement twice, every field of a position set, and no two
// positions of a statement with the same names or the same row.
func LoadMapping(r io.Reader) (*Mapping, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: mapping: %w", ErrInvalid, err)
	}
	var m Mapping
	if err := strictjson.Decode(data, &m); err != nil {
		return nil, fmt.Errorf("%w: mapping: %w", ErrInvalid, err)
	}
	if m.Statements == nil {
		return nil, fmt.Errorf("%w: mapping: no statements", ErrInvalid)
	}
	seen := make(map[string]bool)
	for i := range m.Statements {
		s := &m.Statements[i]
		if !shaRE.MatchString(s.SHA256) {
			return nil, fmt.Errorf("%w: mapping: statement %d: sha256 %q is not 64 lower-case hex digits", ErrInvalid, i+1, s.SHA256)
		}
		if seen[s.SHA256] {
			return nil, fmt.Errorf("%w: mapping: statement %s appears twice", ErrInvalid, s.SHA256)
		}
		seen[s.SHA256] = true
		if err := checkPositions(s); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// checkPositions checks a statement's positions: a list (empty expects no
// record), every field set, no names or row twice.
func checkPositions(s *Statement) error {
	if s.Positions == nil {
		return fmt.Errorf("%w: mapping: statement %s has no positions list (an empty list expects no record)", ErrInvalid, s.SHA256)
	}
	names := make(map[[2]string]bool)
	rows := make(map[[2]string]bool)
	for j, p := range s.Positions {
		if strings.TrimSpace(p.Investor) == "" || strings.TrimSpace(p.Fund) == "" || p.OwnerID == "" || p.AssetID == "" {
			return fmt.Errorf("%w: mapping: statement %s position %d: investor, fund, owner_id and asset_id are required", ErrInvalid, s.SHA256, j+1)
		}
		n := [2]string{verify.NameKey(p.Investor), verify.NameKey(p.Fund)}
		if names[n] {
			return fmt.Errorf("%w: mapping: statement %s names %s / %s twice", ErrInvalid, s.SHA256, p.Investor, p.Fund)
		}
		names[n] = true
		id := [2]string{p.OwnerID, p.AssetID}
		if rows[id] {
			return fmt.Errorf("%w: mapping: statement %s maps two positions to owner %q, asset %q", ErrInvalid, s.SHA256, p.OwnerID, p.AssetID)
		}
		rows[id] = true
	}
	return nil
}
