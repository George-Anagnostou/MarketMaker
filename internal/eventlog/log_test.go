package eventlog

import (
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"os"
	"testing"
)

func price(t *testing.T, s string) fixed.Price {
	t.Helper()
	v, err := fixed.ParsePrice(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func qty(t *testing.T, s string) fixed.Qty {
	t.Helper()
	v, err := fixed.ParseQty(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func money(t *testing.T, s string) fixed.Money {
	t.Helper()
	v, err := fixed.ParseMoney(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func testConfig(t *testing.T) exchange.Config {
	return exchange.Config{Instrument: "SIM", StartingCash: money(t, "1000"), StartingMark: price(t, "100"), MaxPosition: qty(t, "100"), MaxOrderQty: qty(t, "1"), InitialMarginBps: 5000, MaintenanceMarginBps: 2500, MaxOrdersPerTurn: 0, MaxFlowSlippageBps: 10, MinMoveBps: 0, MaxMoveBps: 0, Seed: 3}
}

func TestAppendAndOpen(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(t)
	log, err := Create(root, "game-1", "local", "create-1", cfg)
	if err != nil {
		t.Fatal(err)
	}
	e, err := exchange.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.Execute(exchange.Command{ID: "c-1", Type: exchange.CommandSubmitQuote, Bid: price(t, "99"), Ask: price(t, "101")})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandSubmitQuote, Bid: price(t, "99"), Ask: price(t, "101")}, Result: result}); err != nil {
		t.Fatal(err)
	}
	_, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Result.State != result.State {
		t.Fatalf("records=%+v", records)
	}
}

func TestIgnoreIncompleteTrailingRecord(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log.Path()+"/events.jsonl", []byte(`{"schema":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatal("partial record was replayed")
	}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandSubmitQuote, Bid: price(t, "99"), Ask: price(t, "101")}, Result: exchange.Result{}}); err != nil {
		t.Fatal(err)
	}
	_, records, err = Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records=%+v", records)
	}
}
