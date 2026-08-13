package realtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"market-maker/internal/fixed"
)

const (
	ActionStartSession      = "start_session"
	ActionCountdownComplete = "countdown_complete"
	ActionPauseSession      = "pause_session"
	ActionResumeSession     = "resume_session"
	ActionQuitSession       = "quit"
)

type StartSessionPayload struct {
	Bid fixed.Price `json:"bid"`
	Ask fixed.Price `json:"ask"`
}

type PauseReason string

const (
	PauseReasonPlayer   PauseReason = "player"
	PauseReasonShutdown PauseReason = "shutdown"
	PauseReasonRecovery PauseReason = "recovery"
)

func (r PauseReason) Validate() error {
	switch r {
	case PauseReasonPlayer, PauseReasonShutdown, PauseReasonRecovery:
		return nil
	default:
		return errors.New("invalid pause reason")
	}
}

type PauseSessionPayload struct {
	Reason PauseReason `json:"reason"`
}

type DurableAction struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Source  Source          `json:"source"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func EncodeAction(action Action) (DurableAction, error) {
	if action.Source != SourceSystem && action.Source != SourceParticipant {
		return DurableAction{}, errors.New("invalid action source")
	}
	if err := validateAction(action, action.Source); err != nil {
		return DurableAction{}, err
	}
	if err := validatePayloadType(action); err != nil {
		return DurableAction{}, err
	}
	durable := DurableAction{ID: action.ID, Kind: action.Kind, Source: action.Source}
	if action.Payload != nil {
		payload, err := json.Marshal(action.Payload)
		if err != nil {
			return DurableAction{}, err
		}
		durable.Payload = payload
	}
	return durable, nil
}

func (a DurableAction) Decode() (Action, error) {
	if a.Source != SourceSystem && a.Source != SourceParticipant {
		return Action{}, errors.New("invalid action source")
	}
	action := Action{ID: a.ID, Kind: a.Kind, Source: a.Source}
	if err := validateAction(action, a.Source); err != nil {
		return Action{}, err
	}
	switch a.Kind {
	case ActionStartSession:
		if a.Source != SourceParticipant {
			return Action{}, errors.New("start action must come from a participant")
		}
		var payload StartSessionPayload
		if err := decodeActionPayload(a.Payload, &payload, "bid", "ask"); err != nil {
			return Action{}, err
		}
		action.Payload = payload
	case ActionCountdownComplete:
		if a.Source != SourceSystem || len(a.Payload) != 0 {
			return Action{}, errors.New("countdown action is invalid")
		}
	case ActionPauseSession:
		var payload PauseSessionPayload
		if err := decodeActionPayload(a.Payload, &payload, "reason"); err != nil {
			return Action{}, err
		}
		if err := validateDurablePauseSourceReason(a.Source, payload.Reason); err != nil {
			return Action{}, err
		}
		action.Payload = payload
	case ActionResumeSession:
		if a.Source != SourceParticipant || len(a.Payload) != 0 {
			return Action{}, errors.New("resume action is invalid")
		}
	case ActionQuitSession:
		if a.Source != SourceParticipant || len(a.Payload) != 0 {
			return Action{}, errors.New("quit action is invalid")
		}
	case ActionUpdateQuote:
		if a.Source != SourceParticipant {
			return Action{}, errors.New("quote action must come from a participant")
		}
		var payload UpdateQuotePayload
		if err := decodeActionPayload(a.Payload, &payload, "bid", "ask"); err != nil {
			return Action{}, err
		}
		action.Payload = payload
	case ActionCustomerArrival:
		if a.Source != SourceSystem {
			return Action{}, errors.New("customer action must come from the system")
		}
		var payload CustomerArrivalPayload
		if err := decodeActionPayload(a.Payload, &payload, "buy", "quantity", "slippage_bps", "informed_draw", "has_upcoming_mark"); err != nil {
			return Action{}, err
		}
		action.Payload = payload
	case ActionMarkMove:
		if a.Source != SourceSystem {
			return Action{}, errors.New("mark action must come from the system")
		}
		var payload MarkMovePayload
		if err := decodeActionPayload(a.Payload, &payload, "basis_points"); err != nil {
			return Action{}, err
		}
		action.Payload = payload
	case ActionCarryCharge, ActionTimeExpired:
		if a.Source != SourceSystem || len(a.Payload) != 0 {
			return Action{}, errors.New("payload-free action is invalid")
		}
	default:
		return Action{}, errors.New("unsupported durable action kind")
	}
	return action, nil
}

func validatePayloadType(action Action) error {
	switch action.Kind {
	case ActionStartSession:
		if action.Source != SourceParticipant {
			return errors.New("start action must come from a participant")
		}
		if _, ok := action.Payload.(StartSessionPayload); !ok {
			return errors.New("invalid start action payload")
		}
	case ActionCountdownComplete:
		if action.Source != SourceSystem || action.Payload != nil {
			return errors.New("countdown action is invalid")
		}
	case ActionPauseSession:
		payload, ok := action.Payload.(PauseSessionPayload)
		if !ok {
			return errors.New("invalid pause action payload")
		}
		if err := validateDurablePauseSourceReason(action.Source, payload.Reason); err != nil {
			return err
		}
	case ActionResumeSession:
		if action.Source != SourceParticipant || action.Payload != nil {
			return errors.New("resume action is invalid")
		}
	case ActionQuitSession:
		if action.Source != SourceParticipant || action.Payload != nil {
			return errors.New("quit action is invalid")
		}
	case ActionUpdateQuote:
		if action.Source != SourceParticipant {
			return errors.New("quote action must come from a participant")
		}
		if _, ok := action.Payload.(UpdateQuotePayload); !ok {
			return errors.New("invalid quote action payload")
		}
	case ActionCustomerArrival:
		if action.Source != SourceSystem {
			return errors.New("customer action must come from the system")
		}
		if _, ok := action.Payload.(CustomerArrivalPayload); !ok {
			return errors.New("invalid customer action payload")
		}
	case ActionMarkMove:
		if action.Source != SourceSystem {
			return errors.New("mark action must come from the system")
		}
		if _, ok := action.Payload.(MarkMovePayload); !ok {
			return errors.New("invalid mark action payload")
		}
	case ActionCarryCharge, ActionTimeExpired:
		if action.Source != SourceSystem || action.Payload != nil {
			return errors.New("payload-free action is invalid")
		}
	default:
		return errors.New("unsupported durable action kind")
	}
	return nil
}

func validateDurablePauseSourceReason(source Source, reason PauseReason) error {
	if err := reason.Validate(); err != nil {
		return err
	}
	switch source {
	case SourceParticipant:
		if reason != PauseReasonPlayer {
			return errors.New("participant pause must use player reason")
		}
	case SourceSystem:
		if reason != PauseReasonShutdown && reason != PauseReasonRecovery {
			return errors.New("system pause must use shutdown or recovery reason")
		}
	default:
		return errors.New("invalid action source")
	}
	return nil
}

func decodeActionPayload(data []byte, target any, requiredFields ...string) error {
	if len(data) == 0 {
		return errors.New("action payload is required")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("action payload cannot be null")
	}
	if err := validateUniquePayloadKeys(data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("action payload must be an object")
	}
	for _, field := range requiredFields {
		if _, ok := fields[field]; !ok {
			return errors.New("action payload is missing required field " + field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("action payload must contain one JSON value")
	}
	return nil
}

func validateUniquePayloadKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumePayloadValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("action payload must contain one JSON value")
	}
	return nil
}

func consumePayloadValue(decoder *json.Decoder) error {
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
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("action payload key must be a string")
			}
			if _, exists := keys[key]; exists {
				return errors.New("action payload contains a duplicate key")
			}
			keys[key] = struct{}{}
			if err := consumePayloadValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumePayloadValue(decoder); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid action payload delimiter")
	}
	_, err = decoder.Token()
	return err
}
