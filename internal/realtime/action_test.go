package realtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"market-maker/internal/fixed"
)

func TestDurableActionRoundTrip(t *testing.T) {
	actions := []Action{
		{ID: "start", Kind: ActionStartSession, Source: SourceParticipant, Payload: StartSessionPayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000)}},
		{ID: "system/countdown", Kind: ActionCountdownComplete, Source: SourceSystem},
		{ID: "pause-player", Kind: ActionPauseSession, Source: SourceParticipant, Payload: PauseSessionPayload{Reason: PauseReasonPlayer}},
		{ID: "system/pause", Kind: ActionPauseSession, Source: SourceSystem, Payload: PauseSessionPayload{Reason: PauseReasonShutdown}},
		{ID: "resume", Kind: ActionResumeSession, Source: SourceParticipant},
		{ID: "quit", Kind: ActionQuitSession, Source: SourceParticipant},
		{ID: "quote", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000), ExpectedRevision: revisionPointer(1)}},
		{ID: "customer", Kind: ActionCustomerArrival, Source: SourceSystem, Payload: CustomerArrivalPayload{Buy: true, Quantity: fixed.Qty(10_000), SlippageBps: 5, InformedDraw: 7, HasUpcomingMark: true, UpcomingMarkMoveBps: 10}},
		{ID: "mark", Kind: ActionMarkMove, Source: SourceSystem, Payload: MarkMovePayload{BasisPoints: -10}},
		{ID: "carry", Kind: ActionCarryCharge, Source: SourceSystem},
		{ID: "expiry", Kind: ActionTimeExpired, Source: SourceSystem},
	}
	for _, action := range actions {
		t.Run(action.ID, func(t *testing.T) {
			durable, err := EncodeAction(action)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(durable)
			if err != nil {
				t.Fatal(err)
			}
			var decoded DurableAction
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			got, err := decoded.Decode()
			if err != nil || !reflect.DeepEqual(got, action) {
				t.Fatalf("got=%+v want=%+v err=%v", got, action, err)
			}
		})
	}
}

func TestDurableActionRejectsMalformedPayloads(t *testing.T) {
	for _, action := range []DurableAction{
		{ID: "unknown", Kind: "unknown", Source: SourceSystem},
		{ID: "quote", Kind: ActionUpdateQuote, Source: SourceSystem, Payload: json.RawMessage(`{"bid":"99.0000","ask":"101.0000"}`)},
		{ID: "mark", Kind: ActionMarkMove, Source: SourceSystem, Payload: json.RawMessage(`{"basis_points":1,"extra":true}`)},
		{ID: "carry", Kind: ActionCarryCharge, Source: SourceSystem, Payload: json.RawMessage(`{}`)},
		{ID: "null", Kind: ActionMarkMove, Source: SourceSystem, Payload: json.RawMessage(`null`)},
		{ID: "duplicate", Kind: ActionMarkMove, Source: SourceSystem, Payload: json.RawMessage(`{"basis_points":1,"basis_points":2}`)},
		{ID: "missing-mark", Kind: ActionMarkMove, Source: SourceSystem, Payload: json.RawMessage(`{}`)},
		{ID: "missing-quote", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: json.RawMessage(`{"bid":990000}`)},
		{ID: "system/update_quote/1", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: json.RawMessage(`{"bid":990000,"ask":1010000}`)},
		{ID: "start-system", Kind: ActionStartSession, Source: SourceSystem, Payload: json.RawMessage(`{"bid":990000,"ask":1010000}`)},
		{ID: "start-missing", Kind: ActionStartSession, Source: SourceParticipant, Payload: json.RawMessage(`{"bid":990000}`)},
		{ID: "countdown-payload", Kind: ActionCountdownComplete, Source: SourceSystem, Payload: json.RawMessage(`{}`)},
		{ID: "pause-null", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`null`)},
		{ID: "pause-missing", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`{}`)},
		{ID: "pause-unknown", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`{"reason":"other"}`)},
		{ID: "pause-extra", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`{"reason":"player","extra":true}`)},
		{ID: "pause-duplicate", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`{"reason":"player","reason":"shutdown"}`)},
		{ID: "pause-participant-shutdown", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`{"reason":"shutdown"}`)},
		{ID: "pause-participant-recovery", Kind: ActionPauseSession, Source: SourceParticipant, Payload: json.RawMessage(`{"reason":"recovery"}`)},
		{ID: "system/pause-player", Kind: ActionPauseSession, Source: SourceSystem, Payload: json.RawMessage(`{"reason":"player"}`)},
		{ID: "invalid-source", Kind: ActionPauseSession, Source: Source("operator"), Payload: json.RawMessage(`{"reason":"shutdown"}`)},
		{ID: "resume-system", Kind: ActionResumeSession, Source: SourceSystem},
		{ID: "quit-system", Kind: ActionQuitSession, Source: SourceSystem},
		{ID: "quit-payload", Kind: ActionQuitSession, Source: SourceParticipant, Payload: json.RawMessage(`{}`)},
	} {
		if _, err := action.Decode(); err == nil {
			t.Fatalf("malformed action accepted: %+v", action)
		}
	}
}

func revisionPointer(value uint64) *uint64 { return &value }

func TestEncodeActionRejectsPauseSourceReasonMismatch(t *testing.T) {
	for _, action := range []Action{
		{ID: "participant-shutdown", Kind: ActionPauseSession, Source: SourceParticipant, Payload: PauseSessionPayload{Reason: PauseReasonShutdown}},
		{ID: "participant-recovery", Kind: ActionPauseSession, Source: SourceParticipant, Payload: PauseSessionPayload{Reason: PauseReasonRecovery}},
		{ID: "system/player", Kind: ActionPauseSession, Source: SourceSystem, Payload: PauseSessionPayload{Reason: PauseReasonPlayer}},
		{ID: "unknown-source", Kind: ActionPauseSession, Source: Source("operator"), Payload: PauseSessionPayload{Reason: PauseReasonShutdown}},
	} {
		if _, err := EncodeAction(action); err == nil {
			t.Fatalf("malformed pause action accepted: %+v", action)
		}
	}
}

func TestPauseReasonValidation(t *testing.T) {
	for _, reason := range []PauseReason{PauseReasonPlayer, PauseReasonShutdown, PauseReasonRecovery} {
		if err := reason.Validate(); err != nil {
			t.Fatalf("valid pause reason %q rejected: %v", reason, err)
		}
	}
	for _, reason := range []PauseReason{"", "other"} {
		if err := reason.Validate(); err == nil {
			t.Fatalf("invalid pause reason %q accepted", reason)
		}
	}
}
