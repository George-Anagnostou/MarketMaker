package exchange

import (
	"market-maker/internal/fixed"
	"math"
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
