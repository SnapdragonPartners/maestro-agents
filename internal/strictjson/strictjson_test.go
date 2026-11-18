package strictjson

import (
	"strings"
	"testing"
)

// The full case table runs through the standalone host's decodeStrict
// (standalone/config_test.go); these cases pin the package's own contract.
func TestDecode(t *testing.T) {
	type item struct {
		N int `json:"n"`
	}
	type doc struct {
		Items []item `json:"items"`
	}
	var d doc
	if err := Decode([]byte(`{"items":[{"n":1}]}`), &d); err != nil || len(d.Items) != 1 || d.Items[0].N != 1 {
		t.Fatalf("valid document: %+v, %v", d, err)
	}
	for name, tc := range map[string]struct{ in, want string }{
		"repeated list erases": {`{"items":[{"n":1}],"items":[]}`, `key "items" is repeated`},
		"repeated in an item":  {`{"items":[{"n":1,"n":2}]}`, `key "n" is repeated`},
		"case mismatch":        {`{"Items":[]}`, `unknown field "Items"`},
		"trailing data":        {`{"items":[]} []`, "data after the JSON value"},
	} {
		t.Run(name, func(t *testing.T) {
			var got doc
			if err := Decode([]byte(tc.in), &got); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Embedded structs follow encoding/json: an untagged one's fields are
// promoted by JSON name (even when its Go name shadows them, as
// statement.Line's Amount does), a tagged one is a named field, and the
// strict checks apply inside both (ADR 0021, item 13).
func TestDecodeEmbedded(t *testing.T) {
	type amount struct {
		Amount string `json:"amount"`
		Label  string `json:"label"`
	}
	type line struct {
		Kind string `json:"kind"`
		amount
	}
	type named struct {
		Kind   string `json:"kind"`
		amount `json:"inner"`
	}
	var l line
	if err := Decode([]byte(`{"kind":"m","amount":"1.00","label":"x"}`), &l); err != nil || l.Amount != "1.00" || l.Label != "x" {
		t.Fatalf("promoted fields: %+v, %v", l, err)
	}
	var n named
	if err := Decode([]byte(`{"kind":"m","inner":{"amount":"1.00"}}`), &n); err != nil || n.Amount != "1.00" {
		t.Fatalf("tagged embedded: %+v, %v", n, err)
	}
	for name, tc := range map[string]struct {
		in, want string
		v        any
	}{
		"promoted repeated":      {`{"kind":"m","amount":"1","amount":"2"}`, `key "amount" is repeated`, &line{}},
		"promoted case mismatch": {`{"kind":"m","Amount":"1"}`, `unknown field "Amount"`, &line{}},
		"tagged not promoted":    {`{"kind":"m","amount":"1"}`, `unknown field "amount"`, &named{}},
		"inside tagged repeated": {`{"inner":{"label":"a","label":"b"}}`, `key "label" is repeated`, &named{}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Decode([]byte(tc.in), tc.v); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
