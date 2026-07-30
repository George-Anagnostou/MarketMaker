package fixed

import (
	"encoding/json"
	"math"
	"math/big"
	"testing"
)

func TestExactNotional(t *testing.T) {
	p, err := ParsePrice("99.1250")
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParseQty("1.2345")
	if err != nil {
		t.Fatal(err)
	}
	n, err := Notional(p, q)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := n.String(), "122.36981250"; got != want {
		t.Fatalf("notional=%s want %s", got, want)
	}
}

func TestRejectPrecisionAndExponent(t *testing.T) {
	for _, input := range []string{"1.00001", "NaN", "1e2", ""} {
		if _, err := ParsePrice(input); err == nil {
			t.Errorf("ParsePrice(%q) succeeded", input)
		}
	}
}

func TestCheckedQuantityOperationsRejectMinInt64(t *testing.T) {
	if _, err := NegQty(Qty(math.MinInt64)); err == nil {
		t.Fatal("NegQty accepted MinInt64")
	}
	if _, err := AbsQtyChecked(Qty(math.MinInt64)); err == nil {
		t.Fatal("AbsQtyChecked accepted MinInt64")
	}
	if _, err := SubQty(0, Qty(math.MinInt64)); err == nil {
		t.Fatal("SubQty accepted an unrepresentable result")
	}
}

func TestCheckedQuantityOperationsAtBounds(t *testing.T) {
	if got, err := NegQty(Qty(math.MaxInt64)); err != nil || got != Qty(-math.MaxInt64) {
		t.Fatalf("NegQty=%d, %v", got, err)
	}
	if got, err := AbsQtyChecked(Qty(-math.MaxInt64)); err != nil || got != Qty(math.MaxInt64) {
		t.Fatalf("AbsQtyChecked=%d, %v", got, err)
	}
	if got, err := SubQty(Qty(math.MinInt64), 1); err == nil || got != 0 {
		t.Fatalf("SubQty underflow=%d, %v", got, err)
	}
	if got, err := SubQty(Qty(math.MaxInt64), -1); err == nil || got != 0 {
		t.Fatalf("SubQty overflow=%d, %v", got, err)
	}
	if got, err := SubQty(Qty(math.MinInt64+1), 1); err != nil || got != Qty(math.MinInt64) {
		t.Fatalf("SubQty=%d, %v", got, err)
	}
}

func TestCheckedMoneyOperationsAtBounds(t *testing.T) {
	if _, err := NegMoney(Money(math.MinInt64)); err == nil {
		t.Fatal("NegMoney accepted MinInt64")
	}
	if got, err := SubMoney(Money(math.MinInt64+1), 1); err != nil || got != Money(math.MinInt64) {
		t.Fatalf("SubMoney=%d, %v", got, err)
	}
	if got, err := SubMoney(Money(math.MaxInt64), -1); err == nil || got != 0 {
		t.Fatalf("SubMoney overflow=%d, %v", got, err)
	}
}

func TestScaleRejectsOverflowInFinalAddition(t *testing.T) {
	overflowing := int64(math.MaxInt64/2 + 1)
	underflowing := int64(math.MinInt64/2 - 1)

	if _, err := ScaleMoney(Money(overflowing), 20_000, 10_000); err == nil {
		t.Fatal("ScaleMoney accepted overflowing quotient and remainder sum")
	}
	if _, err := ScaleMoney(Money(underflowing), 20_000, 10_000); err == nil {
		t.Fatal("ScaleMoney accepted underflowing quotient and remainder sum")
	}
	if _, err := ScalePrice(Price(overflowing), 20_000, 10_000); err == nil {
		t.Fatal("ScalePrice accepted overflowing quotient and remainder sum")
	}
	if _, err := ScalePrice(Price(underflowing), 20_000, 10_000); err == nil {
		t.Fatal("ScalePrice accepted underflowing quotient and remainder sum")
	}
}

func TestScalePreservesTruncationTowardZero(t *testing.T) {
	if got, err := ScaleMoney(-5, 1, 2); err != nil || got != -2 {
		t.Fatalf("ScaleMoney(-5, 1, 2)=%d, %v", got, err)
	}
	if got, err := ScalePrice(-5, 1, 2); err != nil || got != -2 {
		t.Fatalf("ScalePrice(-5, 1, 2)=%d, %v", got, err)
	}
}

func TestScaleMatchesBigIntAtBoundaries(t *testing.T) {
	tests := []struct {
		name                          string
		value, numerator, denominator int64
	}{
		{name: "zero", value: 0, numerator: math.MaxInt64, denominator: 1},
		{name: "zero numerator", value: math.MinInt64, numerator: 0, denominator: 1},
		{name: "positive truncation", value: 5, numerator: 1, denominator: 2},
		{name: "negative truncation", value: -5, numerator: 1, denominator: 2},
		{name: "positive overflowing remainder product", value: math.MaxInt64 - 1, numerator: 2, denominator: math.MaxInt64},
		{name: "negative overflowing remainder product", value: -(math.MaxInt64 - 1), numerator: 2, denominator: math.MaxInt64},
		{name: "positive limit", value: math.MaxInt64, numerator: math.MaxInt64, denominator: math.MaxInt64},
		{name: "negative limit", value: math.MinInt64, numerator: math.MaxInt64, denominator: math.MaxInt64},
		{name: "previous positive final overflow", value: math.MaxInt64/2 + 1, numerator: 20_000, denominator: 10_000},
		{name: "previous negative final overflow", value: math.MinInt64/2 - 1, numerator: 20_000, denominator: 10_000},
		{name: "positive overflow after division", value: math.MaxInt64, numerator: math.MaxInt64, denominator: math.MaxInt64 - 1},
		{name: "negative overflow after division", value: math.MinInt64, numerator: math.MaxInt64, denominator: math.MaxInt64 - 1},
		{name: "positive quotient exceeds uint64", value: math.MaxInt64, numerator: math.MaxInt64, denominator: 1},
		{name: "negative quotient exceeds uint64", value: math.MinInt64, numerator: math.MaxInt64, denominator: 1},
	}
	scalers := []struct {
		name  string
		scale func(int64, int64, int64) (int64, error)
	}{
		{name: "Money", scale: func(value, numerator, denominator int64) (int64, error) {
			got, err := ScaleMoney(Money(value), numerator, denominator)
			return int64(got), err
		}},
		{name: "Price", scale: func(value, numerator, denominator int64) (int64, error) {
			got, err := ScalePrice(Price(value), numerator, denominator)
			return int64(got), err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantBig := new(big.Int).Mul(big.NewInt(tt.value), big.NewInt(tt.numerator))
			wantBig.Quo(wantBig, big.NewInt(tt.denominator))
			for _, scaler := range scalers {
				t.Run(scaler.name, func(t *testing.T) {
					got, err := scaler.scale(tt.value, tt.numerator, tt.denominator)
					if !wantBig.IsInt64() {
						if err == nil {
							t.Fatalf("scale(%d, %d, %d)=%d, want overflow", tt.value, tt.numerator, tt.denominator, got)
						}
						return
					}
					if err != nil || got != wantBig.Int64() {
						t.Fatalf("scale(%d, %d, %d)=%d, %v; want %d", tt.value, tt.numerator, tt.denominator, got, err, wantBig.Int64())
					}
				})
			}
		})
	}
}

func TestDecimalBoundariesParseAndJSONRoundTrip(t *testing.T) {
	testDecimalBoundaries(t, "Price", "-922337203685477.5808", "922337203685477.5807", "-922337203685477.5809", "922337203685477.5808", ParsePrice)
	testDecimalBoundaries(t, "Qty", "-922337203685477.5808", "922337203685477.5807", "-922337203685477.5809", "922337203685477.5808", ParseQty)
	testDecimalBoundaries(t, "Money", "-92233720368.54775808", "92233720368.54775807", "-92233720368.54775809", "92233720368.54775808", ParseMoney)
}

func testDecimalBoundaries[T Price | Qty | Money](t *testing.T, name, minString, maxString, belowMin, aboveMax string, parse func(string) (T, error)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		minValue, err := parse(minString)
		if err != nil || int64(minValue) != math.MinInt64 {
			t.Fatalf("parse minimum=%d, %v", minValue, err)
		}
		maxValue, err := parse(maxString)
		if err != nil || int64(maxValue) != math.MaxInt64 {
			t.Fatalf("parse maximum=%d, %v", maxValue, err)
		}
		for _, input := range []string{belowMin, aboveMax} {
			if _, err := parse(input); err == nil {
				t.Errorf("parse(%q) succeeded", input)
			}
		}
		for _, value := range []T{minValue, maxValue} {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip T
			if err := json.Unmarshal(data, &roundTrip); err != nil {
				t.Fatalf("unmarshal %s: %v", data, err)
			}
			if roundTrip != value {
				t.Fatalf("round trip=%d want %d", roundTrip, value)
			}
		}
	})
}
