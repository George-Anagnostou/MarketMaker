// Package eventlog stores immutable, committed exchange commands as JSON Lines.
// One process owns a data directory; each record is fsynced before acknowledgement.
package eventlog

import (
	"bytes"
	"crypto/sha256"
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
	Schema          int             `json:"schema"`
	GameID          string          `json:"game_id"`
	OwnerID         string          `json:"owner_id"`
	CreateCommandID string          `json:"create_command_id"`
	CreatedAt       time.Time       `json:"created_at"`
	Config          exchange.Config `json:"config"`
}

type Record struct {
	Schema           int              `json:"schema"`
	Version          uint64           `json:"version"`
	PreviousChecksum string           `json:"previous_checksum,omitempty"`
	Checksum         string           `json:"checksum"`
	Command          exchange.Command `json:"command"`
	Result           exchange.Result  `json:"result"`
}

type Log struct {
	dir          string
	meta         Meta
	lastChecksum string
	nextVersion  uint64
}

func Create(root, gameID, ownerID, createCommandID string, cfg exchange.Config) (*Log, error) {
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
	if err := syncDir(root); err != nil {
		return nil, err
	}
	if createCommandID == "" {
		return nil, errors.New("create command id is required")
	}
	meta := Meta{Schema: SchemaVersion, GameID: gameID, OwnerID: ownerID, CreateCommandID: createCommandID, CreatedAt: time.Now().UTC(), Config: cfg}
	data, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, "meta.json"), append(data, '\n')); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
	if err := syncDir(dir); err != nil {
		return nil, err
	}
	return &Log{dir: dir, meta: meta, nextVersion: 1}, nil
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
	log := &Log{dir: dir, meta: meta, nextVersion: uint64(len(records) + 1)}
	if len(records) > 0 {
		log.lastChecksum = records[len(records)-1].Checksum
	}
	return log, records, nil
}

func (l *Log) Meta() Meta { return l.meta }

func (l *Log) Append(record Record) error {
	if record.Schema != SchemaVersion || record.Command.ID == "" || record.Version != l.nextVersion {
		return errors.New("invalid event record")
	}
	record.PreviousChecksum = l.lastChecksum
	checksum, err := recordChecksum(record)
	if err != nil {
		return err
	}
	record.Checksum = checksum
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
	if err := f.Sync(); err != nil {
		return err
	}
	l.lastChecksum, l.nextVersion = record.Checksum, record.Version+1
	return nil
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
	previousChecksum := ""
	for i, line := range lines {
		rawLine := line
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			// A power loss may leave only the final append partial. It was never
			// fsynced as a complete record, so replay ignores that trailing data.
			if i == len(lines)-1 && !strings.HasSuffix(string(data), "\n") {
				if err := truncateAndSync(path, int64(len(data)-len(rawLine))); err != nil {
					return nil, err
				}
				break
			}
			return nil, fmt.Errorf("decode event record %d: %w", i+1, err)
		}
		if record.Schema != SchemaVersion || record.Version != uint64(len(records)+1) || record.Command.ID == "" || record.PreviousChecksum != previousChecksum {
			return nil, fmt.Errorf("invalid event record %d", i+1)
		}
		checksum, err := recordChecksum(record)
		if err != nil || record.Checksum == "" || record.Checksum != checksum {
			return nil, fmt.Errorf("invalid event record checksum %d", i+1)
		}
		records = append(records, record)
		previousChecksum = record.Checksum
	}
	return records, nil
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
	digest := sha256.Sum256(append([]byte(record.PreviousChecksum), data...))
	return fmt.Sprintf("%x", digest), nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
