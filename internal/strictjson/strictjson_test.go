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
