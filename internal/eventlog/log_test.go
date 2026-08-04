package eventlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
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

func TestNewLogsUseSchema3AndV3ChecksumDomains(t *testing.T) {
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
	if meta.Schema != 3 || SchemaVersion != 3 {
		t.Fatalf("metadata schema=%d current=%d", meta.Schema, SchemaVersion)
	}
	if got := independentMetadataChecksum(t, meta, "market-maker/eventlog/meta/v3\x00"); got != meta.Checksum {
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
	if record.Schema != 3 || record.MetadataChecksum != meta.Checksum {
		t.Fatalf("record is not schema 3 metadata-bound: %+v", record)
	}
	if got := independentRecordChecksum(t, record, "market-maker/eventlog/record/v3\x00"); got != record.Checksum {
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

func TestOpenRejectsUnknownSchema(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "game-1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	metaData, err := json.Marshal(Meta{Schema: 4, GameID: "game-1", OwnerID: "local", CreateCommandID: "create-1", CreatedAt: time.Now().UTC(), Config: testConfig(t)})
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
	base := Meta{Schema: SchemaVersion, GameID: "game-1", OwnerID: "", CreateCommandID: "create-1", CreatedAt: createdAt, Config: testConfig(t)}
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
	return int64(len(data) + 1)
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
