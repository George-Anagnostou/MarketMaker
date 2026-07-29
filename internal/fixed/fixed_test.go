package fixed

import (
	"math"
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
