package decimal_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SnapdragonPartners/maestro-agents/cmd/addepar/internal/decimal"
)

func TestParseRoundTrip(t *testing.T) {
	for _, s := range []string{"0", "678250", "714760.65", "-15302.17", "0.00", "-0.5", "1171800000", "0.001"} {
		d, err := decimal.Parse(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if d.String() != s {
			t.Errorf("Parse(%q).String() = %q", s, d.String())
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "-", ".", "1.", ".5", "+1", "1e5", "1,000", "$5", " 1", "1 ", "--1", "1.2.3", "(5)", strings.Repeat("9", 41), "NaN", "١٢"} {
		if _, err := decimal.Parse(s); !errors.Is(err, decimal.ErrSyntax) {
			t.Errorf("Parse(%q) err = %v, want ErrSyntax", s, err)
		}
	}
}

func TestArithmetic(t *testing.T) {
	m := decimal.MustParse
	cases := []struct{ got, want string }{
		{m("604211.40").Add(m("125000")).String(), "729211.40"},
		{m("12.08").Sub(m("15302.17")).String(), "-15290.09"},
		{m("-5").Neg().String(), "5"},
		{m("-0.01").Abs().String(), "0.01"},
		{decimal.FromInt(13).Half().String(), "6.5"},
		{decimal.FromInt(2).Half().String(), "1.0"},
		{m("0.05").Half().String(), "0.025"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %s, want %s", c.got, c.want)
		}
	}
	if !m("1.50").Equal(m("1.5")) || m("1.5").Cmp(m("1.49")) != 1 || m("-2").Cmp(m("1")) != -1 {
		t.Error("comparison by value")
	}
	if !m("3.00").IsInteger() || m("3.01").IsInteger() || !m("-7").IsInteger() {
		t.Error("IsInteger")
	}
	if (decimal.Decimal{}).String() != "0" || (decimal.Decimal{}).Sign() != 0 {
		t.Error("zero value is not 0")
	}
}

func TestJSON(t *testing.T) {
	var v struct {
		A decimal.Decimal `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":"-1234.50"}`), &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) != `{"a":"-1234.50"}` {
		t.Fatalf("%s %v", b, err)
	}
	if err := json.Unmarshal([]byte(`{"a":1234.5}`), &v); err == nil {
		t.Fatal("a JSON number was accepted; amounts are decimal strings (N-4)")
	}
	if err := json.Unmarshal([]byte(`{"a":"1,234"}`), &v); err == nil {
		t.Fatal("a grouped string was accepted")
	}
}
