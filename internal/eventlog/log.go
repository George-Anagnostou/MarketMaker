// Package eventlog stores immutable, committed exchange commands as JSON Lines.
// One process owns a data directory; each record is fsynced before acknowledgement.
package eventlog

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"market-maker/internal/exchange"
	"market-maker/internal/scenario"
)

const (
	// SchemaVersion is used for newly created logs. Schemas 1 and 2 remain
	// readable and appendable so existing games retain their wire formats.
	schema1       = 1
	schema2       = 2
	SchemaVersion = 3

	maxMetadataBytes = 1 << 20
	maxEventsBytes   = 64 << 20
)

type Meta struct {
	Schema          int                `json:"schema"`
	GameID          string             `json:"game_id"`
	OwnerID         string             `json:"owner_id"`
	CreateCommandID string             `json:"create_command_id"`
	CreatedAt       time.Time          `json:"created_at"`
	Config          exchange.Config    `json:"config"`
	Scenario        *scenario.Snapshot `json:"scenario,omitempty"`
	Checksum        string             `json:"checksum,omitempty"`
}

type Record struct {
	Schema           int                `json:"schema"`
	Version          uint64             `json:"version"`
	PreviousChecksum string             `json:"previous_checksum,omitempty"`
	Checksum         string             `json:"checksum"`
	MetadataChecksum string             `json:"metadata_checksum,omitempty"`
	Command          exchange.Command   `json:"command"`
	Result           exchange.Result    `json:"result"`
	Coaching         *scenario.Coaching `json:"coaching,omitempty"`
	Recap            *scenario.Recap    `json:"recap,omitempty"`
}

type Log struct {
	dir          string
	meta         Meta
	lastChecksum string
	nextVersion  uint64
	commandIDs   map[string]struct{}
}

func Create(root, gameID, ownerID, createCommandID string, cfg exchange.Config, snapshot *scenario.Snapshot) (*Log, error) {
	return createAt(root, gameID, ownerID, createCommandID, cfg, snapshot, time.Now().UTC())
}

func createAt(root, gameID, ownerID, createCommandID string, cfg exchange.Config, snapshot *scenario.Snapshot, createdAt time.Time) (*Log, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if createCommandID == "" {
		return nil, errors.New("create command id is required")
	}
	dir := filepath.Join(root, gameID)
	if _, err := os.Lstat(dir); err == nil {
		return nil, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	stagingDir, err := os.MkdirTemp(root, ".eventlog-staging-")
	if err != nil {
		return nil, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stagingDir)
		}
	}()

	meta := Meta{Schema: SchemaVersion, GameID: gameID, OwnerID: ownerID, CreateCommandID: createCommandID, CreatedAt: createdAt, Config: cfg, Scenario: cloneSnapshot(snapshot)}
	checksum, err := metadataChecksum(meta)
	if err != nil {
		return nil, err
	}
	meta.Checksum = checksum
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if int64(len(data)+1) > maxMetadataBytes {
		return nil, fmt.Errorf("game metadata exceeds %d-byte limit", maxMetadataBytes)
	}
	if err := writeAtomic(filepath.Join(stagingDir, "meta.json"), append(data, '\n')); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stagingDir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := syncDir(stagingDir); err != nil {
		return nil, err
	}
	if err := os.Rename(stagingDir, dir); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, os.ErrExist
		}
		return nil, err
	}
	published = true
	if err := syncDir(root); err != nil {
		return nil, err
	}
	return &Log{dir: dir, meta: meta, nextVersion: 1, commandIDs: map[string]struct{}{createCommandID: {}}}, nil
}

func Open(root, gameID string) (*Log, []Record, error) {
	dir := filepath.Join(root, gameID)
	data, err := readFileBounded(filepath.Join(dir, "meta.json"), "game metadata", maxMetadataBytes)
	if err != nil {
		return nil, nil, err
	}
	var meta Meta
	if err := decodeStrictJSON(data, &meta); err != nil {
		return nil, nil, fmt.Errorf("decode game metadata: %w", err)
	}
	if !supportedSchema(meta.Schema) || meta.GameID != gameID {
		return nil, nil, errors.New("unsupported or mismatched game metadata")
	}
	switch meta.Schema {
	case schema1:
	case schema2, SchemaVersion:
		checksum, err := metadataChecksum(meta)
		if err != nil || meta.Checksum == "" || meta.Checksum != checksum {
			return nil, nil, errors.New("invalid game metadata checksum")
		}
	}
	if err := meta.Config.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid stored config: %w", err)
	}
	records, err := loadRecords(filepath.Join(dir, "events.jsonl"), meta.Schema, meta.Checksum)
	if err != nil {
		return nil, nil, err
	}
	log := &Log{dir: dir, meta: meta, nextVersion: uint64(len(records) + 1), commandIDs: make(map[string]struct{}, len(records)+1)}
	log.commandIDs[meta.CreateCommandID] = struct{}{}
	if len(records) > 0 {
		log.lastChecksum = records[len(records)-1].Checksum
	}
	for _, record := range records {
		log.commandIDs[record.Command.ID] = struct{}{}
	}
	return log, records, nil
}

func (l *Log) Meta() Meta {
	meta := l.meta
	meta.Scenario = cloneSnapshot(meta.Scenario)
	return meta
}

func cloneSnapshot(snapshot *scenario.Snapshot) *scenario.Snapshot {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	copy.Tutorial = append([]scenario.TutorialStep(nil), snapshot.Tutorial...)
	return &copy
}

func (l *Log) Append(record Record) (Record, error) {
	if (record.Schema != SchemaVersion && record.Schema != l.meta.Schema) || record.Command.ID == "" || record.Version != l.nextVersion {
		return Record{}, errors.New("invalid event record")
	}
	if _, exists := l.commandIDs[record.Command.ID]; exists {
		return Record{}, errors.New("duplicate event command id")
	}
	record.Schema = l.meta.Schema
	record.PreviousChecksum = l.lastChecksum
	if record.Schema == schema1 && record.Recap != nil {
		// Schema 1 checksums must remain readable by binaries that predate
		// lesson-specific recap fields, so retain its original recap wire format.
		recap := *record.Recap
		recap.AdverseSelectionTurns = 0
		recap.Scorecard = nil
		recap.InformedOrders = 0
		recap.InformedOrdersFilled = 0
		recap.InformedUnitsTraded = 0
		recap.InformedFlowPnL = 0
		record.Recap = &recap
	}
	switch record.Schema {
	case schema1:
		record.MetadataChecksum = ""
	case schema2, SchemaVersion:
		record.MetadataChecksum = l.meta.Checksum
	default:
		return Record{}, errors.New("invalid event record")
	}
	checksum, err := recordChecksum(record)
	if err != nil {
		return Record{}, err
	}
	record.Checksum = checksum
	data, err := json.Marshal(record)
	if err != nil {
		return Record{}, err
	}
	lineBytes := int64(len(data) + 1)
	f, err := os.OpenFile(filepath.Join(l.dir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Record{}, err
	}
	if lineBytes > maxEventsBytes || info.Size() > maxEventsBytes-lineBytes {
		return Record{}, fmt.Errorf("event log exceeds %d-byte limit", maxEventsBytes)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return Record{}, err
	}
	if err := f.Sync(); err != nil {
		return Record{}, err
	}
	l.lastChecksum, l.nextVersion = record.Checksum, record.Version+1
	l.commandIDs[record.Command.ID] = struct{}{}
	return record, nil
}

func (l *Log) Path() string { return l.dir }

// Sync establishes a fresh durability barrier for all files and directory
// entries needed to discover and replay this game after a crash.
func (l *Log) Sync() error {
	if err := syncFile(filepath.Join(l.dir, "events.jsonl")); err != nil {
		return fmt.Errorf("sync events file: %w", err)
	}
	if err := syncDir(l.dir); err != nil {
		return fmt.Errorf("sync game directory: %w", err)
	}
	if err := syncDir(filepath.Dir(l.dir)); err != nil {
		return fmt.Errorf("sync event log root: %w", err)
	}
	return nil
}

func loadRecords(path string, schema int, expectedMetadataChecksum string) ([]Record, error) {
	data, err := readFileBounded(path, "event log", maxEventsBytes)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte("\n"))
	records := make([]Record, 0, len(lines))
	previousChecksum := ""
	commandIDs := make(map[string]struct{})
	for i, line := range lines {
		rawLine := line
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// A complete append is newline-terminated before it is synced. Do not
		// treat an unterminated final line as committed: it may be a torn write.
		if i == len(lines)-1 && !bytes.HasSuffix(data, []byte("\n")) {
			if err := truncateAndSync(path, int64(len(data)-len(rawLine))); err != nil {
				return nil, err
			}
			break
		}
		var record Record
		if err := decodeStrictJSON(line, &record); err != nil {
			// A power loss may leave only the final append partial. It was never
			// fsynced as a complete record, so replay ignores that trailing data.
			return nil, fmt.Errorf("decode event record %d: %w", i+1, err)
		}
		if record.Schema != schema || record.Version != uint64(len(records)+1) || record.Command.ID == "" || record.PreviousChecksum != previousChecksum {
			return nil, fmt.Errorf("invalid event record %d", i+1)
		}
		switch schema {
		case schema1:
		case schema2, SchemaVersion:
			if record.MetadataChecksum != expectedMetadataChecksum {
				return nil, fmt.Errorf("event record %d is bound to different metadata", i+1)
			}
		default:
			return nil, fmt.Errorf("unsupported event record schema %d", schema)
		}
		if _, exists := commandIDs[record.Command.ID]; exists {
			return nil, fmt.Errorf("duplicate event command id at record %d", i+1)
		}
		checksum, err := recordChecksum(record)
		if err != nil || record.Checksum == "" || record.Checksum != checksum {
			return nil, fmt.Errorf("invalid event record checksum %d", i+1)
		}
		records = append(records, record)
		previousChecksum = record.Checksum
		commandIDs[record.Command.ID] = struct{}{}
	}
	return records, nil
}

func readFileBounded(path, description string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", description, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", description, maxBytes)
	}
	return data, nil
}

func decodeStrictJSON(data []byte, target any) error {
	if err := validateUniqueJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func validateUniqueJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("must contain exactly one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("JSON object key must be a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = struct{}{}
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

func truncateAndSync(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		return err
	}
	return f.Sync()
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func recordChecksum(record Record) (string, error) {
	record.Checksum = ""
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	input := []byte(record.PreviousChecksum)
	switch record.Schema {
	case schema1:
	case schema2:
		input = append([]byte("market-maker/eventlog/record/v2\x00"), input...)
		input = append(input, record.MetadataChecksum...)
	case SchemaVersion:
		input = append([]byte("market-maker/eventlog/record/v3\x00"), input...)
		input = append(input, record.MetadataChecksum...)
	default:
		return "", errors.New("unsupported event record schema")
	}
	digest := sha256.Sum256(append(input, data...))
	return fmt.Sprintf("%x", digest), nil
}

func metadataChecksum(meta Meta) (string, error) {
	meta.Checksum = ""
	data, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	var domain []byte
	switch meta.Schema {
	case schema2:
		domain = []byte("market-maker/eventlog/meta/v2\x00")
	case SchemaVersion:
		domain = []byte("market-maker/eventlog/meta/v3\x00")
	default:
		return "", errors.New("unsupported game metadata schema")
	}
	digest := sha256.Sum256(append(domain, data...))
	return fmt.Sprintf("%x", digest), nil
}

func supportedSchema(schema int) bool {
	switch schema {
	case schema1, schema2, SchemaVersion:
		return true
	default:
		return false
	}
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
