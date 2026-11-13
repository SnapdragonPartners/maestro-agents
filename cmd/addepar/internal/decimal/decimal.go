// Package decimal is the intake agent's exact decimal number: a signed
// integer coefficient and a count of digits after the point.
//
// Amounts are decimal strings end to end and never pass through binary
// floating point (intake N-4). A Decimal keeps the precision it was written
// with, so "678250" and "714760.65" print back unchanged; arithmetic is exact
// and comparison is by value ("1.50" equals "1.5").
package decimal

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// maxDigits bounds the digits a decimal string may carry. A statement amount
// has at most a few dozen; the bound keeps a hostile string from building a
// huge big.Int.
const maxDigits = 40

// ErrSyntax reports a string that is not a plain decimal number.
var ErrSyntax = errors.New("decimal: invalid syntax")

// Decimal is an exact decimal number. The zero value is 0.
type Decimal struct {
	coef  *big.Int // nil means 0
	scale int      // digits after the point, >= 0
}

// Parse reads a plain decimal: an optional leading '-', digits, and an
// optional '.' followed by at least one digit. It accepts no '+', exponent,
// grouping comma, currency sign or surrounding space: those belong to the
// printed form, which the verifier reads with ParsePrinted.
func Parse(s string) (Decimal, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	intPart, frac, hasPoint := strings.Cut(body, ".")
	if intPart == "" || (hasPoint && frac == "") || len(intPart)+len(frac) > maxDigits {
		return Decimal{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	for _, r := range intPart + frac {
		if r < '0' || r > '9' {
			return Decimal{}, fmt.Errorf("%w: %q", ErrSyntax, s)
		}
	}
	coef, ok := new(big.Int).SetString(intPart+frac, 10)
	if !ok {
		return Decimal{}, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	if neg {
		coef.Neg(coef)
	}
	return Decimal{coef: coef, scale: len(frac)}, nil
}

// MustParse is Parse for constants in tests and tables; it panics on error.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

// FromInt returns the integer n with no fractional digits.
func FromInt(n int64) Decimal { return Decimal{coef: big.NewInt(n)} }

func (d Decimal) c() *big.Int {
	if d.coef == nil {
		return new(big.Int)
	}
	return d.coef
}

// Scale reports the digits after the point the number was written with.
func (d Decimal) Scale() int { return d.scale }

// String prints the number in its own precision: "-1234.50", "678250".
func (d Decimal) String() string {
	c := d.c()
	digits := new(big.Int).Abs(c).String()
	if d.scale > 0 {
		if len(digits) <= d.scale {
			digits = strings.Repeat("0", d.scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-d.scale] + "." + digits[len(digits)-d.scale:]
	}
	if c.Sign() < 0 {
		return "-" + digits
	}
	return digits
}

// MarshalText writes String, so a Decimal is a JSON string.
func (d Decimal) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText reads Parse's syntax.
func (d *Decimal) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// rescale returns the coefficient of d at scale s >= d.scale.
func (d Decimal) rescale(s int) *big.Int {
	c := new(big.Int).Set(d.c())
	if s > d.scale {
		c.Mul(c, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(s-d.scale)), nil))
	}
	return c
}

func align(a, b Decimal) (x, y *big.Int, scale int) {
	scale = max(a.scale, b.scale)
	return a.rescale(scale), b.rescale(scale), scale
}

// Add returns d + e, at the larger of their scales.
func (d Decimal) Add(e Decimal) Decimal {
	x, y, s := align(d, e)
	return Decimal{coef: x.Add(x, y), scale: s}
}

// Sub returns d - e, at the larger of their scales.
func (d Decimal) Sub(e Decimal) Decimal {
	x, y, s := align(d, e)
	return Decimal{coef: x.Sub(x, y), scale: s}
}

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Decimal{coef: new(big.Int).Neg(d.c()), scale: d.scale} }

// Abs returns |d|.
func (d Decimal) Abs() Decimal { return Decimal{coef: new(big.Int).Abs(d.c()), scale: d.scale} }

// Cmp compares by value: -1, 0 or +1.
func (d Decimal) Cmp(e Decimal) int {
	x, y, _ := align(d, e)
	return x.Cmp(y)
}

// Equal reports d == e by value.
func (d Decimal) Equal(e Decimal) bool { return d.Cmp(e) == 0 }

// Sign returns -1, 0 or +1.
func (d Decimal) Sign() int { return d.c().Sign() }

// IsInteger reports whether d has no nonzero fractional digits.
func (d Decimal) IsInteger() bool {
	if d.scale == 0 {
		return true
	}
	ten := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(d.scale)), nil)
	return new(big.Int).Rem(d.c(), ten).Sign() == 0
}

// Half returns d / 2 exactly (one more fractional digit when d is odd at
// its scale).
func (d Decimal) Half() Decimal {
	c := d.rescale(d.scale + 1)
	return Decimal{coef: c.Quo(c, big.NewInt(2)), scale: d.scale + 1}
}
