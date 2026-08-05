package fixed

import (
	"math"
	"testing"
)

func TestCheckedArithmeticInverseInvariants(t *testing.T) {
	values := []int64{math.MinInt64, math.MinInt64 + 1, -100_000_000, -1, 0, 1, 100_000_000, math.MaxInt64 - 1, math.MaxInt64}
	for _, left := range values {
		for _, right := range values {
			if sum, err := AddMoney(Money(left), Money(right)); err == nil {
				got, err := SubMoney(sum, Money(right))
				if err != nil || got != Money(left) {
					t.Fatalf("money add/sub inverse for %d and %d: got=%d err=%v", left, right, got, err)
				}
			}
			if difference, err := SubQty(Qty(left), Qty(right)); err == nil {
				got, err := AddQty(difference, Qty(right))
				if err != nil || got != Qty(left) {
					t.Fatalf("quantity sub/add inverse for %d and %d: got=%d err=%v", left, right, got, err)
				}
			}
		}
	}
}
