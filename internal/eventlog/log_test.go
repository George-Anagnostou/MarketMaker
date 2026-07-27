package eventlog

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/scenario"
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
	log, err := Create(root, "game-1", "local", "create-1", cfg, nil)
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
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
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

func TestOpenRejectsTamperedRecord(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandSubmitQuote, Bid: price(t, "99"), Ask: price(t, "101")}}
	if err := log.Append(record); err != nil {
		t.Fatal(err)
	}
	_, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	record = records[0]
	record.Command.Ask = price(t, "102")
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log.Path()+"/events.jsonl", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("expected tampered record rejection")
	}
}

func TestOpenAcceptsPreScorecardRecap(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	type legacyRecap struct {
		Headline        string             `json:"headline"`
		Body            string             `json:"body"`
		FinalEquity     fixed.Money        `json:"final_equity"`
		TotalPnL        fixed.Money        `json:"total_pnl"`
		MaxAbsInventory fixed.Qty          `json:"max_abs_inventory"`
		UnitsTraded     fixed.Qty          `json:"units_traded"`
		StoragePaid     fixed.Money        `json:"storage_paid"`
		EndReason       exchange.EndReason `json:"end_reason"`
	}
	type legacyRecord struct {
		Schema           int                `json:"schema"`
		Version          uint64             `json:"version"`
		PreviousChecksum string             `json:"previous_checksum,omitempty"`
		Checksum         string             `json:"checksum"`
		Command          exchange.Command   `json:"command"`
		Result           exchange.Result    `json:"result"`
		Coaching         *scenario.Coaching `json:"coaching,omitempty"`
		Recap            *legacyRecap       `json:"recap,omitempty"`
	}
	record := legacyRecord{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}, Recap: &legacyRecap{Headline: "Review", EndReason: exchange.PlayerQuit}}
	checksumRecord := record
	checksumRecord.Checksum = ""
	data, err := json.Marshal(checksumRecord)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append([]byte(record.PreviousChecksum), data...))
	record.Checksum = fmt.Sprintf("%x", digest)
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log.Path()+"/events.jsonl", append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Recap == nil || records[0].Recap.Scorecard != nil {
		t.Fatalf("records=%+v", records)
	}
}
