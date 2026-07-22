package exchange

import (
	"market-maker/internal/fixed"
	"reflect"
	"testing"
)

func mustPrice(t *testing.T, s string) fixed.Price {
	t.Helper()
	v, err := fixed.ParsePrice(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mustQty(t *testing.T, s string) fixed.Qty {
	t.Helper()
	v, err := fixed.ParseQty(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func mustMoney(t *testing.T, s string) fixed.Money {
	t.Helper()
	v, err := fixed.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func config(t *testing.T) Config {
	return Config{Instrument: "SIM", StartingCash: mustMoney(t, "100000"), StartingMark: mustPrice(t, "100"), MaxPosition: mustQty(t, "1000"), MaxOrderQty: mustQty(t, "10"), InitialMarginBps: 5000, MaintenanceMarginBps: 2500, MaxOrdersPerTurn: 1, MaxFlowSlippageBps: 100, MinMoveBps: 0, MaxMoveBps: 0, Seed: 42}
}

func TestQuoteOnlyFillsWhenCustomerLimitCrosses(t *testing.T) {
	cfg := config(t)
	cfg.MaxFlowSlippageBps = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.UnitsTraded != 0 {
		t.Fatalf("wide quote traded %s at mark-only flow", res.Summary.UnitsTraded)
	}
}

func TestQuoteReservesCashAndPosition(t *testing.T) {
	cfg := config(t)
	cfg.StartingCash = mustMoney(t, "100")
	cfg.MaxOrderQty = mustQty(t, "10")
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101")); err == nil {
		t.Fatal("expected cash reservation rejection")
	}
}

func TestRestingBuyOrdersReserveCashCollectively(t *testing.T) {
	cfg := config(t)
	cfg.StartingCash = mustMoney(t, "100")
	cfg.InitialMarginBps = 0
	cfg.MaintenanceMarginBps = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "80"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "80"), mustQty(t, "1"), GTC); err == nil {
		t.Fatal("expected cumulative cash-reservation rejection")
	}
}

func TestRestingOrdersReserveWorstCasePosition(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	cfg.MaxPosition = mustQty(t, "2")
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "90"), mustQty(t, "2"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "89"), mustQty(t, "0.0001"), GTC); err == nil {
		t.Fatal("expected worst-case position rejection")
	}
}

func TestQuoteMatchesExistingLiquidityAndNeverCrossesBook(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "10")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	result, err := e.SubmitQuote(mustPrice(t, "101"), mustPrice(t, "102"))
	if err != nil {
		t.Fatal(err)
	}
	foundTrade := false
	for _, event := range result.Events {
		if event.Type == "trade" && event.Trade.Price == mustPrice(t, "100") {
			foundTrade = true
		}
	}
	if !foundTrade {
		t.Fatal("crossing quote did not execute resting liquidity")
	}
	state := e.State()
	if state.BestBid > 0 && state.BestAsk > 0 && state.BestBid >= state.BestAsk {
		t.Fatalf("crossed book bid=%s ask=%s", state.BestBid, state.BestAsk)
	}
}

func TestTradesConserveCashAndPosition(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "3")); err != nil {
		t.Fatal(err)
	}
	initialCash, initialPosition := fixed.Money(0), fixed.Qty(0)
	for _, account := range e.accounts {
		initialCash += account.Cash
		initialPosition += account.Position
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "2"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "2"), IOC); err != nil {
		t.Fatal(err)
	}
	finalCash, finalPosition := fixed.Money(0), fixed.Qty(0)
	for _, account := range e.accounts {
		finalCash += account.Cash
		finalPosition += account.Position
	}
	if finalCash != initialCash || finalPosition != initialPosition {
		t.Fatalf("cash %s/%s position %s/%s", finalCash, initialCash, finalPosition, initialPosition)
	}
}

func TestDeterministicResults(t *testing.T) {
	e1, err := New(config(t))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := New(config(t))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r1, err := e1.SubmitQuote(mustPrice(t, "99.50"), mustPrice(t, "100.50"))
		if err != nil {
			t.Fatal(err)
		}
		r2, err := e2.SubmitQuote(mustPrice(t, "99.50"), mustPrice(t, "100.50"))
		if err != nil {
			t.Fatal(err)
		}
		if r1.State != r2.State || r1.Summary != r2.Summary || !reflect.DeepEqual(r1.Events, r2.Events) {
			t.Fatal("same scenario diverged")
		}
	}
}

func TestPriceTimePriorityAndSelfTradePrevention(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("maker-a", mustMoney(t, "100000"), mustQty(t, "2")); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("maker-b", mustMoney(t, "100000"), mustQty(t, "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("maker-a", Sell, mustPrice(t, "100"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("maker-b", Sell, mustPrice(t, "100"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	result, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1.5"), IOC)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 5 || result.Events[1].Trade.SellerID != "maker-a" || result.Events[2].Trade.SellerID != "maker-b" {
		t.Fatalf("trades=%+v", result.Events)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "101"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	before := e.State()
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), IOC); err == nil {
		t.Fatal("expected self trade rejection")
	}
	if e.State().Version != before.Version {
		t.Fatal("rejected self trade changed state")
	}
}
