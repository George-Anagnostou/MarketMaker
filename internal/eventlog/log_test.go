package eventlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
	"market-maker/internal/game"
	"market-maker/internal/realtime"
	"market-maker/internal/scenario"
	"os"
	"path/filepath"
	"reflect"
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
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandSubmitQuote, Bid: price(t, "99"), Ask: price(t, "101")}, Result: result}); err != nil {
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

func TestNewLogsUseSchema4AndV4ChecksumDomains(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}

	metaData, err := os.ReadFile(filepath.Join(log.Path(), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Meta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Schema != 4 || SchemaVersion != 4 || meta.Mode != game.PlayModeTurnBased || meta.RealTime != nil {
		t.Fatalf("metadata schema=%d current=%d", meta.Schema, SchemaVersion)
	}
	if got := independentMetadataChecksum(t, meta, "market-maker/eventlog/meta/v4\x00"); got != meta.Checksum {
		t.Fatalf("metadata checksum=%q want=%q", meta.Checksum, got)
	}

	recordData, err := os.ReadFile(filepath.Join(log.Path(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal(recordData, &record); err != nil {
		t.Fatal(err)
	}
	if record.Schema != 4 || record.MetadataChecksum != meta.Checksum {
		t.Fatalf("record is not schema 4 metadata-bound: %+v", record)
	}
	if got := independentRecordChecksum(t, record, "market-maker/eventlog/record/v4\x00"); got != record.Checksum {
		t.Fatalf("record checksum=%q want=%q", record.Checksum, got)
	}
}

func TestOpenAppendAndReopenSchema2WithFrozenDomains(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "game-1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := Meta{Schema: schema2, GameID: "game-1", OwnerID: "local", CreateCommandID: "create-1", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Config: testConfig(t)}
	meta.Checksum = independentMetadataChecksum(t, meta, "market-maker/eventlog/meta/v2\x00")
	metaData, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(metaData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	first := Record{Schema: schema2, Version: 1, MetadataChecksum: meta.Checksum, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}
	first.Checksum = independentRecordChecksum(t, first, "market-maker/eventlog/record/v2\x00")
	firstData, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), append(firstData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	log, records, err := Open(root, "game-1")
	if err != nil || len(records) != 1 {
		t.Fatalf("open schema 2: records=%+v err=%v", records, err)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}

	eventsData, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(eventsData), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("event lines=%d", len(lines))
	}
	for i, line := range lines {
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Schema != schema2 || record.MetadataChecksum != meta.Checksum {
			t.Fatalf("schema 2 record %d changed format: %+v", i+1, record)
		}
		if got := independentRecordChecksum(t, record, "market-maker/eventlog/record/v2\x00"); got != record.Checksum {
			t.Fatalf("schema 2 record %d checksum=%q want=%q", i+1, record.Checksum, got)
		}
	}
	_, records, err = Open(root, "game-1")
	if err != nil || len(records) != 2 || records[1].Command.ID != "c-2" {
		t.Fatalf("reopen schema 2: records=%+v err=%v", records, err)
	}

	var tampered Record
	if err := json.Unmarshal(lines[1], &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.MetadataChecksum = strings.Repeat("0", 64)
	tampered.Checksum = independentRecordChecksum(t, tampered, "market-maker/eventlog/record/v2\x00")
	tamperedData, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), append(append(lines[0], '\n'), append(tamperedData, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("expected schema 2 metadata-binding rejection")
	}
}

func TestOpenAppendAndReopenSchema3WithFrozenDomains(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "game-1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := Meta{Schema: schema3, GameID: "game-1", OwnerID: "local", CreateCommandID: "create-1", CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Config: testConfig(t)}
	meta.Checksum = independentMetadataChecksum(t, meta, "market-maker/eventlog/meta/v3\x00")
	metaData, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(metaData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	first := Record{Schema: schema3, Version: 1, MetadataChecksum: meta.Checksum, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}
	first.Checksum = independentRecordChecksum(t, first, "market-maker/eventlog/record/v3\x00")
	firstData, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), append(firstData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	log, records, err := Open(root, "game-1")
	if err != nil || len(records) != 1 || log.Meta().EffectiveMode() != game.PlayModeTurnBased {
		t.Fatalf("open schema 3: records=%+v err=%v", records, err)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	_, records, err = Open(root, "game-1")
	if err != nil || len(records) != 2 || records[1].Schema != schema3 {
		t.Fatalf("reopen schema 3: records=%+v err=%v", records, err)
	}
	if got := independentRecordChecksum(t, records[1], "market-maker/eventlog/record/v3\x00"); got != records[1].Checksum {
		t.Fatalf("schema 3 checksum=%q want=%q", records[1].Checksum, got)
	}
}

func TestOpenRejectsUnknownSchema(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "game-1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaData, err := json.Marshal(Meta{Schema: 5, GameID: "game-1", OwnerID: "local", CreateCommandID: "create-1", CreatedAt: time.Now().UTC(), Config: testConfig(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), append(metaData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("expected unknown schema rejection")
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

func TestReadMetaDoesNotReadOrMutateEvents(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(log.Path(), "events.jsonl")
	events := []byte(`not an event stream`)
	if err := os.WriteFile(eventsPath, events, 0o600); err != nil {
		t.Fatal(err)
	}

	meta, err := ReadMeta(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if meta.GameID != "game-1" || meta.CreateCommandID != "create-1" || !bytes.Equal(after, events) {
		t.Fatalf("metadata=%+v events=%q", meta, after)
	}
}

func TestReadMetaRejectsInvalidChecksum(t *testing.T) {
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
	meta.OwnerID = "changed"
	data, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMeta(root, "game-1"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("ReadMeta error = %v, want checksum rejection", err)
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
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandSubmitQuote, Bid: price(t, "99"), Ask: price(t, "101")}, Result: exchange.Result{}}); err != nil {
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
	if _, err := log.Append(record); err != nil {
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
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
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
	if _, err := log.Append(record); err != nil {
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
	snapshot := &scenario.Snapshot{ID: "lesson", Tutorial: []scenario.TutorialStep{{Title: "Original", Body: "Original body"}}, Modes: []game.PlayMode{game.PlayModeTurnBased}, RealTime: &scenario.RealTimeConfig{DurationMilliseconds: 90_000}}
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Tutorial[0].Title = "Mutated input"
	snapshot.Modes[0] = game.PlayModeRealTime
	snapshot.RealTime.DurationMilliseconds = 1
	meta := log.Meta()
	if meta.Scenario.Tutorial[0].Title != "Original" || meta.Scenario.Modes[0] != game.PlayModeTurnBased || meta.Scenario.RealTime.DurationMilliseconds != 90_000 {
		t.Fatalf("stored scenario=%+v", meta.Scenario)
	}
	meta.Scenario.Tutorial[0].Title = "Mutated output"
	meta.Scenario.Modes[0] = game.PlayModeRealTime
	meta.Scenario.RealTime.DurationMilliseconds = 1
	got := log.Meta().Scenario
	if got.Tutorial[0].Title != "Original" || got.Modes[0] != game.PlayModeTurnBased || got.RealTime.DurationMilliseconds != 90_000 {
		t.Fatalf("metadata mutation leaked: %+v", got)
	}
}

func TestCreateRealTimePreparingMetadata(t *testing.T) {
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	snapshot := definition.Snapshot()
	root := t.TempDir()
	log, err := CreateRealTime(root, "game-1", "local", "create-1", definition.Config, &snapshot, 42)
	if err != nil {
		t.Fatal(err)
	}
	meta := log.Meta()
	if meta.EffectiveMode() != game.PlayModeRealTime || meta.RealTime == nil || meta.RealTime.Lifecycle != game.LifecyclePreparing || meta.RealTime.LifecycleVersion != game.LifecycleVersion || meta.RealTime.GeneratorVersion != game.GeneratorVersion || meta.RealTime.Seed != 42 {
		t.Fatalf("real-time metadata=%+v", meta)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err == nil {
		t.Fatal("real-time preparing log accepted a record")
	}
	meta.RealTime.Lifecycle = "mutated"
	if log.Meta().RealTime.Lifecycle != game.LifecyclePreparing {
		t.Fatal("real-time metadata aliases caller")
	}
	reopened, records, err := Open(root, "game-1")
	if err != nil || len(records) != 0 || reopened.Meta().EffectiveMode() != game.PlayModeRealTime {
		t.Fatalf("reopen real-time records=%+v err=%v", records, err)
	}
}

func TestAppendAndOpenRealTimeActionsAndRejections(t *testing.T) {
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	snapshot := definition.Snapshot()
	root := t.TempDir()
	log, err := CreateRealTime(root, "game-1", "local", "create-1", definition.Config, &snapshot, 42)
	if err != nil {
		t.Fatal(err)
	}
	start, err := realtime.EncodeAction(realtime.Action{ID: "start-1", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown})
	if err != nil {
		t.Fatal(err)
	}
	countdown, err := realtime.EncodeAction(realtime.Action{ID: "system/countdown-1", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 2, Action: &countdown, Lifecycle: game.LifecycleRunning}); err != nil {
		t.Fatal(err)
	}
	revision := uint64(1)
	quote, err := realtime.EncodeAction(realtime.Action{ID: "quote-1", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99"), Ask: price(t, "101"), ExpectedRevision: &revision}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 3, Action: &quote, ElapsedNanoseconds: int64(3 * time.Second), Lifecycle: game.LifecycleRunning, Result: exchange.Result{State: exchange.State{Version: 1}}}); err != nil {
		t.Fatal(err)
	}
	mark, err := realtime.EncodeAction(realtime.Action{ID: "mark-1", Kind: realtime.ActionMarkMove, Source: realtime.SourceSystem, Payload: realtime.MarkMovePayload{BasisPoints: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 4, Action: &mark, ElapsedNanoseconds: int64(4 * time.Second), Lifecycle: game.LifecycleRunning, Result: exchange.Result{State: exchange.State{Version: 2}}}); err != nil {
		t.Fatal(err)
	}
	rejectedRevision := uint64(2)
	rejectedAction, err := realtime.EncodeAction(realtime.Action{ID: "quote-2", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "101"), Ask: price(t, "99"), ExpectedRevision: &rejectedRevision}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 5, Action: &rejectedAction, ElapsedNanoseconds: int64(5 * time.Second), Lifecycle: game.LifecycleRunning, Result: exchange.Result{State: exchange.State{Version: 2}}, Rejection: &ActionRejection{Code: "command_rejected", Message: "crossed quote"}}); err != nil {
		t.Fatal(err)
	}
	if accepted.Schema != RealTimeSchemaVersion || accepted.MetadataChecksum != log.Meta().Checksum {
		t.Fatalf("accepted=%+v", accepted)
	}
	reopened, records, err := Open(root, "game-1")
	if err != nil || len(records) != 5 || reopened.Meta().Schema != RealTimeSchemaVersion || records[4].Rejection == nil || records[4].Result.State.Version != 2 || reopened.lifecycle != game.LifecycleRunning || reopened.lastElapsedNanoseconds != int64(5*time.Second) {
		t.Fatalf("records=%+v err=%v", records, err)
	}
	if _, err := reopened.Append(Record{Schema: RealTimeSchemaVersion, Version: 6, Action: &rejectedAction, ElapsedNanoseconds: int64(6 * time.Second), Lifecycle: game.LifecycleRunning, Rejection: &ActionRejection{Code: "again", Message: "again"}}); err == nil {
		t.Fatal("duplicate rejected action id accepted")
	}
	if got := independentMetadataChecksum(t, log.Meta(), "market-maker/eventlog/meta/v6\x00"); got != log.Meta().Checksum {
		t.Fatalf("metadata checksum=%q want=%q", log.Meta().Checksum, got)
	}
	if got := independentRecordChecksum(t, records[0], "market-maker/eventlog/record/v6\x00"); got != records[0].Checksum {
		t.Fatalf("record checksum=%q want=%q", records[0].Checksum, got)
	}
}

func TestRealTimeRecordValidation(t *testing.T) {
	definition, _ := scenario.Get("first-spread-v1")
	snapshot := definition.Snapshot()
	newLog := func(t *testing.T) *Log {
		log, err := CreateRealTime(t.TempDir(), "game-1", "local", "create-1", definition.Config, &snapshot, 42)
		if err != nil {
			t.Fatal(err)
		}
		return log
	}
	system, _ := realtime.EncodeAction(realtime.Action{ID: "system", Kind: realtime.ActionMarkMove, Source: realtime.SourceSystem, Payload: realtime.MarkMovePayload{BasisPoints: 1}})
	participant, _ := realtime.EncodeAction(realtime.Action{ID: "participant", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	start, _ := realtime.EncodeAction(realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	tests := map[string]Record{
		"missing action":     {Schema: RealTimeSchemaVersion, Version: 1},
		"missing lifecycle":  {Schema: RealTimeSchemaVersion, Version: 1, Action: &start},
		"wrong lifecycle":    {Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleRunning},
		"negative elapsed":   {Schema: RealTimeSchemaVersion, Version: 1, Action: &system, ElapsedNanoseconds: -1},
		"system rejection":   {Schema: RealTimeSchemaVersion, Version: 1, Action: &system, Rejection: &ActionRejection{Code: "bad", Message: "bad"}},
		"empty rejection":    {Schema: RealTimeSchemaVersion, Version: 1, Action: &participant, Rejection: &ActionRejection{}},
		"command and action": {Schema: RealTimeSchemaVersion, Version: 1, Action: &participant, Command: exchange.Command{ID: "command"}},
	}
	for name, record := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := newLog(t).Append(record); err == nil {
				t.Fatal("invalid record accepted")
			}
		})
	}
}

func TestRealTimeLifecycleTransitions(t *testing.T) {
	log := newRealTimeTestLog(t)
	state := exchange.State{Version: 7}

	rejectedStart := durableTestAction(t, realtime.Action{ID: "start-rejected", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "101"), Ask: price(t, "99")}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &rejectedStart, Lifecycle: game.LifecyclePreparing, Result: exchange.Result{State: state}, Rejection: &ActionRejection{Code: RejectionCodeCommandRejected, Message: "crossed quote"}}); err != nil {
		t.Fatalf("rejected start: %v", err)
	}
	start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 2, Action: &start, Lifecycle: game.LifecycleCountdown, Result: exchange.Result{State: state}}); err != nil {
		t.Fatalf("accepted start: %v", err)
	}
	countdown := durableTestAction(t, realtime.Action{ID: "system/countdown", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 3, Action: &countdown, Lifecycle: game.LifecycleRunning, Result: exchange.Result{State: state}}); err != nil {
		t.Fatalf("countdown completion: %v", err)
	}
	pause := durableTestAction(t, realtime.Action{ID: "pause", Kind: realtime.ActionPauseSession, Source: realtime.SourceParticipant, Payload: realtime.PauseSessionPayload{Reason: realtime.PauseReasonPlayer}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 4, Action: &pause, ElapsedNanoseconds: 3, Lifecycle: game.LifecyclePaused, Result: exchange.Result{State: state}}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	resume := durableTestAction(t, realtime.Action{ID: "resume", Kind: realtime.ActionResumeSession, Source: realtime.SourceParticipant})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 5, Action: &resume, ElapsedNanoseconds: 4, Lifecycle: game.LifecycleRunning, Result: exchange.Result{State: state}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	revision := uint64(1)
	quote := durableTestAction(t, realtime.Action{ID: "quote", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99"), Ask: price(t, "101"), ExpectedRevision: &revision}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 6, Action: &quote, ElapsedNanoseconds: 5, Lifecycle: game.LifecycleCompleted, Result: exchange.Result{State: exchange.State{IsOver: true}}}); err != nil {
		t.Fatalf("completing quote: %v", err)
	}
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 7, Action: &quote, ElapsedNanoseconds: 6, Lifecycle: game.LifecycleCompleted}); err == nil {
		t.Fatal("record after completion accepted")
	}

	_, records, err := Open(filepath.Dir(log.Path()), "game-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 1, 2, 3, 4} {
		if records[index].Result.State != state {
			t.Fatalf("lifecycle-only record %d changed exchange state: %+v", index+1, records[index].Result.State)
		}
	}
}

func TestPauseDuringCountdownAndSourceReasonGrammar(t *testing.T) {
	log := newRealTimeTestLog(t)
	start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
		t.Fatal(err)
	}
	badPause := realtime.DurableAction{ID: "system/bad-pause", Kind: realtime.ActionPauseSession, Source: realtime.SourceSystem, Payload: json.RawMessage(`{"reason":"player"}`)}
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 2, Action: &badPause, Lifecycle: game.LifecyclePaused}); err == nil {
		t.Fatal("system pause with player reason accepted")
	}
	pause := durableTestAction(t, realtime.Action{ID: "system/pause", Kind: realtime.ActionPauseSession, Source: realtime.SourceSystem, Payload: realtime.PauseSessionPayload{Reason: realtime.PauseReasonRecovery}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 2, Action: &pause, Lifecycle: game.LifecyclePaused}); err != nil {
		t.Fatalf("countdown pause: %v", err)
	}
}

func TestCountdownLifecycleRejectsMarketElapsedTime(t *testing.T) {
	for _, test := range []struct {
		name      string
		prepare   func(*testing.T, *Log)
		action    realtime.Action
		lifecycle game.LifecycleState
	}{
		{name: "start", action: realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}}, lifecycle: game.LifecycleCountdown},
		{name: "complete", prepare: func(t *testing.T, log *Log) {
			start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
			if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
				t.Fatal(err)
			}
		}, action: realtime.Action{ID: "system/countdown", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem}, lifecycle: game.LifecycleRunning},
		{name: "pause", prepare: func(t *testing.T, log *Log) {
			start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
			if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
				t.Fatal(err)
			}
		}, action: realtime.Action{ID: "system/pause", Kind: realtime.ActionPauseSession, Source: realtime.SourceSystem, Payload: realtime.PauseSessionPayload{Reason: realtime.PauseReasonRecovery}}, lifecycle: game.LifecyclePaused},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := newRealTimeTestLog(t)
			if test.prepare != nil {
				test.prepare(t, log)
			}
			action := durableTestAction(t, test.action)
			if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: log.nextVersion, Action: &action, ElapsedNanoseconds: 1, Lifecycle: test.lifecycle}); err == nil {
				t.Fatal("countdown lifecycle accepted nonzero market elapsed time")
			}
		})
	}
}

func TestRealTimeSystemEventLifecycleOutcomes(t *testing.T) {
	tests := []struct {
		name      string
		action    realtime.Action
		isOver    bool
		lifecycle game.LifecycleState
	}{
		{name: "customer", action: realtime.Action{ID: "system/customer", Kind: realtime.ActionCustomerArrival, Source: realtime.SourceSystem, Payload: realtime.CustomerArrivalPayload{Buy: true, Quantity: 1, HasUpcomingMark: false}}, lifecycle: game.LifecycleRunning},
		{name: "mark", action: realtime.Action{ID: "system/mark", Kind: realtime.ActionMarkMove, Source: realtime.SourceSystem, Payload: realtime.MarkMovePayload{BasisPoints: 1}}, lifecycle: game.LifecycleRunning},
		{name: "carry", action: realtime.Action{ID: "system/carry", Kind: realtime.ActionCarryCharge, Source: realtime.SourceSystem}, lifecycle: game.LifecycleRunning},
		{name: "expiry", action: realtime.Action{ID: "system/expiry", Kind: realtime.ActionTimeExpired, Source: realtime.SourceSystem}, isOver: true, lifecycle: game.LifecycleCompleted},
		{name: "mark completes", action: realtime.Action{ID: "system/mark", Kind: realtime.ActionMarkMove, Source: realtime.SourceSystem, Payload: realtime.MarkMovePayload{BasisPoints: 1}}, isOver: true, lifecycle: game.LifecycleCompleted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			log := runningRealTimeTestLog(t)
			action := durableTestAction(t, test.action)
			if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 3, Action: &action, Lifecycle: test.lifecycle, Result: exchange.Result{State: exchange.State{IsOver: test.isOver}}}); err != nil {
				t.Fatal(err)
			}
		})
	}

	log := runningRealTimeTestLog(t)
	expiry := durableTestAction(t, realtime.Action{ID: "system/expiry", Kind: realtime.ActionTimeExpired, Source: realtime.SourceSystem})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 3, Action: &expiry, Lifecycle: game.LifecycleRunning}); err == nil {
		t.Fatal("non-completing time expiry accepted")
	}
}

func TestRealTimeRejectsBackwardElapsedAppendAndColdOpen(t *testing.T) {
	log := newRealTimeTestLog(t)
	start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
		t.Fatal(err)
	}
	countdown := durableTestAction(t, realtime.Action{ID: "system/countdown", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 2, Action: &countdown, Lifecycle: game.LifecycleRunning}); err != nil {
		t.Fatal(err)
	}
	revision := uint64(1)
	quote := durableTestAction(t, realtime.Action{ID: "quote", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99"), Ask: price(t, "101"), ExpectedRevision: &revision}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 3, Action: &quote, ElapsedNanoseconds: 2, Lifecycle: game.LifecycleRunning}); err != nil {
		t.Fatal(err)
	}
	pause := durableTestAction(t, realtime.Action{ID: "pause", Kind: realtime.ActionPauseSession, Source: realtime.SourceParticipant, Payload: realtime.PauseSessionPayload{Reason: realtime.PauseReasonPlayer}})
	backward := Record{Schema: RealTimeSchemaVersion, Version: 4, Action: &pause, ElapsedNanoseconds: 1, Lifecycle: game.LifecyclePaused}
	if _, err := log.Append(backward); err == nil {
		t.Fatal("backward elapsed append accepted")
	}
	appendFile(t, filepath.Join(log.Path(), "events.jsonl"), encodedRecordLine(t, log, backward))
	if _, _, err := Open(filepath.Dir(log.Path()), "game-1"); err == nil {
		t.Fatal("cold open accepted backward elapsed record")
	}
}

func TestHistoricalSchemasRejectLifecycle(t *testing.T) {
	for _, schema := range []int{schema1, schema2, schema3, SchemaVersion} {
		meta := Meta{Schema: schema, Mode: game.PlayModeTurnBased}
		if schema <= schema3 {
			meta.Mode = ""
		}
		record := Record{Schema: schema, Version: 1, Command: exchange.Command{ID: "command"}, Lifecycle: game.LifecycleRunning}
		if _, err := validateRecordInput(record, meta, 1, 0, ""); err == nil {
			t.Fatalf("schema %d accepted lifecycle", schema)
		}
	}
}

func TestRecoverMatchesAcknowledgedRealTimeProjection(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Log)
	}{
		{name: "elapsed", mutate: func(log *Log) { log.lastElapsedNanoseconds++ }},
		{name: "lifecycle", mutate: func(log *Log) { log.lifecycle = game.LifecycleRunning }},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := newRealTimeTestLog(t)
			start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
			if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
				t.Fatal(err)
			}
			test.mutate(log)
			if _, err := log.Recover(); err == nil || !strings.Contains(err.Error(), "does not match active state") {
				t.Fatalf("Recover error = %v, want projection mismatch", err)
			}
		})
	}
}

func TestOpenRejectsRealTimeActionUsingCreateCommandID(t *testing.T) {
	definition, _ := scenario.Get("first-spread-v1")
	snapshot := definition.Snapshot()
	root := t.TempDir()
	log, err := CreateRealTime(root, "game-1", "local", "create-1", definition.Config, &snapshot, 42)
	if err != nil {
		t.Fatal(err)
	}
	action, err := realtime.EncodeAction(realtime.Action{ID: "create-1", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Schema: RealTimeSchemaVersion, Version: 1, MetadataChecksum: log.Meta().Checksum, Action: &action, Result: exchange.Result{State: exchange.State{Version: 1}}}
	record.Checksum, err = recordChecksum(record)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(log.Path(), "events.jsonl"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil {
		t.Fatal("cold open accepted action using create command id")
	}
}

func TestMetadataModeValidation(t *testing.T) {
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	snapshot := definition.Snapshot()
	validRealTime := Meta{
		Schema:   RealTimeSchemaVersion,
		Config:   definition.Config,
		Scenario: &snapshot,
		Mode:     game.PlayModeRealTime,
		RealTime: &RealTimeMeta{LifecycleVersion: game.LifecycleVersion, Lifecycle: game.LifecyclePreparing, GeneratorVersion: game.GeneratorVersion, Seed: 42},
	}
	if err := validateMeta(validRealTime); err != nil {
		t.Fatalf("valid real-time metadata rejected: %v", err)
	}
	tests := map[string]func(*Meta){
		"missing mode":            func(meta *Meta) { meta.Mode = "" },
		"missing lifecycle":       func(meta *Meta) { meta.RealTime = nil },
		"missing scenario":        func(meta *Meta) { meta.Scenario = nil },
		"missing scenario config": func(meta *Meta) { meta.Scenario.RealTime = nil },
		"lifecycle version":       func(meta *Meta) { meta.RealTime.LifecycleVersion++ },
		"lifecycle state":         func(meta *Meta) { meta.RealTime.Lifecycle = "running" },
		"generator version":       func(meta *Meta) { meta.RealTime.GeneratorVersion++ },
		"seed":                    func(meta *Meta) { meta.RealTime.Seed = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			meta := validRealTime
			scenarioCopy := *validRealTime.Scenario
			realTimeConfig := *validRealTime.Scenario.RealTime
			scenarioCopy.RealTime = &realTimeConfig
			meta.Scenario = &scenarioCopy
			realTimeMeta := *validRealTime.RealTime
			meta.RealTime = &realTimeMeta
			mutate(&meta)
			if err := validateMeta(meta); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	turnBased := validRealTime
	turnBased.Schema = SchemaVersion
	turnBased.Mode = game.PlayModeTurnBased
	turnBased.RealTime = nil
	if err := validateMeta(turnBased); err != nil {
		t.Fatalf("valid turn-based metadata rejected: %v", err)
	}
	turnBased.RealTime = validRealTime.RealTime
	if err := validateMeta(turnBased); err == nil {
		t.Fatal("turn-based metadata accepted real-time lifecycle")
	}
	historical := Meta{Schema: schema3, Mode: game.PlayModeTurnBased}
	if err := validateMeta(historical); err == nil {
		t.Fatal("historical schema accepted mode field")
	}
}

func TestOpenRejectsTamperedSchema3Metadata(t *testing.T) {
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

func TestOpenRejectsUnknownMetadataFields(t *testing.T) {
	for _, test := range []struct {
		name string
		path []string
	}{
		{name: "top-level"},
		{name: "nested", path: []string{"config"}},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			data = addUnknownJSONField(t, data, test.path...)
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Open error = %v, want unknown field rejection", err)
			}
		})
	}
}

func TestOpenRejectsDuplicateMetadataKeys(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "top-level",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte("{"), []byte(`{"schema":3,`), 1)
			},
		},
		{
			name: "nested",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"config":{`), []byte(`"config":{"instrument":"SIM",`), 1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			data = test.mutate(bytes.TrimSpace(data))
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
				t.Fatalf("Open error = %v, want duplicate key rejection", err)
			}
		})
	}
}

func TestOpenRejectsTrailingMetadataJSON(t *testing.T) {
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
	if err := os.WriteFile(path, append(data, []byte("{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "exactly one JSON value") {
		t.Fatalf("Open error = %v, want trailing JSON rejection", err)
	}
}

func TestOpenRejectsSchema3RecordBoundToDifferentMetadata(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
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
	record.Checksum = independentRecordChecksum(t, record, "market-maker/eventlog/record/v3\x00")
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

func TestOpenRejectsUnknownRecordFields(t *testing.T) {
	for _, test := range []struct {
		name string
		path []string
	}{
		{name: "top-level"},
		{name: "nested", path: []string{"command"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(log.Path(), "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = addUnknownJSONField(t, data, test.path...)
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Open error = %v, want unknown field rejection", err)
			}
		})
	}
}

func TestOpenRejectsDuplicateRecordKeys(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "top-level",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte("{"), []byte(`{"schema":3,`), 1)
			},
		},
		{
			name: "nested",
			mutate: func(data []byte) []byte {
				return bytes.Replace(data, []byte(`"command":{`), []byte(`"command":{"id":"c-1",`), 1)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(log.Path(), "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = test.mutate(bytes.TrimSpace(data))
			if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "duplicate JSON object key") {
				t.Fatalf("Open error = %v, want duplicate key rejection", err)
			}
		})
	}
}

func TestOpenRejectsTrailingRecordJSON(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(log.Path(), "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.TrimSuffix(data, []byte("\n"))
	if err := os.WriteFile(path, append(data, []byte(" {}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "exactly one JSON value") {
		t.Fatalf("Open error = %v, want trailing JSON rejection", err)
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
	if record, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); !errors.Is(err, os.ErrNotExist) || !reflect.DeepEqual(record, Record{}) {
		t.Fatalf("Append error = %v, want missing events error", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("events file was recreated: %v", err)
	}
}

func TestAppendRejectsActiveFileSizeAndIdentityChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, []byte)
	}{
		{
			name: "truncate",
			mutate: func(t *testing.T, path string, data []byte) {
				t.Helper()
				if err := os.Truncate(path, int64(len(data)-1)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "same-size replacement",
			mutate: func(t *testing.T, path string, data []byte) {
				t.Helper()
				replaceFile(t, path, data)
			},
		},
		{
			name: "extension",
			mutate: func(t *testing.T, path string, _ []byte) {
				t.Helper()
				appendFile(t, path, []byte("unacknowledged"))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(log.Path(), "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			acknowledgedSize := log.eventsSize
			test.mutate(t, path, data)

			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err == nil {
				t.Fatal("Append accepted changed active event log")
			}
			if log.eventsSize != acknowledgedSize || log.nextVersion != 2 || log.lastChecksum == "" {
				t.Fatalf("Append advanced active state after rejection: size=%d version=%d checksum=%q", log.eventsSize, log.nextVersion, log.lastChecksum)
			}
		})
	}
}

func TestSyncRejectsActiveFileReplacement(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(log.Path(), "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replaceFile(t, path, data)
	if err := log.Sync(); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("Sync error = %v, want identity rejection", err)
	}
}

func TestRecoverRetainsValidUncertainRecord(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	line := encodedRecordLine(t, log, Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}})
	path := filepath.Join(log.Path(), "events.jsonl")
	appendFile(t, path, line)

	records, err := log.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Command.ID != "c-1" || log.eventsSize != int64(len(line)) || log.nextVersion != 2 {
		t.Fatalf("recovered records=%+v size=%d version=%d", records, log.eventsSize, log.nextVersion)
	}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
		t.Fatalf("Append after Recover: %v", err)
	}
}

func TestRecoverTruncatesMalformedSuffix(t *testing.T) {
	for _, test := range []struct {
		name   string
		suffix []byte
	}{
		{name: "newline terminated", suffix: []byte("{\"bad\":true}\n")},
		{name: "unterminated", suffix: []byte("{\"schema\":")},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(log.Path(), "events.jsonl")
			acknowledged, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			appendFile(t, path, test.suffix)

			records, err := log.Recover()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || !bytes.Equal(data, acknowledged) || log.eventsSize != int64(len(acknowledged)) {
				t.Fatalf("records=%+v data size=%d acknowledged=%d active=%d", records, len(data), len(acknowledged), log.eventsSize)
			}
			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
				t.Fatalf("Append after Recover: %v", err)
			}
		})
	}
}

func TestRecoverRefusesRollbackAndReplacement(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, []byte)
	}{
		{
			name: "rollback",
			mutate: func(t *testing.T, path string, data []byte) {
				t.Helper()
				if err := os.Truncate(path, int64(len(data)-1)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{name: "replacement", mutate: replaceFile},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(log.Path(), "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, path, data)
			if records, err := log.Recover(); err == nil || records != nil {
				t.Fatalf("Recover records=%+v error=%v, want refusal", records, err)
			}
			if log.eventsSize != int64(len(data)) || log.nextVersion != 2 {
				t.Fatalf("Recover changed active state: size=%d version=%d", log.eventsSize, log.nextVersion)
			}
		})
	}
}

func TestCreateDurablyCreatesAbsentStorageRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new", "eventlogs")
	if _, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(root), root} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory %s permissions = %o, want no group/other access", path, info.Mode().Perm())
		}
	}
}

func TestRejectsDuplicateCommandID(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}
	if _, err := log.Append(record); err != nil {
		t.Fatal(err)
	}
	record.Version = 2
	if _, err := log.Append(record); err == nil {
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

func TestOpenAcceptsHistoricalCreateCommandIDCollision(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "create-1", Type: exchange.CommandQuit}}
	if _, err := log.Append(record); err == nil {
		t.Fatal("Append accepted the create command id")
	}

	record.MetadataChecksum = log.Meta().Checksum
	record.Checksum, err = recordChecksum(record)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(log.Path(), "events.jsonl"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, records, err := Open(root, "game-1")
	if err != nil || len(records) != 1 || records[0].Command.ID != "create-1" {
		t.Fatalf("Open records = %+v, error = %v", records, err)
	}
	if _, err := reopened.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "create-1", Type: exchange.CommandQuit}}); err == nil {
		t.Fatal("Append accepted the create command id after historical replay")
	}
}

func TestCreateEnforcesMetadataWriteLimit(t *testing.T) {
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 678901234, time.UTC)
	base := Meta{Schema: SchemaVersion, GameID: "game-1", OwnerID: "", CreateCommandID: "create-1", CreatedAt: createdAt, Config: testConfig(t), Mode: game.PlayModeTurnBased}
	checksum, err := metadataChecksum(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Checksum = checksum
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	ownerBytes := int(maxMetadataBytes) - len(data) - 1
	if ownerBytes < 1 {
		t.Fatal("metadata fixture already exceeds limit")
	}

	root := t.TempDir()
	log, err := createAt(root, "game-1", strings.Repeat("x", ownerBytes), "create-1", testConfig(t), nil, createdAt)
	if err != nil {
		t.Fatalf("boundary Create error = %v", err)
	}
	info, err := os.Stat(filepath.Join(log.Path(), "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != maxMetadataBytes {
		t.Fatalf("metadata size = %d, want %d", info.Size(), maxMetadataBytes)
	}

	oversizeRoot := t.TempDir()
	if _, err := createAt(oversizeRoot, "game-1", strings.Repeat("x", ownerBytes+1), "create-1", testConfig(t), nil, createdAt); err == nil || !strings.Contains(err.Error(), "game metadata exceeds") {
		t.Fatalf("oversize Create error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(oversizeRoot, "game-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversize game was published: %v", err)
	}
}

func TestAppendEnforcesEventWriteLimit(t *testing.T) {
	for _, test := range []struct {
		name       string
		extraBytes int64
		wantError  bool
	}{
		{name: "boundary"},
		{name: "oversize", extraBytes: 1, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			record := Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}}
			lineBytes := encodedRecordLineBytes(t, log, record)
			path := filepath.Join(log.Path(), "events.jsonl")
			initialSize := maxEventsBytes - lineBytes + test.extraBytes
			if err := os.Truncate(path, initialSize); err != nil {
				t.Fatal(err)
			}
			// This fixture establishes an already acknowledged sparse boundary;
			// active Logs now correctly reject unacknowledged external growth.
			log.eventsSize = initialSize
			_, err = log.Append(record)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "event log exceeds") {
					t.Fatalf("Append error = %v, want size rejection", err)
				}
			} else if err != nil {
				t.Fatalf("boundary Append error = %v", err)
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			wantSize := initialSize
			if !test.wantError {
				wantSize = maxEventsBytes
			}
			if info.Size() != wantSize {
				t.Fatalf("event size = %d, want %d", info.Size(), wantSize)
			}
		})
	}
}

func TestOpenRejectsOversizedMetadata(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(log.Path(), "meta.json"), maxMetadataBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "game metadata exceeds 1048576-byte limit") {
		t.Fatalf("Open error = %v, want metadata size limit", err)
	}
}

func TestOpenRejectsOversizedEvents(t *testing.T) {
	root := t.TempDir()
	log, err := Create(root, "game-1", "local", "create-1", testConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(log.Path(), "events.jsonl"), maxEventsBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Open(root, "game-1"); err == nil || !strings.Contains(err.Error(), "event log exceeds 67108864-byte limit") {
		t.Fatalf("Open error = %v, want event log size limit", err)
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
	record := legacyRecord{Schema: schema1, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}, Recap: &legacyRecap{Headline: "Review", EndReason: exchange.PlayerQuit}}
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
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}}); err != nil {
		t.Fatal(err)
	}
	_, records, err = Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[1].Schema != schema1 || records[1].MetadataChecksum != "" {
		t.Fatalf("legacy append changed format: %+v", records)
	}
}

func TestAppendReturnsSchemaProjectedDurableRecord(t *testing.T) {
	root := t.TempDir()
	log, err := createLegacyLog(root, "game-1", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}})
	if err != nil {
		t.Fatal(err)
	}
	recap := &scenario.Recap{Headline: "Review", EndReason: exchange.PlayerQuit, AdverseSelectionTurns: 3, Scorecard: &scenario.Scorecard{FocusLabel: "Risk", FocusValue: "Low"}, InformedOrders: 2, InformedOrdersFilled: 1, InformedUnitsTraded: qty(t, "1"), InformedFlowPnL: money(t, "-1")}
	input := Record{Schema: SchemaVersion, Version: 2, Command: exchange.Command{ID: "c-2", Type: exchange.CommandQuit}, Result: exchange.Result{State: exchange.State{Version: 2}}, Recap: recap}
	appended, err := log.Append(input)
	if err != nil {
		t.Fatal(err)
	}
	if appended.Schema != schema1 || appended.PreviousChecksum != first.Checksum || appended.Checksum == "" || appended.MetadataChecksum != "" {
		t.Fatalf("returned record metadata was not projected: %+v", appended)
	}
	if appended.Recap == nil || appended.Recap.AdverseSelectionTurns != 0 || appended.Recap.Scorecard != nil || appended.Recap.InformedOrders != 0 || appended.Recap.InformedOrdersFilled != 0 || appended.Recap.InformedUnitsTraded != 0 || appended.Recap.InformedFlowPnL != 0 {
		t.Fatalf("returned recap was not projected: %+v", appended.Recap)
	}
	if recap.Scorecard == nil || recap.AdverseSelectionTurns != 3 || recap.InformedOrders != 2 {
		t.Fatalf("Append mutated its input recap: %+v", recap)
	}
	_, records, err := Open(root, "game-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !reflect.DeepEqual(records[1], appended) {
		t.Fatalf("returned record differs from durable record: returned=%+v records=%+v", appended, records)
	}
}

func TestLegacyAppendOmitsScorecardFromWireFormat(t *testing.T) {
	root := t.TempDir()
	log, err := createLegacyLog(root, "game-1", testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	recap := &scenario.Recap{Headline: "Review", EndReason: exchange.PlayerQuit, AdverseSelectionTurns: 3, Scorecard: &scenario.Scorecard{FocusLabel: "Risk", FocusValue: "Low"}, InformedOrders: 2, InformedOrdersFilled: 1, InformedUnitsTraded: qty(t, "1"), InformedFlowPnL: money(t, "-1")}
	if _, err := log.Append(Record{Schema: SchemaVersion, Version: 1, Command: exchange.Command{ID: "c-1", Type: exchange.CommandQuit}, Recap: recap}); err != nil {
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
	meta := Meta{Schema: schema1, GameID: gameID, OwnerID: "local", CreateCommandID: "create-1", CreatedAt: time.Now().UTC(), Config: cfg}
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

func newRealTimeTestLog(t *testing.T) *Log {
	t.Helper()
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	snapshot := definition.Snapshot()
	log, err := CreateRealTime(t.TempDir(), "game-1", "local", "create-1", definition.Config, &snapshot, 42)
	if err != nil {
		t.Fatal(err)
	}
	return log
}

func TestSchema5RealTimeLogRetainsFrozenGrammar(t *testing.T) {
	definition, ok := scenario.Get("first-spread-v1")
	if !ok {
		t.Fatal("missing first scenario")
	}
	root := t.TempDir()
	snapshot := definition.Snapshot()
	log, err := CreateRealTime(root, "game-1", "local", "create-1", definition.Config, &snapshot, 42)
	if err != nil {
		t.Fatal(err)
	}
	meta := log.Meta()
	meta.Schema = realTimeSchema5
	meta.Checksum, err = metadataChecksum(meta)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(log.Path(), "meta.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, records, err := Open(root, "game-1")
	if err != nil || len(records) != 0 || legacy.Meta().Schema != realTimeSchema5 {
		t.Fatalf("legacy schema=%d records=%d err=%v", legacy.Meta().Schema, len(records), err)
	}
	start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if _, err := legacy.Append(Record{Schema: realTimeSchema5, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
		t.Fatal(err)
	}
	countdown := durableTestAction(t, realtime.Action{ID: "system/countdown", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem})
	if _, err := legacy.Append(Record{Schema: realTimeSchema5, Version: 2, Action: &countdown, Lifecycle: game.LifecycleRunning}); err != nil {
		t.Fatal(err)
	}
	quote := durableTestAction(t, realtime.Action{ID: "quote", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99.5"), Ask: price(t, "100.5")}})
	if _, err := legacy.Append(Record{Schema: realTimeSchema5, Version: 3, Action: &quote, Lifecycle: game.LifecycleRunning, Result: exchange.Result{State: exchange.State{Version: 1}}}); err != nil {
		t.Fatal(err)
	}
	revision := uint64(0)
	newQuote := durableTestAction(t, realtime.Action{ID: "new-quote", Kind: realtime.ActionUpdateQuote, Source: realtime.SourceParticipant, Payload: realtime.UpdateQuotePayload{Bid: price(t, "99.6"), Ask: price(t, "100.4"), ExpectedRevision: &revision}})
	if _, err := legacy.Append(Record{Schema: realTimeSchema5, Version: 4, Action: &newQuote, Lifecycle: game.LifecycleRunning}); err == nil {
		t.Fatal("schema 5 accepted quote revision")
	}
	quit := durableTestAction(t, realtime.Action{ID: "quit", Kind: realtime.ActionQuitSession, Source: realtime.SourceParticipant})
	if _, err := legacy.Append(Record{Schema: realTimeSchema5, Version: 4, Action: &quit, Lifecycle: game.LifecycleCompleted, Result: exchange.Result{State: exchange.State{IsOver: true}}}); err == nil {
		t.Fatal("schema 5 accepted quit")
	}
	reopened, records, err := Open(root, "game-1")
	if err != nil || len(records) != 3 || reopened.Meta().Schema != realTimeSchema5 {
		t.Fatalf("reopened schema=%d records=%d err=%v", reopened.Meta().Schema, len(records), err)
	}
}

func durableTestAction(t *testing.T, action realtime.Action) realtime.DurableAction {
	t.Helper()
	durable, err := realtime.EncodeAction(action)
	if err != nil {
		t.Fatal(err)
	}
	return durable
}

func runningRealTimeTestLog(t *testing.T) *Log {
	t.Helper()
	log := newRealTimeTestLog(t)
	start := durableTestAction(t, realtime.Action{ID: "start", Kind: realtime.ActionStartSession, Source: realtime.SourceParticipant, Payload: realtime.StartSessionPayload{Bid: price(t, "99"), Ask: price(t, "101")}})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 1, Action: &start, Lifecycle: game.LifecycleCountdown}); err != nil {
		t.Fatal(err)
	}
	countdown := durableTestAction(t, realtime.Action{ID: "system/countdown", Kind: realtime.ActionCountdownComplete, Source: realtime.SourceSystem})
	if _, err := log.Append(Record{Schema: RealTimeSchemaVersion, Version: 2, Action: &countdown, Lifecycle: game.LifecycleRunning}); err != nil {
		t.Fatal(err)
	}
	return log
}

func independentMetadataChecksum(t *testing.T, meta Meta, domain string) string {
	t.Helper()
	meta.Checksum = ""
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(append([]byte(domain), data...))
	return fmt.Sprintf("%x", digest)
}

func independentRecordChecksum(t *testing.T, record Record, domain string) string {
	t.Helper()
	record.Checksum = ""
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	input := append([]byte(domain), record.PreviousChecksum...)
	input = append(input, record.MetadataChecksum...)
	digest := sha256.Sum256(append(input, data...))
	return fmt.Sprintf("%x", digest)
}

func encodedRecordLineBytes(t *testing.T, log *Log, record Record) int64 {
	t.Helper()
	return int64(len(encodedRecordLine(t, log, record)))
}

func encodedRecordLine(t *testing.T, log *Log, record Record) []byte {
	t.Helper()
	record.Schema = log.meta.Schema
	record.PreviousChecksum = log.lastChecksum
	if record.Schema != schema1 {
		record.MetadataChecksum = log.meta.Checksum
	}
	checksum, err := recordChecksum(record)
	if err != nil {
		t.Fatal(err)
	}
	record.Checksum = checksum
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || n != len(data) || syncErr != nil || closeErr != nil {
		t.Fatalf("append file: wrote %d/%d bytes, write error=%v sync error=%v close error=%v", n, len(data), writeErr, syncErr, closeErr)
	}
}

func replaceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
}

func addUnknownJSONField(t *testing.T, data []byte, path ...string) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	object := value
	for _, name := range path {
		nested, ok := object[name].(map[string]any)
		if !ok {
			t.Fatalf("JSON field %q is not an object", name)
		}
		object = nested
	}
	object["unknown_field"] = true
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
