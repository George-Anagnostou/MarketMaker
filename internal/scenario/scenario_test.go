package scenario

import (
	"testing"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

func TestCatalogIsValidAndStable(t *testing.T) {
	if err := ValidateCatalog(); err != nil {
		t.Fatal(err)
	}
	if len(List()) < 3 {
		t.Fatal("expected initial lesson catalog")
	}
	if _, ok := Get("first-spread-v1"); !ok {
		t.Fatal("missing first scenario")
	}
}

func TestCoachPrioritizesInventory(t *testing.T) {
	before := exchange.State{Position: 0, Mark: fixed.Price(1_000_000)}
	result := exchange.Result{State: exchange.State{Position: fixed.Qty(10_000), Mark: fixed.Price(1_000_000)}}
	if got := Coach(before, result); got.Code != "inventory-built" {
		t.Fatalf("coaching=%+v", got)
	}
}
