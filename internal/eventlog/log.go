// Package eventlog stores immutable, committed exchange commands as JSON Lines.
// One process owns a data directory; each record is fsynced before acknowledgement.
package eventlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"market-maker/internal/exchange"
)

const SchemaVersion = 1

type Meta struct {
	Schema    int             `json:"schema"`
	GameID    string          `json:"game_id"`
	OwnerID   string          `json:"owner_id"`
	CreatedAt time.Time       `json:"created_at"`
	Config    exchange.Config `json:"config"`
}

type Record struct {
	Schema  int              `json:"schema"`
	Version uint64           `json:"version"`
	Command exchange.Command `json:"command"`
	Result  exchange.Result  `json:"result"`
}

type Log struct {
	dir  string
	meta Meta
}

func Create(root, gameID, ownerID string, cfg exchange.Config) (*Log, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, gameID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, os.ErrExist
		}
		return nil, err
	}
	meta := Meta{Schema: SchemaVersion, GameID: gameID, OwnerID: ownerID, CreatedAt: time.Now().UTC(), Config: cfg}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, "meta.json"), append(data, '\n')); err != nil {
		return nil, err
	}
	return &Log{dir: dir, meta: meta}, nil
}

func Open(root, gameID string) (*Log, []Record, error) {
	dir := filepath.Join(root, gameID)
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, nil, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, nil, fmt.Errorf("decode game metadata: %w", err)
	}
	if meta.Schema != SchemaVersion || meta.GameID != gameID {
		return nil, nil, errors.New("unsupported or mismatched game metadata")
	}
	if err := meta.Config.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid stored config: %w", err)
	}
	records, err := loadRecords(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, nil, err
	}
	return &Log{dir: dir, meta: meta}, records, nil
}

func (l *Log) Meta() Meta { return l.meta }

func (l *Log) Append(record Record) error {
	if record.Schema != SchemaVersion || record.Command.ID == "" || record.Version == 0 {
		return errors.New("invalid event record")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(l.dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (l *Log) Path() string { return l.dir }

func loadRecords(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(data, []byte("\n"))
	records := make([]Record, 0, len(lines))
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			// A power loss may leave only the final append partial. It was never
			// fsynced as a complete record, so replay ignores that trailing data.
			if i == len(lines)-1 && !strings.HasSuffix(string(data), "\n") {
				break
			}
			return nil, fmt.Errorf("decode event record %d: %w", i+1, err)
		}
		if record.Schema != SchemaVersion || record.Version != uint64(len(records)+1) || record.Command.ID == "" {
			return nil, fmt.Errorf("invalid event record %d", i+1)
		}
		records = append(records, record)
	}
	return records, nil
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
	return os.Rename(tmp, path)
}
