package realtime

import (
	"encoding/json"
	"reflect"
	"testing"

	"market-maker/internal/fixed"
)

func TestDurableActionRoundTrip(t *testing.T) {
	actions := []Action{
		{ID: "quote", Kind: ActionUpdateQuote, Source: SourceParticipant, Payload: UpdateQuotePayload{Bid: fixed.Price(990_000), Ask: fixed.Price(1_010_000)}},
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
	} {
		if _, err := action.Decode(); err == nil {
			t.Fatalf("malformed action accepted: %+v", action)
		}
	}
}
