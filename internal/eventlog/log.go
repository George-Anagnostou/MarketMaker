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
	"runtime"
	"sync"
	"time"

	"market-maker/internal/exchange"
	"market-maker/internal/game"
	"market-maker/internal/realtime"
	"market-maker/internal/scenario"
)

const (
	// SchemaVersion is used for newly created logs. Schemas 1 through 3 remain
	// readable and appendable so existing games retain their wire formats.
	schema1               = 1
	schema2               = 2
	schema3               = 3
	SchemaVersion         = 4
	realTimeSchema5       = 5
	RealTimeSchemaVersion = 6

	maxMetadataBytes = 1 << 20
	maxEventsBytes   = 64 << 20
)

func isRealTimeSchema(schema int) bool {
	return schema == realTimeSchema5 || schema == RealTimeSchemaVersion
}

type RealTimeMeta struct {
	LifecycleVersion uint32              `json:"lifecycle_version"`
	Lifecycle        game.LifecycleState `json:"lifecycle"`
	GeneratorVersion uint32              `json:"generator_version"`
	Seed             uint64              `json:"seed"`
}

type Meta struct {
	Schema          int                `json:"schema"`
	GameID          string             `json:"game_id"`
	OwnerID         string             `json:"owner_id"`
	CreateCommandID string             `json:"create_command_id"`
	CreatedAt       time.Time          `json:"created_at"`
	Config          exchange.Config    `json:"config"`
	Scenario        *scenario.Snapshot `json:"scenario,omitempty"`
	Mode            game.PlayMode      `json:"mode,omitempty"`
	RealTime        *RealTimeMeta      `json:"real_time,omitempty"`
	Checksum        string             `json:"checksum,omitempty"`
}

func (m Meta) EffectiveMode() game.PlayMode {
	if m.Schema <= schema3 {
		return game.PlayModeTurnBased
	}
	return m.Mode
}

type Record struct {
	Schema             int                     `json:"schema"`
	Version            uint64                  `json:"version"`
	PreviousChecksum   string                  `json:"previous_checksum,omitempty"`
	Checksum           string                  `json:"checksum"`
	MetadataChecksum   string                  `json:"metadata_checksum,omitempty"`
	Command            exchange.Command        `json:"command"`
	Action             *realtime.DurableAction `json:"action,omitempty"`
	ElapsedNanoseconds int64                   `json:"elapsed_nanoseconds,omitempty"`
	Lifecycle          game.LifecycleState     `json:"lifecycle,omitempty"`
	Result             exchange.Result         `json:"result"`
	Rejection          *ActionRejection        `json:"rejection,omitempty"`
	Coaching           *scenario.Coaching      `json:"coaching,omitempty"`
	Recap              *scenario.Recap         `json:"recap,omitempty"`
}

type ActionRejection struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	RejectionCodeCommandRejected       = "command_rejected"
	RejectionCodeQuoteRevisionConflict = "quote_revision_conflict"
)

type Log struct {
	dir                    string
	meta                   Meta
	mu                     sync.Mutex
	lastChecksum           string
	nextVersion            uint64
	commandIDs             map[string]struct{}
	lastElapsedNanoseconds int64
	lifecycle              game.LifecycleState
	eventsInfo             os.FileInfo
	eventsSize             int64
}

func Create(root, gameID, ownerID, createCommandID string, cfg exchange.Config, snapshot *scenario.Snapshot) (*Log, error) {
	return createAtWithMode(root, gameID, ownerID, createCommandID, cfg, snapshot, game.PlayModeTurnBased, 0, time.Now().UTC())
}

func CreateRealTime(root, gameID, ownerID, createCommandID string, cfg exchange.Config, snapshot *scenario.Snapshot, seed uint64) (*Log, error) {
	return createAtWithMode(root, gameID, ownerID, createCommandID, cfg, snapshot, game.PlayModeRealTime, seed, time.Now().UTC())
}

func createAt(root, gameID, ownerID, createCommandID string, cfg exchange.Config, snapshot *scenario.Snapshot, createdAt time.Time) (*Log, error) {
	return createAtWithMode(root, gameID, ownerID, createCommandID, cfg, snapshot, game.PlayModeTurnBased, 0, createdAt)
}

func createAtWithMode(root, gameID, ownerID, createCommandID string, cfg exchange.Config, snapshot *scenario.Snapshot, mode game.PlayMode, seed uint64, createdAt time.Time) (*Log, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := mkdirAllDurable(root, 0o700); err != nil {
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

	schema := SchemaVersion
	if mode == game.PlayModeRealTime {
		schema = RealTimeSchemaVersion
	}
	meta := Meta{Schema: schema, GameID: gameID, OwnerID: ownerID, CreateCommandID: createCommandID, CreatedAt: createdAt, Config: cfg, Scenario: cloneSnapshot(snapshot), Mode: mode}
	if mode == game.PlayModeRealTime {
		meta.RealTime = &RealTimeMeta{LifecycleVersion: game.LifecycleVersion, Lifecycle: game.LifecyclePreparing, GeneratorVersion: game.GeneratorVersion, Seed: seed}
	}
	if err := validateMeta(meta); err != nil {
		return nil, err
	}
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
		return nil, closeFile(f, err)
	}
	eventsInfo, err := f.Stat()
	if err != nil {
		return nil, closeFile(f, err)
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
	publishedInfo, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	if !os.SameFile(eventsInfo, publishedInfo) || publishedInfo.Size() != 0 {
		return nil, errors.New("published event log identity or size changed")
	}
	return &Log{dir: dir, meta: meta, nextVersion: 1, commandIDs: map[string]struct{}{createCommandID: {}}, lifecycle: initialLifecycle(meta), eventsInfo: publishedInfo}, nil
}

func Open(root, gameID string) (*Log, []Record, error) {
	meta, err := ReadMeta(root, gameID)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(root, gameID)
	eventsPath := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(eventsPath, os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	records, eventsInfo, eventsSize, err := loadRecords(f, meta.Schema, meta.Checksum, meta.CreateCommandID, initialLifecycle(meta))
	if err != nil {
		return nil, nil, closeFile(f, err)
	}
	pathInfo, err := os.Stat(eventsPath)
	if err != nil {
		return nil, nil, closeFile(f, err)
	}
	if !os.SameFile(eventsInfo, pathInfo) || pathInfo.Size() != eventsSize {
		return nil, nil, closeFile(f, errors.New("event log identity or size changed while opening"))
	}
	if err := f.Close(); err != nil {
		return nil, nil, fmt.Errorf("close event log: %w", err)
	}
	log := &Log{dir: dir, meta: meta, nextVersion: uint64(len(records) + 1), commandIDs: make(map[string]struct{}, len(records)+1), lifecycle: initialLifecycle(meta), eventsInfo: eventsInfo, eventsSize: eventsSize}
	log.commandIDs[meta.CreateCommandID] = struct{}{}
	if len(records) > 0 {
		log.lastChecksum = records[len(records)-1].Checksum
		if isRealTimeSchema(meta.Schema) {
			log.lastElapsedNanoseconds = records[len(records)-1].ElapsedNanoseconds
			log.lifecycle = records[len(records)-1].Lifecycle
		}
	}
	for _, record := range records {
		log.commandIDs[recordID(record)] = struct{}{}
	}
	return log, records, nil
}

// ReadMeta validates a published game's metadata without opening, reading, or
// mutating its event stream.
func ReadMeta(root, gameID string) (Meta, error) {
	dir := filepath.Join(root, gameID)
	data, err := readFileBounded(filepath.Join(dir, "meta.json"), "game metadata", maxMetadataBytes)
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := decodeStrictJSON(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("decode game metadata: %w", err)
	}
	if !supportedSchema(meta.Schema) || meta.GameID != gameID {
		return Meta{}, errors.New("unsupported or mismatched game metadata")
	}
	switch meta.Schema {
	case schema1:
	case schema2, schema3, SchemaVersion, realTimeSchema5, RealTimeSchemaVersion:
		checksum, err := metadataChecksum(meta)
		if err != nil || meta.Checksum == "" || meta.Checksum != checksum {
			return Meta{}, errors.New("invalid game metadata checksum")
		}
	}
	if err := meta.Config.Validate(); err != nil {
		return Meta{}, fmt.Errorf("invalid stored config: %w", err)
	}
	if err := validateMeta(meta); err != nil {
		return Meta{}, fmt.Errorf("invalid stored metadata: %w", err)
	}
	meta.Scenario = cloneSnapshot(meta.Scenario)
	meta.RealTime = cloneRealTimeMeta(meta.RealTime)
	return meta, nil
}

func (l *Log) Meta() Meta {
	meta := l.meta
	meta.Scenario = cloneSnapshot(meta.Scenario)
	meta.RealTime = cloneRealTimeMeta(meta.RealTime)
	return meta
}

func cloneSnapshot(snapshot *scenario.Snapshot) *scenario.Snapshot {
	if snapshot == nil {
		return nil
	}
	copy := *snapshot
	copy.Tutorial = append([]scenario.TutorialStep(nil), snapshot.Tutorial...)
	copy.Modes = append([]game.PlayMode(nil), snapshot.Modes...)
	if snapshot.RealTime != nil {
		realTime := *snapshot.RealTime
		copy.RealTime = &realTime
	}
	return &copy
}

func cloneRealTimeMeta(meta *RealTimeMeta) *RealTimeMeta {
	if meta == nil {
		return nil
	}
	copy := *meta
	return &copy
}

func validateMeta(meta Meta) error {
	if meta.Schema <= schema3 {
		if meta.Mode != "" || meta.RealTime != nil || meta.Scenario != nil && (len(meta.Scenario.Modes) != 0 || meta.Scenario.RealTime != nil) {
			return errors.New("historical schema contains real-time metadata")
		}
		return nil
	}
	if meta.Schema != SchemaVersion && !isRealTimeSchema(meta.Schema) {
		return errors.New("unsupported game metadata schema")
	}
	if err := meta.Mode.Validate(); err != nil {
		return err
	}
	switch meta.Mode {
	case game.PlayModeTurnBased:
		if meta.Schema != SchemaVersion || meta.RealTime != nil {
			return errors.New("turn-based game contains real-time lifecycle")
		}
	case game.PlayModeRealTime:
		if !isRealTimeSchema(meta.Schema) || meta.RealTime == nil || meta.Scenario == nil || meta.Scenario.RealTime == nil {
			return errors.New("real-time game is missing scenario or lifecycle metadata")
		}
		if !snapshotSupportsMode(meta.Scenario, game.PlayModeRealTime) {
			return errors.New("scenario does not support real-time mode")
		}
		if meta.RealTime.LifecycleVersion != game.LifecycleVersion || meta.RealTime.GeneratorVersion != game.GeneratorVersion || meta.Scenario.RealTime.GeneratorVersion != meta.RealTime.GeneratorVersion || meta.RealTime.Seed == 0 {
			return errors.New("unsupported real-time lifecycle, generator version, or seed")
		}
		if meta.RealTime.Lifecycle != game.LifecyclePreparing {
			return errors.New("real-time game must initially be preparing")
		}
		if err := scenario.ValidateRealTimeConfig(meta.Config, meta.Scenario.RealTime); err != nil {
			return fmt.Errorf("invalid real-time scenario config: %w", err)
		}
	}
	return nil
}

func snapshotSupportsMode(snapshot *scenario.Snapshot, mode game.PlayMode) bool {
	for _, supported := range snapshot.Modes {
		if supported == mode {
			return true
		}
	}
	return false
}

func (l *Log) Append(record Record) (Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	id, err := validateRecordInput(record, l.meta, l.nextVersion, l.lastElapsedNanoseconds, l.lifecycle)
	if err != nil {
		return Record{}, errors.New("invalid event record")
	}
	if _, exists := l.commandIDs[id]; exists {
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
	case schema2, schema3, SchemaVersion, realTimeSchema5, RealTimeSchemaVersion:
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
	operationErr := l.verifyActiveFile(f, l.eventsSize, false)
	if operationErr == nil && (lineBytes > maxEventsBytes || l.eventsSize > maxEventsBytes-lineBytes) {
		operationErr = fmt.Errorf("event log exceeds %d-byte limit", maxEventsBytes)
	}
	if operationErr == nil {
		operationErr = writeChecked(f, append(data, '\n'))
	}
	if operationErr == nil {
		operationErr = f.Sync()
	}
	newSize := l.eventsSize + lineBytes
	if operationErr == nil {
		operationErr = l.verifyActiveFile(f, newSize, false)
	}
	closeErr := f.Close()
	if operationErr != nil || closeErr != nil {
		return Record{}, errors.Join(operationErr, wrapCloseError("event log", closeErr))
	}
	l.lastChecksum, l.nextVersion = record.Checksum, record.Version+1
	l.commandIDs[id] = struct{}{}
	if isRealTimeSchema(record.Schema) {
		l.lastElapsedNanoseconds = record.ElapsedNanoseconds
		l.lifecycle = record.Lifecycle
	}
	l.eventsSize = newSize
	return record, nil
}

func validateRecordInput(record Record, meta Meta, version uint64, lastElapsedNanoseconds int64, lifecycle game.LifecycleState) (string, error) {
	validSchema := record.Schema == meta.Schema || meta.EffectiveMode() == game.PlayModeTurnBased && record.Schema == SchemaVersion
	if record.Version != version || !validSchema {
		return "", errors.New("invalid record version or schema")
	}
	if meta.EffectiveMode() == game.PlayModeRealTime {
		if record.Action == nil || record.Command != (exchange.Command{}) || record.ElapsedNanoseconds < lastElapsedNanoseconds {
			return "", errors.New("invalid real-time record")
		}
		action, err := record.Action.Decode()
		if err != nil {
			return "", err
		}
		if err := validateRealTimeSchemaAction(meta.Schema, action); err != nil {
			return "", err
		}
		if record.Rejection != nil {
			if action.Source != realtime.SourceParticipant || !validRejectionCode(meta.Schema, action, record.Rejection.Code) || record.Rejection.Message == "" {
				return "", errors.New("invalid durable rejection")
			}
		}
		if err := validateLifecycleTransition(lifecycle, action, record); err != nil {
			return "", err
		}
		return action.ID, nil
	}
	if record.Command.ID == "" || record.Action != nil || record.ElapsedNanoseconds != 0 || record.Lifecycle != "" || record.Rejection != nil {
		return "", errors.New("invalid turn-based record")
	}
	return record.Command.ID, nil
}

func validateRealTimeSchemaAction(schema int, action realtime.Action) error {
	if schema == realTimeSchema5 {
		if action.Kind == realtime.ActionCountdownComplete && action.Payload != nil {
			return errors.New("countdown payload is not supported by real-time schema 5")
		}
		if action.Kind == realtime.ActionQuitSession {
			return errors.New("quit is not supported by real-time schema 5")
		}
		if action.Kind != realtime.ActionUpdateQuote {
			return nil
		}
		payload, ok := action.Payload.(realtime.UpdateQuotePayload)
		if !ok || payload.ExpectedRevision != nil {
			return errors.New("quote revision is not supported by real-time schema 5")
		}
		return nil
	}
	if schema == RealTimeSchemaVersion && action.Kind == realtime.ActionCountdownComplete {
		if _, ok := action.Payload.(realtime.CountdownCompletePayload); !ok {
			return errors.New("countdown quote is required by real-time schema 6")
		}
	}
	if schema == RealTimeSchemaVersion && action.Kind == realtime.ActionUpdateQuote {
		payload, ok := action.Payload.(realtime.UpdateQuotePayload)
		if !ok || payload.ExpectedRevision == nil {
			return errors.New("quote revision is required by real-time schema 6")
		}
	}
	return nil
}

func validRejectionCode(schema int, action realtime.Action, code string) bool {
	if code == RejectionCodeCommandRejected {
		return true
	}
	return schema == RealTimeSchemaVersion && action.Kind == realtime.ActionUpdateQuote && code == RejectionCodeQuoteRevisionConflict
}

func initialLifecycle(meta Meta) game.LifecycleState {
	if isRealTimeSchema(meta.Schema) && meta.RealTime != nil {
		return meta.RealTime.Lifecycle
	}
	return ""
}

func validateLifecycleTransition(previous game.LifecycleState, action realtime.Action, record Record) error {
	if previous == game.LifecycleCompleted {
		return errors.New("event recorded after lifecycle completed")
	}
	if (action.Kind == realtime.ActionStartSession || action.Kind == realtime.ActionCountdownComplete || action.Kind == realtime.ActionPauseSession && previous == game.LifecycleCountdown) && record.ElapsedNanoseconds != 0 {
		return errors.New("countdown lifecycle cannot advance market time")
	}
	want := previous
	switch action.Kind {
	case realtime.ActionStartSession:
		if previous != game.LifecyclePreparing || action.Source != realtime.SourceParticipant {
			return errors.New("invalid start lifecycle transition")
		}
		if record.Rejection == nil {
			want = game.LifecycleCountdown
		}
	case realtime.ActionCountdownComplete:
		if previous != game.LifecycleCountdown || action.Source != realtime.SourceSystem || record.Rejection != nil {
			return errors.New("invalid countdown lifecycle transition")
		}
		want = game.LifecycleRunning
	case realtime.ActionPauseSession:
		payload, ok := action.Payload.(realtime.PauseSessionPayload)
		if !ok || previous != game.LifecycleRunning && previous != game.LifecycleCountdown || record.Rejection != nil {
			return errors.New("invalid pause lifecycle transition")
		}
		if action.Source == realtime.SourceParticipant && payload.Reason != realtime.PauseReasonPlayer || action.Source == realtime.SourceSystem && payload.Reason != realtime.PauseReasonShutdown && payload.Reason != realtime.PauseReasonRecovery {
			return errors.New("pause source does not match reason")
		}
		want = game.LifecyclePaused
	case realtime.ActionResumeSession:
		if previous != game.LifecyclePaused || action.Source != realtime.SourceParticipant || record.Rejection != nil {
			return errors.New("invalid resume lifecycle transition")
		}
		want = game.LifecycleRunning
	case realtime.ActionQuitSession:
		if action.Source != realtime.SourceParticipant || action.Payload != nil || record.Rejection == nil && !record.Result.State.IsOver {
			return errors.New("invalid quit lifecycle transition")
		}
		if record.Rejection == nil {
			want = game.LifecycleCompleted
		}
	case realtime.ActionUpdateQuote:
		if previous != game.LifecycleRunning || action.Source != realtime.SourceParticipant {
			return errors.New("quote outside running lifecycle")
		}
		if record.Rejection == nil && record.Result.State.IsOver {
			want = game.LifecycleCompleted
		}
	case realtime.ActionCustomerArrival, realtime.ActionMarkMove, realtime.ActionCarryCharge, realtime.ActionTimeExpired:
		if previous != game.LifecycleRunning || action.Source != realtime.SourceSystem || record.Rejection != nil {
			return errors.New("system event outside running lifecycle")
		}
		if record.Result.State.IsOver {
			want = game.LifecycleCompleted
		}
		if action.Kind == realtime.ActionTimeExpired && want != game.LifecycleCompleted {
			return errors.New("time expiry must complete lifecycle")
		}
	default:
		return errors.New("unsupported lifecycle action")
	}
	if err := record.Lifecycle.Validate(); err != nil || record.Lifecycle != want {
		return errors.New("record has invalid resulting lifecycle")
	}
	return nil
}

func recordID(record Record) string {
	if record.Action != nil {
		return record.Action.ID
	}
	return record.Command.ID
}

func (l *Log) Path() string { return l.dir }

// Sync establishes a fresh durability barrier for all files and directory
// entries needed to discover and replay this game after a crash.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(filepath.Join(l.dir, "events.jsonl"))
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	operationErr := l.verifyActiveFile(f, l.eventsSize, false)
	if operationErr == nil {
		operationErr = f.Sync()
	}
	if operationErr == nil {
		operationErr = l.verifyActiveFile(f, l.eventsSize, false)
	}
	closeErr := f.Close()
	if operationErr != nil || closeErr != nil {
		return fmt.Errorf("sync events file: %w", errors.Join(operationErr, wrapCloseError("event log", closeErr)))
	}
	if err := syncDir(l.dir); err != nil {
		return fmt.Errorf("sync game directory: %w", err)
	}
	if err := syncDir(filepath.Dir(l.dir)); err != nil {
		return fmt.Errorf("sync event log root: %w", err)
	}
	return nil
}

// Recover validates the active log through its original file identity. It
// retains complete valid records from an uncertain append and removes any
// malformed or partial suffix without accepting rollback or replacement.
// The returned slice contains every retained record, not only recovered ones.
func (l *Log) Recover() ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	path := filepath.Join(l.dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operationErr := l.verifyActiveFile(f, l.eventsSize, true)
	var records []Record
	var finalInfo os.FileInfo
	var finalSize int64
	if operationErr == nil {
		info, statErr := f.Stat()
		if statErr != nil {
			operationErr = statErr
		} else {
			data, readErr := readFileAtSize(f, info.Size(), "event log", maxEventsBytes)
			if readErr != nil {
				operationErr = readErr
			} else {
				records, finalSize, operationErr = l.recoverData(f, data)
			}
		}
	}
	if operationErr == nil {
		operationErr = f.Sync()
	}
	if operationErr == nil {
		operationErr = l.verifyActiveFile(f, finalSize, false)
	}
	if operationErr == nil {
		finalInfo, operationErr = f.Stat()
	}
	closeErr := f.Close()
	if operationErr != nil || closeErr != nil {
		return nil, errors.Join(operationErr, wrapCloseError("event log", closeErr))
	}

	l.lastChecksum = ""
	l.nextVersion = uint64(len(records) + 1)
	l.commandIDs = make(map[string]struct{}, len(records)+1)
	l.commandIDs[l.meta.CreateCommandID] = struct{}{}
	l.lastElapsedNanoseconds = 0
	l.lifecycle = initialLifecycle(l.meta)
	if len(records) > 0 {
		l.lastChecksum = records[len(records)-1].Checksum
		if isRealTimeSchema(l.meta.Schema) {
			l.lastElapsedNanoseconds = records[len(records)-1].ElapsedNanoseconds
			l.lifecycle = records[len(records)-1].Lifecycle
		}
	}
	for _, record := range records {
		l.commandIDs[recordID(record)] = struct{}{}
	}
	l.eventsInfo = finalInfo
	l.eventsSize = finalSize
	return records, nil
}

func loadRecords(f *os.File, schema int, expectedMetadataChecksum, createCommandID string, lifecycle game.LifecycleState) ([]Record, os.FileInfo, int64, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	data, err := readFileAtSize(f, info.Size(), "event log", maxEventsBytes)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		size := int64(bytes.LastIndexByte(data, '\n') + 1)
		if err := f.Truncate(size); err != nil {
			return nil, nil, 0, err
		}
		if err := f.Sync(); err != nil {
			return nil, nil, 0, err
		}
		data = data[:size]
	}
	records, err := parseRecords(data, schema, expectedMetadataChecksum, createCommandID, lifecycle)
	if err != nil {
		return nil, nil, 0, err
	}
	finalInfo, err := f.Stat()
	if err != nil {
		return nil, nil, 0, err
	}
	if !os.SameFile(info, finalInfo) || finalInfo.Size() != int64(len(data)) {
		return nil, nil, 0, errors.New("event log identity or size changed while reading")
	}
	return records, finalInfo, finalInfo.Size(), nil
}

func parseRecords(data []byte, schema int, expectedMetadataChecksum, createCommandID string, lifecycle game.LifecycleState) ([]Record, error) {
	lines := bytes.Split(data, []byte("\n"))
	records := make([]Record, 0, len(lines))
	previousChecksum := ""
	lastElapsedNanoseconds := int64(0)
	commandIDs := make(map[string]struct{})
	if isRealTimeSchema(schema) {
		commandIDs[createCommandID] = struct{}{}
	}
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var record Record
		if err := decodeStrictJSON(line, &record); err != nil {
			return nil, fmt.Errorf("decode event record %d: %w", i+1, err)
		}
		if err := validateRecord(record, schema, expectedMetadataChecksum, uint64(len(records)+1), previousChecksum, commandIDs, lastElapsedNanoseconds, lifecycle); err != nil {
			return nil, fmt.Errorf("event record %d: %w", i+1, err)
		}
		records = append(records, record)
		previousChecksum = record.Checksum
		if isRealTimeSchema(schema) {
			lastElapsedNanoseconds = record.ElapsedNanoseconds
			lifecycle = record.Lifecycle
		}
		commandIDs[recordID(record)] = struct{}{}
	}
	return records, nil
}

func (l *Log) recoverData(f *os.File, data []byte) ([]Record, int64, error) {
	if int64(len(data)) < l.eventsSize {
		return nil, 0, errors.New("event log was truncated below acknowledged size")
	}
	prefix := data[:int(l.eventsSize)]
	if len(prefix) > 0 && prefix[len(prefix)-1] != '\n' {
		return nil, 0, errors.New("acknowledged event log boundary is not newline-terminated")
	}
	records, err := parseRecords(prefix, l.meta.Schema, l.meta.Checksum, l.meta.CreateCommandID, initialLifecycle(l.meta))
	if err != nil {
		return nil, 0, fmt.Errorf("validate acknowledged event log prefix: %w", err)
	}
	if !l.matchesAcknowledgedRecords(records) {
		return nil, 0, errors.New("acknowledged event log prefix does not match active state")
	}

	commandIDs := make(map[string]struct{}, len(l.commandIDs))
	for id := range l.commandIDs {
		commandIDs[id] = struct{}{}
	}
	previousChecksum := l.lastChecksum
	nextVersion := l.nextVersion
	lastElapsedNanoseconds := l.lastElapsedNanoseconds
	lifecycle := l.lifecycle
	validSize := l.eventsSize
	suffix := data[int(l.eventsSize):]
	for offset := 0; offset < len(suffix); {
		newline := bytes.IndexByte(suffix[offset:], '\n')
		if newline < 0 {
			break
		}
		lineEnd := offset + newline
		line := bytes.TrimSpace(suffix[offset:lineEnd])
		if len(line) == 0 {
			break
		}
		var record Record
		if err := decodeStrictJSON(line, &record); err != nil {
			break
		}
		if err := validateRecord(record, l.meta.Schema, l.meta.Checksum, nextVersion, previousChecksum, commandIDs, lastElapsedNanoseconds, lifecycle); err != nil {
			break
		}
		records = append(records, record)
		commandIDs[recordID(record)] = struct{}{}
		previousChecksum = record.Checksum
		if isRealTimeSchema(l.meta.Schema) {
			lastElapsedNanoseconds = record.ElapsedNanoseconds
			lifecycle = record.Lifecycle
		}
		nextVersion++
		offset = lineEnd + 1
		validSize = l.eventsSize + int64(offset)
	}
	if validSize != int64(len(data)) {
		if err := f.Truncate(validSize); err != nil {
			return nil, 0, fmt.Errorf("truncate uncertain event log suffix: %w", err)
		}
	}
	return records, validSize, nil
}

func (l *Log) matchesAcknowledgedRecords(records []Record) bool {
	if l.nextVersion != uint64(len(records)+1) {
		return false
	}
	lastChecksum := ""
	if len(records) > 0 {
		lastChecksum = records[len(records)-1].Checksum
	}
	if l.lastChecksum != lastChecksum {
		return false
	}
	lastElapsedNanoseconds := int64(0)
	lifecycle := initialLifecycle(l.meta)
	if len(records) > 0 && isRealTimeSchema(l.meta.Schema) {
		lastElapsedNanoseconds = records[len(records)-1].ElapsedNanoseconds
		lifecycle = records[len(records)-1].Lifecycle
	}
	if l.lastElapsedNanoseconds != lastElapsedNanoseconds || l.lifecycle != lifecycle {
		return false
	}
	commandIDs := make(map[string]struct{}, len(records)+1)
	commandIDs[l.meta.CreateCommandID] = struct{}{}
	for _, record := range records {
		commandIDs[recordID(record)] = struct{}{}
	}
	if len(commandIDs) != len(l.commandIDs) {
		return false
	}
	for id := range commandIDs {
		if _, ok := l.commandIDs[id]; !ok {
			return false
		}
	}
	return true
}

func validateRecord(record Record, schema int, expectedMetadataChecksum string, version uint64, previousChecksum string, commandIDs map[string]struct{}, lastElapsedNanoseconds int64, lifecycle game.LifecycleState) error {
	id := recordID(record)
	if record.Schema != schema || record.Version != version || id == "" || record.PreviousChecksum != previousChecksum {
		return errors.New("invalid event record")
	}
	if isRealTimeSchema(schema) {
		if record.Action == nil || record.Command != (exchange.Command{}) || record.ElapsedNanoseconds < lastElapsedNanoseconds {
			return errors.New("invalid real-time event record")
		}
		action, err := record.Action.Decode()
		if err != nil || validateRealTimeSchemaAction(schema, action) != nil || record.Rejection != nil && (action.Source != realtime.SourceParticipant || !validRejectionCode(schema, action, record.Rejection.Code) || record.Rejection.Message == "") {
			return errors.New("invalid real-time action or rejection")
		}
		if err := validateLifecycleTransition(lifecycle, action, record); err != nil {
			return err
		}
	} else if record.Action != nil || record.Rejection != nil || record.ElapsedNanoseconds != 0 || record.Lifecycle != "" || record.Command.ID == "" {
		return errors.New("invalid turn-based event record")
	}
	switch schema {
	case schema1:
	case schema2, schema3, SchemaVersion, realTimeSchema5, RealTimeSchemaVersion:
		if record.MetadataChecksum != expectedMetadataChecksum {
			return errors.New("record is bound to different metadata")
		}
	default:
		return fmt.Errorf("unsupported event record schema %d", schema)
	}
	if _, exists := commandIDs[id]; exists {
		return errors.New("duplicate event command id")
	}
	checksum, err := recordChecksum(record)
	if err != nil || record.Checksum == "" || record.Checksum != checksum {
		return errors.New("invalid event record checksum")
	}
	return nil
}

func (l *Log) verifyActiveFile(f *os.File, size int64, allowExtension bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if l.eventsInfo == nil || !os.SameFile(l.eventsInfo, info) {
		return errors.New("event log file identity changed")
	}
	if (allowExtension && info.Size() < size) || (!allowExtension && info.Size() != size) {
		return fmt.Errorf("event log size changed: got %d, acknowledged %d", info.Size(), size)
	}
	pathInfo, err := os.Stat(filepath.Join(l.dir, "events.jsonl"))
	if err != nil {
		return err
	}
	if !os.SameFile(info, pathInfo) {
		return errors.New("event log path identity changed")
	}
	if pathInfo.Size() != info.Size() {
		return errors.New("event log size changed during verification")
	}
	return nil
}

func readFileAtSize(f *os.File, size int64, description string, maxBytes int64) ([]byte, error) {
	if size > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", description, maxBytes)
	}
	data := make([]byte, size)
	if size == 0 {
		return data, nil
	}
	n, err := f.ReadAt(data, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if int64(n) != size {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

func readFileBounded(path, description string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, closeFile(f, err)
	}
	if info.Size() > maxBytes {
		return nil, closeFile(f, fmt.Errorf("%s exceeds %d-byte limit", description, maxBytes))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, closeFile(f, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, closeFile(f, fmt.Errorf("%s exceeds %d-byte limit", description, maxBytes))
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close %s: %w", description, err)
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

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := writeChecked(f, data); err != nil {
		return closeFile(f, err)
	}
	if err := f.Sync(); err != nil {
		return closeFile(f, err)
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
	case schema3:
		input = append([]byte("market-maker/eventlog/record/v3\x00"), input...)
		input = append(input, record.MetadataChecksum...)
	case SchemaVersion:
		input = append([]byte("market-maker/eventlog/record/v4\x00"), input...)
		input = append(input, record.MetadataChecksum...)
	case realTimeSchema5:
		input = append([]byte("market-maker/eventlog/record/v5\x00"), input...)
		input = append(input, record.MetadataChecksum...)
	case RealTimeSchemaVersion:
		input = append([]byte("market-maker/eventlog/record/v6\x00"), input...)
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
	case schema3:
		domain = []byte("market-maker/eventlog/meta/v3\x00")
	case SchemaVersion:
		domain = []byte("market-maker/eventlog/meta/v4\x00")
	case realTimeSchema5:
		domain = []byte("market-maker/eventlog/meta/v5\x00")
	case RealTimeSchemaVersion:
		domain = []byte("market-maker/eventlog/meta/v6\x00")
	default:
		return "", errors.New("unsupported game metadata schema")
	}
	digest := sha256.Sum256(append(domain, data...))
	return fmt.Sprintf("%x", digest), nil
}

func supportedSchema(schema int) bool {
	switch schema {
	case schema1, schema2, schema3, SchemaVersion, realTimeSchema5, RealTimeSchemaVersion:
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
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, wrapCloseError("directory", closeErr))
}

func mkdirAllDurable(path string, perm os.FileMode) error {
	path = filepath.Clean(path)
	missing := make([]string, 0)
	for current := path; ; current = filepath.Dir(current) {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return os.ErrNotExist
		}
	}
	if err := os.MkdirAll(path, perm); err != nil {
		return err
	}
	// Sync deepest parents first so every new child entry is durable before
	// the entry that makes its parent reachable.
	for _, created := range missing {
		if err := syncDirBestEffort(filepath.Dir(created)); err != nil {
			return err
		}
	}
	return nil
}

func syncDirBestEffort(path string) error {
	err := syncDir(path)
	if err != nil && (runtime.GOOS == "windows" || errors.Is(err, os.ErrInvalid)) {
		return nil
	}
	return err
}

func writeChecked(f *os.File, data []byte) error {
	n, err := f.Write(data)
	if n != len(data) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return err
}

func closeFile(f *os.File, operationErr error) error {
	return errors.Join(operationErr, wrapCloseError(f.Name(), f.Close()))
}

func wrapCloseError(description string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close %s: %w", description, err)
}
