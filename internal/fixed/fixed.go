// Package fixed provides exact decimal values used by the exchange.
package fixed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

func SubQty(a, b Qty) (Qty, error) { return AddQty(a, -b) }

// ScaleMoney returns amount * numerator / denominator without overflowing the
// intermediate product. It truncates toward zero, matching integer settlement.
func ScaleMoney(amount Money, numerator, denominator int64) (Money, error) {
	if denominator <= 0 || numerator < 0 {
		return 0, errors.New("invalid money scale")
	}
	if numerator == 0 {
		return 0, nil
	}
	quotient, remainder := int64(amount)/denominator, int64(amount)%denominator
	if quotient != 0 && (quotient > math.MaxInt64/numerator || quotient < math.MinInt64/numerator) {
		return 0, errors.New("money scale overflows range")
	}
	if remainder != 0 && (remainder > math.MaxInt64/numerator || remainder < math.MinInt64/numerator) {
		return 0, errors.New("money scale overflows range")
	}
	return Money(quotient*numerator + remainder*numerator/denominator), nil
}

func AbsQty(v Qty) Qty {
	if v < 0 {
		return -v
	}
	return v
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
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole < 0 || whole > math.MaxInt64/scale {
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
	frac, err := strconv.ParseInt(fraction, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid decimal %q", s)
	}
	if whole == math.MaxInt64/scale && frac > math.MaxInt64%scale {
		return 0, fmt.Errorf("decimal out of range %q", s)
	}
	value := whole*scale + frac
	if negative {
		value = -value
	}
	return value, nil
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
