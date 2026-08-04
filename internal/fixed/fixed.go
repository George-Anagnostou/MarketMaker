// Package fixed provides exact decimal values used by the exchange.
package fixed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

const (
	priceScale = int64(10_000)
	qtyScale   = int64(10_000)
	moneyScale = int64(100_000_000)
)

// Price is dollars per unit in ten-thousandths of a dollar.
type Price int64

// Qty is units in ten-thousandths of a unit.
type Qty int64

// Money is dollars in hundred-millionths of a dollar. It is the exact product
// of a Price and Qty and avoids rounding on every execution.
type Money int64

func ParsePrice(s string) (Price, error) { v, err := parse(s, priceScale); return Price(v), err }
func ParseQty(s string) (Qty, error)     { v, err := parse(s, qtyScale); return Qty(v), err }
func ParseMoney(s string) (Money, error) { v, err := parse(s, moneyScale); return Money(v), err }

func (v Price) String() string { return format(int64(v), priceScale) }
func (v Qty) String() string   { return format(int64(v), qtyScale) }
func (v Money) String() string { return format(int64(v), moneyScale) }

func (v Price) Positive() bool { return v > 0 }
func (v Qty) Positive() bool   { return v > 0 }

func (v Price) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }
func (v Qty) MarshalJSON() ([]byte, error)   { return json.Marshal(v.String()) }
func (v Money) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func (v *Price) UnmarshalJSON(data []byte) error {
	value, err := unmarshal(data, priceScale)
	*v = Price(value)
	return err
}
func (v *Qty) UnmarshalJSON(data []byte) error {
	value, err := unmarshal(data, qtyScale)
	*v = Qty(value)
	return err
}
func (v *Money) UnmarshalJSON(data []byte) error {
	value, err := unmarshal(data, moneyScale)
	*v = Money(value)
	return err
}

// Notional returns price * quantity with no rounding.
func Notional(p Price, q Qty) (Money, error) {
	a, b := int64(p), int64(q)
	if a == 0 || b == 0 {
		return 0, nil
	}
	if (a > 0 && b > 0 && a > math.MaxInt64/b) ||
		(a > 0 && b < 0 && b < math.MinInt64/a) ||
		(a < 0 && b > 0 && a < math.MinInt64/b) ||
		(a < 0 && b < 0 && a < math.MaxInt64/b) {
		return 0, errors.New("notional overflows money range")
	}
	return Money(a * b), nil
}

func AddMoney(a, b Money) (Money, error) {
	if (b > 0 && a > Money(math.MaxInt64)-b) || (b < 0 && a < Money(math.MinInt64)-b) {
		return 0, errors.New("money overflows range")
	}
	return a + b, nil
}

func AddQty(a, b Qty) (Qty, error) {
	if (b > 0 && a > Qty(math.MaxInt64)-b) || (b < 0 && a < Qty(math.MinInt64)-b) {
		return 0, errors.New("quantity overflows range")
	}
	return a + b, nil
}

// NegMoney returns the additive inverse of v when it is representable.
func NegMoney(v Money) (Money, error) {
	if v == Money(math.MinInt64) {
		return 0, errors.New("money negation overflows range")
	}
	return -v, nil
}

// NegQty returns the additive inverse of v when it is representable.
func NegQty(v Qty) (Qty, error) {
	if v == Qty(math.MinInt64) {
		return 0, errors.New("quantity negation overflows range")
	}
	return -v, nil
}

func SubMoney(a, b Money) (Money, error) {
	if (b > 0 && a < Money(math.MinInt64)+b) || (b < 0 && a > Money(math.MaxInt64)+b) {
		return 0, errors.New("money overflows range")
	}
	return a - b, nil
}

func SubQty(a, b Qty) (Qty, error) {
	if (b > 0 && a < Qty(math.MinInt64)+b) || (b < 0 && a > Qty(math.MaxInt64)+b) {
		return 0, errors.New("quantity overflows range")
	}
	return a - b, nil
}

// ScaleMoney returns amount * numerator / denominator without overflowing the
// intermediate product. It truncates toward zero, matching integer settlement.
func ScaleMoney(amount Money, numerator, denominator int64) (Money, error) {
	if denominator <= 0 || numerator < 0 {
		return 0, errors.New("invalid money scale")
	}
	result, ok := scaleInt64(int64(amount), numerator, denominator)
	if !ok {
		return 0, errors.New("money scale overflows range")
	}
	return Money(result), nil
}

// ScalePrice returns price * numerator / denominator without an overflowing
// intermediate product.
func ScalePrice(price Price, numerator, denominator int64) (Price, error) {
	if denominator <= 0 || numerator < 0 {
		return 0, errors.New("invalid price scale")
	}
	result, ok := scaleInt64(int64(price), numerator, denominator)
	if !ok {
		return 0, errors.New("price scale overflows range")
	}
	return Price(result), nil
}

func scaleInt64(value, numerator, denominator int64) (int64, bool) {
	negative := value < 0
	magnitude := uint64(value)
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	}

	hi, lo := bits.Mul64(magnitude, uint64(numerator))
	unsignedDenominator := uint64(denominator)
	if hi >= unsignedDenominator {
		return 0, false
	}
	quotient, _ := bits.Div64(hi, lo, unsignedDenominator)
	limit := uint64(math.MaxInt64)
	if negative {
		limit++
	}
	if quotient > limit {
		return 0, false
	}
	if negative {
		if quotient == uint64(math.MaxInt64)+1 {
			return math.MinInt64, true
		}
		return -int64(quotient), true
	}
	return int64(quotient), true
}

// AbsQtyChecked returns the magnitude of v when it is representable as a Qty.
func AbsQtyChecked(v Qty) (Qty, error) {
	if v < 0 {
		return NegQty(v)
	}
	return v, nil
}

// AbsQty returns the magnitude of v. Call AbsQtyChecked when the input can be
// untrusted; the magnitude of MinInt64 cannot be represented as a Qty.
func AbsQty(v Qty) Qty {
	abs, err := AbsQtyChecked(v)
	if err != nil {
		panic(err)
	}
	return abs
}

func parse(s string, scale int64) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("value is required")
	}
	negative := false
	if s[0] == '-' || s[0] == '+' {
		negative = s[0] == '-'
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, fmt.Errorf("invalid decimal %q", s)
	}
	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decimal out of range %q", s)
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	digits := len(strconv.FormatInt(scale, 10)) - 1
	if len(fraction) > digits {
		return 0, fmt.Errorf("decimal %q exceeds %d places", s, digits)
	}
	if fraction == "" {
		fraction = "0"
	}
	for len(fraction) < digits {
		fraction += "0"
	}
	frac, err := strconv.ParseUint(fraction, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid decimal %q", s)
	}
	limit := uint64(math.MaxInt64)
	if negative {
		limit++
	}
	unsignedScale := uint64(scale)
	if whole > limit/unsignedScale || (whole == limit/unsignedScale && frac > limit%unsignedScale) {
		return 0, fmt.Errorf("decimal out of range %q", s)
	}
	magnitude := whole*unsignedScale + frac
	if negative {
		if magnitude == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return -int64(magnitude), nil
	}
	return int64(magnitude), nil
}

func format(value, scale int64) string {
	negative := value < 0
	var magnitude uint64
	if negative {
		magnitude = uint64(-(value + 1)) + 1
	} else {
		magnitude = uint64(value)
	}
	digits := len(strconv.FormatInt(scale, 10)) - 1
	whole, fraction := magnitude/uint64(scale), magnitude%uint64(scale)
	s := fmt.Sprintf("%d.%0*d", whole, digits, fraction)
	if negative {
		return "-" + s
	}
	return s
}

func unmarshal(data []byte, scale int64) (int64, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 1 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return 0, err
		}
		return parse(s, scale)
	}
	return parse(string(data), scale)
}
