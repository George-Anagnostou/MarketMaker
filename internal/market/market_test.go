package market

import (
	"math/rand/v2"
	"testing"
)

func TestGenerateFlowCountAndSizes(t *testing.T) {
	r := rand.New(rand.NewPCG(123, 456))

	for trial := 0; trial < 100; trial++ {
		orders := GenerateFlow(r, 5, 10)
		if len(orders) < 1 || len(orders) > 5 {
			t.Fatalf("unexpected order count: %d", len(orders))
		}
		for _, o := range orders {
			if o.Qty < 1 || o.Qty > 10 {
				t.Errorf("qty out of range: %v", o.Qty)
			}
		}
	}
}

func TestGenerateFlowZeroMax(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	orders := GenerateFlow(r, 0, 10)
	if len(orders) != 0 {
		t.Errorf("expected 0 orders, got %d", len(orders))
	}
}

func TestNextPriceRange(t *testing.T) {
	r := rand.New(rand.NewPCG(99, 88))
	start := 100.0
	for i := 0; i < 1000; i++ {
		p := NextPrice(r, start, -0.03, 0.03)
		if p <= 0 {
			t.Fatal("price went non-positive")
		}
		// just ensure it moves within a sane band over many steps
	}
}

func TestNextPriceDeterministic(t *testing.T) {
	r1 := rand.New(rand.NewPCG(42, 42))
	r2 := rand.New(rand.NewPCG(42, 42))

	p1 := NextPrice(r1, 100, -0.01, 0.02)
	p2 := NextPrice(r2, 100, -0.01, 0.02)

	if p1 != p2 {
		t.Errorf("NextPrice not deterministic: %v != %v", p1, p2)
	}
}
