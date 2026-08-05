package exchange

import (
	"encoding/json"
	"market-maker/internal/fixed"
	"market-maker/internal/orderbook"
	"math"
	"reflect"
	"strings"
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

func TestOrdersRejectWorstCaseInsolvencyIncludingOpenOrdersAndReplacement(t *testing.T) {
	cfg := config(t)
	cfg.StartingCash = mustMoney(t, "100")
	cfg.MaxPosition = mustQty(t, "2")
	cfg.MaxOrderQty = mustQty(t, "2")
	cfg.InitialMarginBps, cfg.MaintenanceMarginBps, cfg.MaxOrdersPerTurn = 0, 0, 0

	t.Run("generic includes open orders", func(t *testing.T) {
		e, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "1"), mustQty(t, "1"), GTC); err != nil {
			t.Fatal(err)
		}
		before := e.State()
		if _, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "1"), mustQty(t, "1"), GTC); err == nil {
			t.Fatal("expected aggregate one-sided insolvency rejection")
		}
		if e.State() != before || len(e.OpenOrders(PlayerAccount)) != 1 {
			t.Fatal("rejected order changed venue")
		}
	})

	t.Run("post-fill initial margin", func(t *testing.T) {
		marginCfg := cfg
		marginCfg.InitialMarginBps = 10_000
		e, err := New(marginCfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "50"), mustQty(t, "1"), GTC); err == nil {
			t.Fatal("expected post-fill initial-margin rejection")
		}
	})

	t.Run("replacement excludes old order", func(t *testing.T) {
		e, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		placed, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "101"), mustQty(t, "2"), GTC)
		if err != nil {
			t.Fatal(err)
		}
		oldID := placed.Events[0].Order.ID
		replaced, err := e.ReplaceLimit(PlayerAccount, oldID, Sell, mustPrice(t, "102"), mustQty(t, "2"), GTC)
		if err != nil {
			t.Fatal("replacement counted the old order against risk limits:", err)
		}
		oldID = replaced.Events[0].Order.ID
		beforeState, beforeOrders := e.State(), e.OpenOrders(PlayerAccount)
		if _, err := e.ReplaceLimit(PlayerAccount, oldID, Sell, mustPrice(t, "1"), mustQty(t, "2"), GTC); err == nil {
			t.Fatal("expected replacement insolvency rejection")
		}
		if e.State() != beforeState || !reflect.DeepEqual(e.OpenOrders(PlayerAccount), beforeOrders) {
			t.Fatal("rejected replacement changed venue")
		}
	})
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

func TestAccountReservationsFollowOrderLifecycle(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	placed, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "2"), GTC)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := e.Account(PlayerAccount)
	if !ok || state.ReservedCash != mustMoney(t, "198") || state.AvailableCash != mustMoney(t, "99802") || state.OpenBuyQuantity != mustQty(t, "2") {
		t.Fatalf("reserved account=%+v", state)
	}
	if _, err := e.Cancel(PlayerAccount, placed.Events[0].Order.ID); err != nil {
		t.Fatal(err)
	}
	state, _ = e.Account(PlayerAccount)
	if state.ReservedCash != 0 || state.OpenBuyQuantity != 0 || state.AvailableCash != mustMoney(t, "100000") {
		t.Fatalf("released account=%+v", state)
	}
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	state, _ = e.Account(PlayerAccount)
	if state.Cash != mustMoney(t, "99900") || state.Position != mustQty(t, "1") || state.ReservedCash != 0 {
		t.Fatalf("filled account=%+v", state)
	}
}

func TestReservationsDeriveFromLiveOrdersAcrossLifecycle(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "2"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "2"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.Cancel(PlayerAccount, 1); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "2"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "98"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.Cancel(PlayerAccount, 5); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "90"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "97"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.ReplaceLimit(PlayerAccount, 8, Buy, mustPrice(t, "96"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "110"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
	if _, err := e.ReplaceLimit(PlayerAccount, 10, Sell, mustPrice(t, "111"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	assertReservationsDerivedFromBook(t, e)
}

func TestExchangeReadModelsAreDefensiveCopies(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	ordersBefore := e.OpenOrders(PlayerAccount)
	ledgerBefore := e.LedgerEntries()
	orders := e.OpenOrders(PlayerAccount)
	ledger := e.LedgerEntries()
	orders[0].Remaining = 0
	ledger[0].Postings[0].Money++
	if got := e.OpenOrders(PlayerAccount); !reflect.DeepEqual(got, ordersBefore) {
		t.Fatalf("orders changed through read model: got=%+v want=%+v", got, ordersBefore)
	}
	if got := e.LedgerEntries(); !reflect.DeepEqual(got, ledgerBefore) {
		t.Fatalf("ledger changed through read model: got=%+v want=%+v", got, ledgerBefore)
	}
}

func assertReservationsDerivedFromBook(t *testing.T, e *Engine) {
	t.Helper()
	type reservation struct {
		cash      fixed.Money
		buy, sell fixed.Qty
	}
	want := make(map[string]reservation)
	for _, order := range e.book.Orders("") {
		account := e.accounts[order.OwnerID]
		if account.External {
			continue
		}
		current := want[order.OwnerID]
		if order.Side == orderbook.Buy {
			notional, err := fixed.Notional(order.Price, order.Remaining)
			if err != nil {
				t.Fatal(err)
			}
			current.cash, err = fixed.AddMoney(current.cash, notional)
			if err != nil {
				t.Fatal(err)
			}
			current.buy, err = fixed.AddQty(current.buy, order.Remaining)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			var err error
			current.sell, err = fixed.AddQty(current.sell, order.Remaining)
			if err != nil {
				t.Fatal(err)
			}
		}
		want[order.OwnerID] = current
	}
	for id, account := range e.accounts {
		if account.External {
			continue
		}
		got, ok := e.Account(id)
		if !ok {
			t.Fatalf("account %q missing", id)
		}
		if got.ReservedCash != want[id].cash || got.OpenBuyQuantity != want[id].buy || got.OpenSellQuantity != want[id].sell {
			t.Fatalf("account %q reservations=%+v want=%+v", id, got, want[id])
		}
	}
}

func TestOpenAccountIsAVersionedVenueCommand(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Execute(Command{ID: "open-seller", Type: CommandOpenAccount, AccountID: "seller", InitialCash: mustMoney(t, "250"), InitialPosition: mustQty(t, "2")})
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Version != 1 || len(result.Events) != 1 || result.Events[0].Type != "account_opened" || result.Events[0].Account == nil {
		t.Fatalf("result=%+v", result)
	}
	account, ok := e.Account("seller")
	if !ok || account.Cash != mustMoney(t, "250") || account.Position != mustQty(t, "2") {
		t.Fatalf("account=%+v", account)
	}
	if _, err := e.Execute(Command{ID: "duplicate", Type: CommandOpenAccount, AccountID: "seller"}); err == nil {
		t.Fatal("expected duplicate account rejection")
	}
}

func TestReplaceIsAtomicAndSettlesAtMakerPrice(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "1"), GTC)
	if err != nil {
		t.Fatal(err)
	}
	oldID := initial.Events[0].Order.ID
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	result, err := e.ReplaceLimit(PlayerAccount, oldID, Buy, mustPrice(t, "101"), mustQty(t, "1"), GTC)
	if err != nil {
		t.Fatal(err)
	}
	if result.Events[0].Type != "order_replaced" || result.Events[0].PreviousOrder.ID != oldID || result.Events[0].Order.ID == oldID {
		t.Fatalf("events=%+v", result.Events)
	}
	if _, ok := e.book.Order(oldID); ok {
		t.Fatal("old order remains live")
	}
	state, _ := e.Account(PlayerAccount)
	if state.Cash != mustMoney(t, "99900") || state.Position != mustQty(t, "1") || state.ReservedCash != 0 {
		t.Fatalf("account=%+v", state)
	}
	assertLedgerMatchesAccounts(t, e)
}

func TestRejectedReplaceLeavesVenueUntouched(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	placed, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "1"), GTC)
	if err != nil {
		t.Fatal(err)
	}
	oldID := placed.Events[0].Order.ID
	beforeState := e.State()
	beforeLedger := len(e.LedgerEntries())
	if _, err := e.ReplaceLimit(PlayerAccount, oldID, Sell, mustPrice(t, "101"), mustQty(t, "1"), GTC); err == nil {
		t.Fatal("expected side-change rejection")
	}
	if e.State() != beforeState || len(e.LedgerEntries()) != beforeLedger {
		t.Fatal("rejected replace changed venue state")
	}
	order, ok := e.book.Order(oldID)
	if !ok || order.Remaining != mustQty(t, "1") {
		t.Fatalf("old order=%+v", order)
	}
	assertLedgerMatchesAccounts(t, e)
}

func TestLedgerEntriesBalanceAndMatchAccountProjections(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "2")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "99"), mustQty(t, "2"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Cancel(PlayerAccount, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	assertLedgerMatchesAccounts(t, e)
}

func TestMakerBuySettlementConsumesReservedCash(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("seller", mustMoney(t, "100000"), mustQty(t, "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("seller", Sell, mustPrice(t, "100"), mustQty(t, "1"), IOC); err != nil {
		t.Fatal(err)
	}
	entries := e.LedgerEntries()
	var entry LedgerEntry
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == "trade_settled" {
			entry = entries[i]
			break
		}
	}
	if entry.Type != "trade_settled" {
		t.Fatalf("entries=%+v", entries)
	}
	postings := make(map[LedgerAccount]Posting)
	for _, posting := range entry.Postings {
		postings[posting.Account] = posting
	}
	if postings[cashReserved(PlayerAccount)].Money != -mustMoney(t, "101") {
		t.Fatalf("reserved posting=%+v", postings)
	}
	if postings[cashAvailable(PlayerAccount)].Money != 0 {
		t.Fatalf("buyer available posting=%+v", postings)
	}
	if postings[cashAvailable("seller")].Money != mustMoney(t, "101") {
		t.Fatalf("seller posting=%+v", postings)
	}
	if postings[instrumentAvailable(PlayerAccount)].Instrument != mustQty(t, "1") || postings[instrumentAvailable("seller")].Instrument != -mustQty(t, "1") {
		t.Fatalf("instrument postings=%+v", postings)
	}
	assertLedgerMatchesAccounts(t, e)
}

func assertLedgerMatchesAccounts(t *testing.T, e *Engine) {
	t.Helper()
	for _, entry := range e.LedgerEntries() {
		money, instrument := fixed.Money(0), fixed.Qty(0)
		for _, posting := range entry.Postings {
			money += posting.Money
			instrument += posting.Instrument
		}
		if money != 0 || instrument != 0 {
			t.Fatalf("unbalanced ledger entry=%+v", entry)
		}
	}
	for id, account := range e.accounts {
		cash := e.LedgerBalance(cashAvailable(id)).Money + e.LedgerBalance(cashReserved(id)).Money
		position := e.LedgerBalance(instrumentAvailable(id)).Instrument + e.LedgerBalance(instrumentReserved(id)).Instrument
		if cash != account.Cash || position != account.Position {
			t.Fatalf("ledger projection account=%s cash=%s/%s position=%s/%s", id, cash, account.Cash, position, account.Position)
		}
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
		if r1.State != r2.State || r1.Summary != r2.Summary || !reflect.DeepEqual(r1.Events, r2.Events) || !reflect.DeepEqual(r1.Ledger, r2.Ledger) {
			t.Fatal("same scenario diverged")
		}
	}
}

func TestCommandStreamIsDeterministicAndTransactional(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 3
	cfg.MinMoveBps, cfg.MaxMoveBps = -100, 100
	cfg.InitialMarginBps, cfg.MaintenanceMarginBps = 0, 0
	cfg.MaxOrderQty = mustQty(t, "1")
	left, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	right, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stream := []Command{
		{ID: "01", Type: CommandOpenAccount, AccountID: "seller", InitialCash: mustMoney(t, "10000"), InitialPosition: mustQty(t, "2")},
		{ID: "02", Type: CommandPlaceOrder, AccountID: PlayerAccount, Side: Buy, Price: mustPrice(t, "99"), Quantity: mustQty(t, "1"), TIF: GTC},
		{ID: "03", Type: CommandPlaceOrder, AccountID: PlayerAccount, Side: Sell, Price: mustPrice(t, "98"), Quantity: mustQty(t, "1"), TIF: IOC},
		{ID: "04", Type: CommandReplaceOrder, AccountID: PlayerAccount, OrderID: 1, Side: Buy, Price: mustPrice(t, "98"), Quantity: mustQty(t, "1"), TIF: GTC},
		{ID: "05", Type: CommandCancelOrder, AccountID: PlayerAccount, OrderID: 2},
		{ID: "06", Type: CommandPlaceOrder, AccountID: "seller", Side: Sell, Price: mustPrice(t, "100"), Quantity: mustQty(t, "1"), TIF: GTC},
		{ID: "07", Type: CommandPlaceOrder, AccountID: PlayerAccount, Side: Buy, Price: mustPrice(t, "101"), Quantity: mustQty(t, "1"), TIF: IOC},
		{ID: "08", Type: CommandPlaceOrder, AccountID: PlayerAccount, Side: Buy, Price: mustPrice(t, "99"), Quantity: mustQty(t, "1"), TIF: GTC},
		{ID: "09", Type: CommandSubmitQuote, Bid: mustPrice(t, "99"), Ask: mustPrice(t, "101")},
		{ID: "10", Type: CommandQuit},
		{ID: "11", Type: CommandPlaceOrder, AccountID: PlayerAccount, Side: Buy, Price: mustPrice(t, "99"), Quantity: mustQty(t, "1"), TIF: GTC},
	}
	for step, command := range stream {
		leftBefore := exchangeObservableState(left)
		rightBefore := exchangeObservableState(right)
		leftResult, leftErr := left.Execute(command)
		rightResult, rightErr := right.Execute(command)
		if (leftErr == nil) != (rightErr == nil) {
			t.Fatalf("step %d command=%+v errors differ: left=%v right=%v", step, command, leftErr, rightErr)
		}
		if leftErr != nil && leftErr.Error() != rightErr.Error() {
			t.Fatalf("step %d command=%+v error left=%v right=%v", step, command, leftErr, rightErr)
		}
		if !reflect.DeepEqual(leftResult, rightResult) || !reflect.DeepEqual(exchangeObservableState(left), exchangeObservableState(right)) {
			t.Fatalf("step %d command=%+v produced divergent engines", step, command)
		}
		if leftErr != nil {
			if !reflect.DeepEqual(exchangeObservableState(left), leftBefore) || !reflect.DeepEqual(exchangeObservableState(right), rightBefore) {
				t.Fatalf("step %d rejected command changed engine state", step)
			}
		}
		assertReservationsDerivedFromBook(t, left)
		assertReservationsDerivedFromBook(t, right)
		assertLedgerMatchesAccounts(t, left)
		assertLedgerMatchesAccounts(t, right)
	}
}

type observableExchangeState struct {
	state                           State
	orders                          []orderbook.Order
	ledger                          []LedgerEntry
	nextOrder, nextTrade, nextEvent uint64
	flowRNG, markRNG, informedRNG   uint64
	pcgState                        string
}

func exchangeObservableState(e *Engine) observableExchangeState {
	pcgState, err := e.pcg.MarshalBinary()
	if err != nil {
		panic(err)
	}
	return observableExchangeState{
		state: e.State(), orders: e.book.Orders(""), ledger: e.LedgerEntries(),
		nextOrder: e.nextOrder, nextTrade: e.nextTrade, nextEvent: e.nextEvent,
		flowRNG: e.flowRNG.state, markRNG: e.markRNG.state, informedRNG: e.informedRNG.state,
		pcgState: string(pcgState),
	}
}

func TestSimulationVersionConfigValidation(t *testing.T) {
	tests := []struct {
		name     string
		version  SimulationVersion
		informed int64
		wantErr  bool
	}{
		{name: "zero is legacy"},
		{name: "explicit legacy", version: SimulationVersionLegacy},
		{name: "legacy rejects informed flow", version: SimulationVersionLegacy, informed: 1, wantErr: true},
		{name: "zero rejects informed flow", informed: 1, wantErr: true},
		{name: "v2 zero", version: SimulationVersionAdverseSelection},
		{name: "v2 maximum", version: SimulationVersionAdverseSelection, informed: 10_000},
		{name: "v2 negative", version: SimulationVersionAdverseSelection, informed: -1, wantErr: true},
		{name: "v2 above maximum", version: SimulationVersionAdverseSelection, informed: 10_001, wantErr: true},
		{name: "unknown version", version: 3, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config(t)
			cfg.SimulationVersion = test.version
			cfg.InformedFlowBps = test.informed
			err := cfg.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestLegacyVersionPreservesResultsAndJSONShape(t *testing.T) {
	implicitConfig := config(t)
	explicitConfig := implicitConfig
	explicitConfig.SimulationVersion = SimulationVersionLegacy
	implicit, err := New(implicitConfig)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := New(explicitConfig)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		implicitResult, err := implicit.SubmitQuote(mustPrice(t, "99.50"), mustPrice(t, "100.50"))
		if err != nil {
			t.Fatal(err)
		}
		explicitResult, err := explicit.SubmitQuote(mustPrice(t, "99.50"), mustPrice(t, "100.50"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(implicitResult, explicitResult) {
			t.Fatalf("implicit and explicit legacy results differ\nimplicit=%+v\nexplicit=%+v", implicitResult, explicitResult)
		}
		data, err := json.Marshal(implicitResult)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"informed", "previous_mark", "pnl_attribution", "informed_orders", "informed_units_traded", "informed_flow_pnl"} {
			if strings.Contains(string(data), `"`+field+`"`) {
				t.Fatalf("legacy result unexpectedly contains %q: %s", field, data)
			}
		}
		for _, event := range implicitResult.Events {
			if event.Type == "mark_updated" {
				if event.PreviousMark != 0 || event.Message != "previous=100.0000" {
					t.Fatalf("legacy mark event=%+v", event)
				}
			}
		}
	}
	configData, err := json.Marshal(implicitConfig)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configData), "simulation_version") || strings.Contains(string(configData), "informed_flow_bps") {
		t.Fatalf("zero-value version fields changed legacy config JSON: %s", configData)
	}
}

func TestLegacyFlowSummaryPreservesVenueWideUnits(t *testing.T) {
	summary := Summary{}
	competitorTrade := Trade{Price: mustPrice(t, "100"), Quantity: mustQty(t, "2"), BuyerID: FlowAccount, SellerID: "competitor"}
	playerTrade := Trade{Price: mustPrice(t, "101"), Quantity: mustQty(t, "1"), BuyerID: FlowAccount, SellerID: PlayerAccount}
	for _, trade := range []Trade{competitorTrade, playerTrade} {
		if err := addLegacyFlowTrade(&summary, trade); err != nil {
			t.Fatal(err)
		}
	}
	wantCash, err := fixed.Notional(playerTrade.Price, playerTrade.Quantity)
	if err != nil {
		t.Fatal(err)
	}
	wantUnits, err := fixed.AddQty(competitorTrade.Quantity, playerTrade.Quantity)
	if err != nil {
		t.Fatal(err)
	}
	if summary.UnitsTraded != wantUnits || summary.BuyVolume != playerTrade.Quantity || summary.SellVolume != 0 || summary.NetFillCash != wantCash {
		t.Fatalf("legacy summary=%+v", summary)
	}
}

func TestLegacyEquityDeltaOverflowRollsBackTransaction(t *testing.T) {
	const startingPosition = fixed.Qty(-10)
	cfg := config(t)
	cfg.StartingCash = fixed.Money(math.MaxInt64)
	cfg.StartingPosition = startingPosition
	cfg.StartingMark = fixed.Price(math.MaxInt64 / 20)
	cfg.StoragePerUnit = fixed.Price(math.MaxInt64 / 11)
	cfg.MaxPosition = fixed.Qty(11)
	cfg.MaxOrderQty = fixed.Qty(1)
	cfg.InitialMarginBps, cfg.MaintenanceMarginBps = 0, 0
	cfg.MaxOrdersPerTurn = 0
	cfg.MinMoveBps, cfg.MaxMoveBps = 10_000, 10_000
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	beforeState := e.State()
	beforeLedger := e.LedgerEntries()
	beforeRNG, err := e.pcg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeOrder, beforeTrade, beforeEvent := e.nextOrder, e.nextTrade, e.nextEvent

	if _, err := e.SubmitQuote(fixed.Price(1), fixed.Price(2)); err == nil {
		t.Fatal("expected legacy equity delta overflow")
	}
	afterRNG, err := e.pcg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if e.State() != beforeState || len(e.OpenOrders(PlayerAccount)) != 0 || !reflect.DeepEqual(e.LedgerEntries(), beforeLedger) {
		t.Fatal("equity delta overflow changed live venue")
	}
	if e.nextOrder != beforeOrder || e.nextTrade != beforeTrade || e.nextEvent != beforeEvent || !reflect.DeepEqual(afterRNG, beforeRNG) {
		t.Fatal("equity delta overflow consumed counters or RNG")
	}
}

func TestLegacyFlowSummaryOverflowRollsBackTransaction(t *testing.T) {
	maxFlowQty := fixed.Qty(math.MaxInt64 / 4)
	cfg := config(t)
	cfg.StartingCash = fixed.Money(maxFlowQty)
	cfg.StartingMark = fixed.Price(3)
	cfg.MaxPosition = maxFlowQty
	cfg.MaxOrderQty = maxFlowQty
	cfg.InitialMarginBps, cfg.MaintenanceMarginBps = 0, 0
	cfg.MaxOrdersPerTurn = 20
	cfg.MaxFlowSlippageBps = 0
	cfg.Seed = 3569
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		id := string(rune('a' + i))
		e.accounts[id] = &Account{ID: id, External: true}
		if _, err := e.PlaceLimit(id, Sell, fixed.Price(2+i/2), maxFlowQty, GTC); err != nil {
			t.Fatal(err)
		}
	}
	beforeState := e.State()
	beforeOrders := e.book.Orders("")
	beforeLedger := e.LedgerEntries()
	beforeAccounts := make(map[string]Account, len(e.accounts))
	for id, account := range e.accounts {
		beforeAccounts[id] = *account
	}
	beforeRNG, err := e.pcg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	beforeOrder, beforeTrade, beforeEvent := e.nextOrder, e.nextTrade, e.nextEvent

	if _, err := e.SubmitQuote(fixed.Price(1), fixed.Price(3)); err == nil {
		t.Fatal("expected legacy venue-flow unit overflow")
	}
	afterRNG, err := e.pcg.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if e.State() != beforeState || !reflect.DeepEqual(e.book.Orders(""), beforeOrders) || !reflect.DeepEqual(e.LedgerEntries(), beforeLedger) {
		t.Fatal("flow summary overflow changed live venue")
	}
	for id, before := range beforeAccounts {
		if *e.accounts[id] != before {
			t.Fatalf("flow summary overflow changed account %q", id)
		}
	}
	if e.nextOrder != beforeOrder || e.nextTrade != beforeTrade || e.nextEvent != beforeEvent || !reflect.DeepEqual(afterRNG, beforeRNG) {
		t.Fatal("flow summary overflow consumed counters or RNG")
	}
}

func TestV2DeterministicResults(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.InformedFlowBps = 4_000
	cfg.MinMoveBps, cfg.MaxMoveBps = -100, 100
	one, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	two, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		oneResult, err := one.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101"))
		if err != nil {
			t.Fatal(err)
		}
		twoResult, err := two.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101"))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(oneResult, twoResult) {
			t.Fatal("same v2 scenario diverged")
		}
		if oneResult.Summary.PnLAttribution == nil {
			t.Fatal("v2 omitted zero-valued attribution")
		}
	}
}

func TestV2MarkStreamIsIndependentFromFlowSettings(t *testing.T) {
	quietConfig := config(t)
	quietConfig.SimulationVersion = SimulationVersionAdverseSelection
	quietConfig.MaxOrdersPerTurn = 0
	quietConfig.MinMoveBps, quietConfig.MaxMoveBps = -100, 100
	busyConfig := quietConfig
	busyConfig.MaxOrdersPerTurn = 7
	busyConfig.MaxOrderQty = mustQty(t, "3")
	busyConfig.MaxFlowSlippageBps = 250
	quiet, err := New(quietConfig)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := New(busyConfig)
	if err != nil {
		t.Fatal(err)
	}
	for turn := 1; turn <= 8; turn++ {
		quietResult, err := quiet.SubmitQuote(mustPrice(t, "50"), mustPrice(t, "150"))
		if err != nil {
			t.Fatal(err)
		}
		busyResult, err := busy.SubmitQuote(mustPrice(t, "50"), mustPrice(t, "150"))
		if err != nil {
			t.Fatal(err)
		}
		if quietResult.State.Mark != busyResult.State.Mark {
			t.Fatalf("turn %d marks differ: quiet=%s busy=%s", turn, quietResult.State.Mark, busyResult.State.Mark)
		}
	}
}

func TestV2InformedDirectionMetricsAndEvents(t *testing.T) {
	tests := []struct {
		name       string
		move       int64
		bid        string
		ask        string
		wantSide   Side
		wantMark   string
		buyingFlow bool
	}{
		{name: "up move buys", move: 100, bid: "99", ask: "100", wantSide: Buy, wantMark: "101", buyingFlow: true},
		{name: "down move sells", move: -100, bid: "100", ask: "101", wantSide: Sell, wantMark: "99"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config(t)
			cfg.SimulationVersion = SimulationVersionAdverseSelection
			cfg.InformedFlowBps = 10_000
			cfg.MaxOrderQty = mustQty(t, "1")
			cfg.MaxFlowSlippageBps = 0
			cfg.MinMoveBps, cfg.MaxMoveBps = test.move, test.move
			e, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := e.SubmitQuote(mustPrice(t, test.bid), mustPrice(t, test.ask))
			if err != nil {
				t.Fatal(err)
			}
			var flowOrder *Order
			var flowTrade *Trade
			var markEvent *Event
			for i := range result.Events {
				event := &result.Events[i]
				switch event.Type {
				case "flow_order":
					flowOrder = event.Order
				case "trade":
					if event.Trade.Informed {
						flowTrade = event.Trade
					}
				case "mark_updated":
					markEvent = event
				}
			}
			if flowOrder == nil || !flowOrder.Informed || flowOrder.Side != test.wantSide || flowOrder.TIF != IOC {
				t.Fatalf("flow order=%+v", flowOrder)
			}
			if flowTrade == nil || !flowTrade.Informed {
				t.Fatalf("informed trade=%+v events=%+v", flowTrade, result.Events)
			}
			if result.Summary.InformedOrders != 1 || result.Summary.InformedOrdersFilled != 1 || result.Summary.InformedUnitsTraded != flowTrade.Quantity || result.Summary.UnitsTraded != flowTrade.Quantity {
				t.Fatalf("summary=%+v trade=%+v", result.Summary, flowTrade)
			}
			if test.buyingFlow {
				if result.Summary.BuyVolume != flowTrade.Quantity || result.Summary.SellVolume != 0 {
					t.Fatalf("legacy volume meanings changed: %+v", result.Summary)
				}
			} else if result.Summary.SellVolume != flowTrade.Quantity || result.Summary.BuyVolume != 0 {
				t.Fatalf("legacy volume meanings changed: %+v", result.Summary)
			}
			wantMark := mustPrice(t, test.wantMark)
			if markEvent == nil || markEvent.PreviousMark != mustPrice(t, "100") || markEvent.Mark != wantMark || result.State.Mark != wantMark {
				t.Fatalf("mark event=%+v state=%+v", markEvent, result.State)
			}
			wantInformedPnL, err := playerFillPnL(*flowTrade, wantMark)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.InformedFlowPnL != wantInformedPnL || result.Summary.TurnPnL != wantInformedPnL {
				t.Fatalf("informed pnl=%s turn pnl=%s want=%s", result.Summary.InformedFlowPnL, result.Summary.TurnPnL, wantInformedPnL)
			}
		})
	}
}

func TestV2InformedArrivalCanRemainUnfilled(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.InformedFlowBps = 10_000
	cfg.MaxOrdersPerTurn = 1
	cfg.MaxFlowSlippageBps = 0
	cfg.MinMoveBps, cfg.MaxMoveBps = 100, 100
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.InformedOrders != 1 || result.Summary.InformedOrdersFilled != 0 || result.Summary.InformedUnitsTraded != 0 || result.Summary.UnitsTraded != 0 {
		t.Fatalf("summary=%+v", result.Summary)
	}
	for _, event := range result.Events {
		if event.Type == "flow_order" && (event.Order == nil || !event.Order.Informed || event.Order.Side != Buy) {
			t.Fatalf("flow event=%+v", event)
		}
	}
}

func TestV2InformedMetricsExcludeCompetingLiquidity(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.InformedFlowBps = 10_000
	cfg.MaxOrdersPerTurn = 1
	cfg.MaxOrderQty = mustQty(t, "1")
	cfg.MaxFlowSlippageBps = 0
	cfg.MinMoveBps, cfg.MaxMoveBps = 100, 100
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("competitor", mustMoney(t, "100000"), mustQty(t, "1")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit("competitor", Sell, mustPrice(t, "99"), mustQty(t, "1"), GTC); err != nil {
		t.Fatal(err)
	}
	result, err := e.SubmitQuote(mustPrice(t, "98"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.InformedOrders != 1 || result.Summary.InformedOrdersFilled != 0 || result.Summary.InformedUnitsTraded != 0 || result.Summary.InformedFlowPnL != 0 {
		t.Fatalf("player metrics included competing fill: %+v", result.Summary)
	}
	if result.Summary.UnitsTraded != 0 {
		t.Fatalf("player volume included competing fill: %+v", result.Summary)
	}
}

func TestV2FlowMetricsIncludeOnlyPlayerTrades(t *testing.T) {
	summary := Summary{}
	competitorTrade := Trade{Price: mustPrice(t, "100"), Quantity: mustQty(t, "2"), BuyerID: FlowAccount, SellerID: "competitor"}
	playerTrade := Trade{Price: mustPrice(t, "101"), Quantity: mustQty(t, "1"), BuyerID: FlowAccount, SellerID: PlayerAccount}
	if err := addFlowTrade(&summary, competitorTrade); err != nil {
		t.Fatal(err)
	}
	if err := addFlowTrade(&summary, playerTrade); err != nil {
		t.Fatal(err)
	}
	if summary.UnitsTraded != playerTrade.Quantity || summary.BuyVolume != playerTrade.Quantity || summary.SellVolume != 0 {
		t.Fatalf("volume metrics=%+v", summary)
	}
	wantCash, err := fixed.Notional(playerTrade.Price, playerTrade.Quantity)
	if err != nil {
		t.Fatal(err)
	}
	if summary.NetFillCash != wantCash {
		t.Fatalf("net fill cash=%s want %s", summary.NetFillCash, wantCash)
	}
}

func TestV2FlatActualMarkMoveIsNotInformed(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.InformedFlowBps = 10_000
	cfg.StartingMark = fixed.Price(1)
	cfg.MinMoveBps, cfg.MaxMoveBps = 1, 1
	cfg.MaxOrdersPerTurn = 1
	cfg.MaxFlowSlippageBps = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.SubmitQuote(fixed.Price(1), fixed.Price(2))
	if err != nil {
		t.Fatal(err)
	}
	if result.State.Mark != cfg.StartingMark || result.Summary.InformedOrders != 0 {
		t.Fatalf("state=%+v summary=%+v", result.State, result.Summary)
	}
	for _, event := range result.Events {
		if event.Type == "flow_order" && event.Order.Informed {
			t.Fatalf("flat actual mark classified informed: %+v", event.Order)
		}
	}
}

func TestV2ExactAttributionIncludesImmediateExecutionsAndStorage(t *testing.T) {
	tests := []struct {
		name          string
		makerSide     Side
		makerPrice    string
		move          int64
		wantPosition  string
		wantExecution string
		wantInventory string
		wantStorage   string
		wantTurn      string
	}{
		{name: "player buy", makerSide: Sell, makerPrice: "90", move: 100, wantPosition: "3", wantExecution: "10", wantInventory: "3", wantStorage: "-1.5", wantTurn: "11.5"},
		{name: "player sell", makerSide: Buy, makerPrice: "110", move: -100, wantPosition: "1", wantExecution: "10", wantInventory: "-1", wantStorage: "-0.5", wantTurn: "8.5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config(t)
			cfg.SimulationVersion = SimulationVersionAdverseSelection
			cfg.StartingPosition = mustQty(t, "2")
			cfg.MaxOrderQty = mustQty(t, "1")
			cfg.MaxOrdersPerTurn = 0
			cfg.StoragePerUnit = mustPrice(t, "0.5")
			cfg.MinMoveBps, cfg.MaxMoveBps = test.move, test.move
			e, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.PlaceLimit(FlowAccount, test.makerSide, mustPrice(t, test.makerPrice), mustQty(t, "1"), GTC); err != nil {
				t.Fatal(err)
			}
			result, err := e.SubmitQuote(mustPrice(t, "95"), mustPrice(t, "105"))
			if err != nil {
				t.Fatal(err)
			}
			attribution := result.Summary.PnLAttribution
			if attribution == nil {
				t.Fatal("missing attribution")
			}
			if attribution.ExecutionEdge != mustMoney(t, test.wantExecution) || attribution.InventoryMarkPnL != mustMoney(t, test.wantInventory) || attribution.StoragePnL != mustMoney(t, test.wantStorage) || result.Summary.TurnPnL != mustMoney(t, test.wantTurn) {
				t.Fatalf("attribution=%+v summary=%+v", attribution, result.Summary)
			}
			if result.State.Position != mustQty(t, test.wantPosition) || result.Summary.StorageCost != -attribution.StoragePnL {
				t.Fatalf("state=%+v summary=%+v", result.State, result.Summary)
			}
			if result.Summary.UnitsTraded != 0 || result.Summary.NetFillCash != 0 {
				t.Fatalf("immediate fills changed synthetic-flow legacy metrics: %+v", result.Summary)
			}
		})
	}
}

func TestV2ArithmeticFailureRollsBackStateAndStreams(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.StartingPosition = mustQty(t, "1")
	cfg.MaxOrdersPerTurn = 1
	cfg.MinMoveBps, cfg.MaxMoveBps = -100, 100
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := e.State()
	e.cfg.StoragePerUnit = fixed.Price(math.MaxInt64)
	if _, err := e.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101")); err == nil {
		t.Fatal("expected checked storage arithmetic failure")
	}
	if e.State() != before || len(e.OpenOrders(PlayerAccount)) != 0 {
		t.Fatal("failed v2 turn changed live venue")
	}
	e.cfg.StoragePerUnit = cfg.StoragePerUnit
	got, err := e.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.SubmitQuote(mustPrice(t, "99"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("failed transaction consumed v2 RNG state")
	}
}

func TestV2RejectsInsolventQuoteWithoutConsumingStateOrRNG(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.StartingCash = mustMoney(t, "250")
	cfg.MaxPosition = mustQty(t, "10")
	cfg.MaxOrderQty = mustQty(t, "3")
	cfg.InitialMarginBps, cfg.MaintenanceMarginBps = 0, 0
	cfg.MinMoveBps, cfg.MaxMoveBps = -100, 100
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, beforeLedger := e.State(), e.LedgerEntries()
	beforeFlow, beforeMark, beforeInformed := e.flowRNG, e.markRNG, e.informedRNG
	beforeOrder, beforeTrade, beforeEvent := e.nextOrder, e.nextTrade, e.nextEvent
	if _, err := e.SubmitQuote(mustPrice(t, "1"), mustPrice(t, "2")); err == nil {
		t.Fatal("expected deeply underpriced quote rejection")
	}
	if e.State() != beforeState || !reflect.DeepEqual(e.LedgerEntries(), beforeLedger) || len(e.OpenOrders(PlayerAccount)) != 0 {
		t.Fatal("rejected quote changed venue state")
	}
	if e.flowRNG != beforeFlow || e.markRNG != beforeMark || e.informedRNG != beforeInformed || e.nextOrder != beforeOrder || e.nextTrade != beforeTrade || e.nextEvent != beforeEvent {
		t.Fatal("rejected quote consumed RNG or counters")
	}
	got, err := e.SubmitQuote(mustPrice(t, "50"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want, err := fresh.SubmitQuote(mustPrice(t, "50"), mustPrice(t, "101"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("rejected quote changed subsequent deterministic result")
	}
}

func TestV2FlowLimitOverflowRollsBackTransactionAndStreams(t *testing.T) {
	cfg := config(t)
	cfg.SimulationVersion = SimulationVersionAdverseSelection
	cfg.MaxOrdersPerTurn = 1
	cfg.MaxFlowSlippageBps = 10_000
	cfg.MinMoveBps, cfg.MaxMoveBps = 0, 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.mark = fixed.Price(math.MaxInt64/2 + 1)

	for state := uint64(0); ; state++ {
		probe := splitMix64{state: state}
		probe.bounded(1)
		side := probe.bounded(2)
		probe.bounded(uint64(cfg.MaxOrderQty))
		slip := probe.bounded(uint64(cfg.MaxFlowSlippageBps + 1))
		if side == 0 && slip == uint64(cfg.MaxFlowSlippageBps) {
			e.flowRNG.state = state
			break
		}
	}

	beforeState := e.State()
	beforeOrders := e.book.Orders("")
	beforeLedger := e.LedgerEntries()
	beforeAccounts := make(map[string]Account, len(e.accounts))
	for id, account := range e.accounts {
		beforeAccounts[id] = *account
	}
	beforeFlowRNG, beforeMarkRNG, beforeInformedRNG := e.flowRNG, e.markRNG, e.informedRNG
	beforeOrder, beforeTrade, beforeEvent := e.nextOrder, e.nextTrade, e.nextEvent

	if _, err := e.SubmitQuote(fixed.Price(1), fixed.Price(2)); err == nil {
		t.Fatal("expected overflowing v2 flow limit to fail instead of clamping to price 1")
	}
	if e.State() != beforeState || !reflect.DeepEqual(e.book.Orders(""), beforeOrders) || !reflect.DeepEqual(e.LedgerEntries(), beforeLedger) {
		t.Fatal("overflowing v2 flow limit changed venue state")
	}
	for id, before := range beforeAccounts {
		if *e.accounts[id] != before {
			t.Fatalf("overflowing v2 flow limit changed account %q", id)
		}
	}
	if e.nextOrder != beforeOrder || e.nextTrade != beforeTrade || e.nextEvent != beforeEvent {
		t.Fatal("overflowing v2 flow limit consumed counters")
	}
	if e.flowRNG != beforeFlowRNG || e.markRNG != beforeMarkRNG || e.informedRNG != beforeInformedRNG {
		t.Fatal("overflowing v2 flow limit consumed RNG streams")
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

func TestRejectedSelfTradeDoesNotConsumeCountersOrEvents(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	placed, err := e.PlaceLimit(PlayerAccount, Sell, mustPrice(t, "101"), mustQty(t, "1"), GTC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "101"), mustQty(t, "1"), IOC); err == nil {
		t.Fatal("expected self-trade rejection")
	}
	canceled, err := e.Cancel(PlayerAccount, placed.Events[0].Order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Events[0].Sequence != 3 {
		t.Fatalf("failed command consumed event sequence: %d", canceled.Events[0].Sequence)
	}
	if canceled.State.Version != 2 {
		t.Fatalf("failed command consumed version: %d", canceled.State.Version)
	}
}

func TestSettlementOverflowLeavesLiveVenueUntouched(t *testing.T) {
	cfg := config(t)
	cfg.StartingCash = mustMoney(t, "10")
	cfg.StartingMark = mustPrice(t, "1")
	cfg.MaxPosition = mustQty(t, "1")
	cfg.InitialMarginBps, cfg.MaintenanceMarginBps, cfg.MaxOrdersPerTurn = 0, 0, 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("seller", fixed.Money(math.MaxInt64), 0); err != nil {
		t.Fatal(err)
	}
	e.accounts["seller"].External = true
	sell, err := e.PlaceLimit("seller", Sell, mustPrice(t, "1"), mustQty(t, "1"), GTC)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, beforeLedger := e.State(), len(e.LedgerEntries())
	if _, err := e.PlaceLimit(PlayerAccount, Buy, mustPrice(t, "1"), mustQty(t, "1"), IOC); err == nil {
		t.Fatal("expected settlement overflow")
	}
	if e.State() != beforeState || len(e.LedgerEntries()) != beforeLedger {
		t.Fatal("overflowing settlement changed live venue")
	}
	if _, ok := e.book.Order(sell.Events[0].Order.ID); !ok {
		t.Fatal("overflowing settlement consumed maker order")
	}
}

func TestMarkedEquityOverflowRollsBackTakerAndMakerSettlement(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *Engine) uint64
		trade func(*Engine) error
	}{
		{
			name: "taker",
			setup: func(t *testing.T, e *Engine) uint64 {
				result, err := e.PlaceLimit(FlowAccount, Sell, fixed.Price(1), fixed.Qty(1), GTC)
				if err != nil {
					t.Fatal(err)
				}
				return result.Events[0].Order.ID
			},
			trade: func(e *Engine) error {
				_, err := e.PlaceLimit(PlayerAccount, Buy, fixed.Price(100), fixed.Qty(1), IOC)
				return err
			},
		},
		{
			name: "maker",
			setup: func(t *testing.T, e *Engine) uint64 {
				e.mark = fixed.Price(1)
				result, err := e.PlaceLimit(PlayerAccount, Buy, fixed.Price(1), fixed.Qty(1), GTC)
				if err != nil {
					t.Fatal(err)
				}
				e.mark = fixed.Price(100)
				return result.Events[0].Order.ID
			},
			trade: func(e *Engine) error {
				_, err := e.PlaceLimit(FlowAccount, Sell, fixed.Price(1), fixed.Qty(1), IOC)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config(t)
			cfg.StartingCash = fixed.Money(math.MaxInt64 - 50)
			cfg.StartingMark = fixed.Price(100)
			cfg.MaxPosition = fixed.Qty(10)
			cfg.MaxOrderQty = fixed.Qty(1)
			cfg.InitialMarginBps, cfg.MaintenanceMarginBps, cfg.MaxOrdersPerTurn = 0, 0, 0
			e, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			restingID := test.setup(t, e)
			beforeState, beforeOrders, beforeLedger := e.State(), e.book.Orders(""), e.LedgerEntries()
			beforeOrder, beforeTrade, beforeEvent := e.nextOrder, e.nextTrade, e.nextEvent
			if err := test.trade(e); err == nil {
				t.Fatal("expected marked equity overflow")
			}
			if e.State() != beforeState || !reflect.DeepEqual(e.book.Orders(""), beforeOrders) || !reflect.DeepEqual(e.LedgerEntries(), beforeLedger) {
				t.Fatal("overflowing settlement changed venue")
			}
			if e.nextOrder != beforeOrder || e.nextTrade != beforeTrade || e.nextEvent != beforeEvent {
				t.Fatal("overflowing settlement consumed counters")
			}
			if _, ok := e.book.Order(restingID); !ok {
				t.Fatal("overflowing settlement consumed resting order")
			}
		})
	}
}

func TestEngineRejectsLevelQuantityOverflowAtomically(t *testing.T) {
	cfg := config(t)
	cfg.StartingMark = fixed.Price(1)
	cfg.MaxPosition = fixed.Qty(1)
	cfg.MaxOrderQty = fixed.Qty(1)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.PlaceLimit(FlowAccount, Buy, fixed.Price(1), fixed.Qty(math.MaxInt64), GTC); err != nil {
		t.Fatal(err)
	}
	beforeState, beforeOrders, beforeLedger := e.State(), e.book.Orders(""), e.LedgerEntries()
	beforeOrder, beforeTrade, beforeEvent := e.nextOrder, e.nextTrade, e.nextEvent
	if _, err := e.PlaceLimit(FlowAccount, Buy, fixed.Price(1), fixed.Qty(1), GTC); err == nil {
		t.Fatal("expected engine level quantity overflow")
	}
	if e.State() != beforeState || !reflect.DeepEqual(e.book.Orders(""), beforeOrders) || !reflect.DeepEqual(e.LedgerEntries(), beforeLedger) {
		t.Fatal("level overflow changed venue")
	}
	if e.nextOrder != beforeOrder || e.nextTrade != beforeTrade || e.nextEvent != beforeEvent {
		t.Fatal("level overflow consumed counters")
	}
}

func TestQuitRejectsSecondDistinctCommand(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(Command{ID: "first", Type: CommandQuit}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(Command{ID: "second", Type: CommandQuit}); err == nil {
		t.Fatal("expected second quit rejection")
	}
	if e.State().Version != 1 {
		t.Fatalf("second quit changed version=%d", e.State().Version)
	}
}

func TestInvalidMarkMovementRollsBackTurn(t *testing.T) {
	cfg := config(t)
	cfg.MaxOrdersPerTurn = 0
	cfg.StartingMark = mustPrice(t, "0.0001")
	cfg.MinMoveBps, cfg.MaxMoveBps = -9999, -9999
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := e.State()
	if _, err := e.SubmitQuote(mustPrice(t, "0.0001"), mustPrice(t, "0.0002")); err == nil {
		t.Fatal("expected invalid mark rejection")
	}
	if e.State() != before || len(e.OpenOrders(PlayerAccount)) != 0 {
		t.Fatal("invalid mark movement changed venue")
	}
}

func TestConfigRejectsUnrepresentableStorage(t *testing.T) {
	cfg := config(t)
	cfg.MaxPosition = mustQty(t, "1")
	cfg.StoragePerUnit = fixed.Price(math.MaxInt64)
	if _, err := New(cfg); err == nil {
		t.Fatal("expected storage overflow rejection")
	}
}

func TestConfigRejectsMinInt64StartingPosition(t *testing.T) {
	cfg := config(t)
	cfg.StartingPosition = fixed.Qty(math.MinInt64)
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unrepresentable starting position rejection")
	}
}

func TestAddAccountRejectsMinInt64Position(t *testing.T) {
	cfg := config(t)
	cfg.StartingMark = fixed.Price(1)
	cfg.MaxPosition = fixed.Qty(math.MaxInt64)
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddAccount("unsafe", mustMoney(t, "100000"), fixed.Qty(math.MinInt64)); err == nil {
		t.Fatal("expected unrepresentable position rejection")
	}
	if _, ok := e.Account("unsafe"); ok {
		t.Fatal("rejected account was added")
	}
}
