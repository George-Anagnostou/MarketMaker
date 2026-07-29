package eventlog

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/scenario"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if records[0].MetadataChecksum == "" || records[0].MetadataChecksum != log.Meta().Checksum {
		t.Fatalf("record is not bound to metadata: %+v", records[0])
	}
}

func TestCreateIgnoresAbandonedStagingDirectory(t *testing.T) {
	root := t.TempDir()
	staging, err := os.MkdirTemp(root, ".eventlog-staging-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "meta.json"), []byte("unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRejectsPublishedMetadataWithoutEvents(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(log.Path(), "events.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open error = %v, want missing events error", err)
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

func TestOpenDropsUnterminatedCompleteTrailingRecord(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}
	if err := log.Append(record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(log.Path(), "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	log, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("unterminated records=%+v", records)
	}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	_, records, err = Open(root, "game-1")
	if err != nil || len(records) != 1 || records[0].Command.ID != "c-2" {
		t.Fatalf("records=%+v err=%v", records, err)
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

func TestScenarioMetadataIsCopied(t *testing.T) {
	root := t.TempDir()
	snapshot := &scenario.Snapshot{ID: "lesson", Tutorial: []scenario.TutorialStep{{Title: "Original", Body: "Original body"}}}
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tutorial[0].Title = "Mutated input"
	meta := log.Meta()
	if meta.Scenario.Tutorial[0].Title != "Original" {
		t.Fatalf("stored scenario=%+v", meta.Scenario)
	}
	meta.Scenario.Tutorial[0].Title = "Mutated output"
	if got := log.Meta().Scenario.Tutorial[0].Title; got != "Original" {
		t.Fatalf("metadata mutation leaked as %q", got)
	}
}

func TestOpenRejectsTamperedSchema2Metadata(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(log.Path(), "meta.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta.OwnerID = "attacker"
	data, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("expected tampered metadata rejection")
	}
}

func TestOpenRejectsSchema2RecordBoundToDifferentMetadata(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(log.Path(), "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.MetadataChecksum = strings.Repeat("0", 64)
	record.Checksum, err = recordChecksum(record)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("expected metadata-binding rejection")
	}
}

func TestAppendDoesNotRecreateMissingEventsFile(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(log.Path(), "events.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Append error = %v, want missing events error", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("events file was recreated: %v", err)
	}
}

func TestRejectsDuplicateCommandID(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}
	if err := log.Append(record); err != nil {
		t.Fatal(err)
	}
	record.Version = 2
	if err := log.Append(record); err == nil {
		t.Fatal("expected duplicate command rejection")
	}

	path := filepath.Join(log.Path(), "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var first Record
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatal(err)
	}
	duplicate := first
	duplicate.Version = 2
	duplicate.PreviousChecksum = first.Checksum
	duplicate.Checksum, err = recordChecksum(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	duplicateData, err := json.Marshal(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(data, '\n'), append(duplicateData, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("expected duplicate persisted record rejection")
	}
}

func TestOpenAcceptsPreScorecardRecap(t *testing.T) {
	root := t.TempDir()
	log, err := createLegacyLog(root, "game-1", testConfig(t))
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
	record := legacyRecord{Schema: legacySchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}, Recap: &legacyRecap{Headline: "Review", EndReason: exchange.PlayerQuit}}
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
	log, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Recap == nil || records[0].Recap.Scorecard != nil {
		t.Fatalf("records=%+v", records)
	}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	_, records, err = Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Schema != legacySchemaVersion || records[1].MetadataChecksum != "" {
		t.Fatalf("legacy append changed format: %+v", records)
	}
}

func TestLegacyAppendOmitsScorecardFromWireFormat(t *testing.T) {
	root := t.TempDir()
	log, err := createLegacyLog(root, "game-1", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	recap := &scenario.Recap{Headline: "Review", EndReason: exchange.PlayerQuit, Scorecard: &scenario.Scorecard{FocusLabel: "Risk", FocusValue: "Low"}}
	if err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}, Recap: recap}); err != nil {
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
	data, err := os.ReadFile(filepath.Join(log.Path(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record legacyRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	checksumRecord := record
	checksumRecord.Checksum = ""
	checksumData, err := json.Marshal(checksumRecord)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append([]byte(record.PreviousChecksum), checksumData...))
	if record.Checksum != fmt.Sprintf("%x", digest) {
		t.Fatal("legacy checksum changed by scorecard field")
	}
	if record.Recap == nil || record.Recap.Headline != "Review" {
		t.Fatalf("legacy recap=%+v", record.Recap)
	}
}

func createLegacyLog(root, gameID string, cfg exchange.Config) (*Log, error) {
	dir := filepath.Join(root, gameID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	meta := Meta{Schema: legacySchemaVersion, GameID: gameID, OwnerID: "local", CreateCommandID: "create-1", CreatedAt: time.Now().UTC(), Config: cfg}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(data, '\n'), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		return nil, err
	}
	log, _, err := Open(root, gameID)
	return log, err
}
