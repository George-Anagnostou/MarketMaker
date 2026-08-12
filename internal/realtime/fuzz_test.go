package realtime

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"

	"market-maker/internal/fixed"
)

func FuzzDurableActionDecode(f *testing.F) {
	seeds := []string{
		`{"id":"quote","kind":"update_quote","source":"participant","payload":{"bid":"99.0000","ask":"101.0000"}}`,
		`{"id":"system/mark/1","kind":"mark_move","source":"system","payload":{"basis_points":-10}}`,
		`{"id":"pause","kind":"pause_session","source":"participant","payload":{"reason":"player"}}`,
		`{"id":"resume","kind":"resume_session","source":"participant"}`,
		`{"id":"bad","kind":"mark_move","source":"operator","payload":{}}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		var durable DurableAction
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&durable); err != nil {
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return
		}
		action, err := durable.Decode()
		if err != nil {
			return
		}
		encoded, err := EncodeAction(action)
		if err != nil {
			t.Fatalf("decoded action could not be encoded: %+v: %v", action, err)
		}
		decoded, err := encoded.Decode()
		if err != nil || !reflect.DeepEqual(decoded, action) {
			t.Fatalf("action round trip=%+v want=%+v err=%v", decoded, action, err)
		}
	})
}

func FuzzGeneratorSeedDeterminism(f *testing.F) {
	for _, seed := range []uint64{1, 42, ^uint64(0)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed uint64) {
		config := GeneratorConfig{
			Version:            1,
			Seed:               seed,
			Duration:           100 * time.Millisecond,
			CustomerCadence:    Interval{Minimum: 5 * time.Millisecond, Maximum: 12 * time.Millisecond},
			MarkCadence:        Interval{Minimum: 15 * time.Millisecond, Maximum: 25 * time.Millisecond},
			CarryCadence:       20 * time.Millisecond,
			MaxOrderQuantity:   fixed.Qty(100_000),
			MaxFlowSlippageBps: 200,
			MinMoveBps:         -25,
			MaxMoveBps:         25,
			CustomerDomain:     "customer/v1",
			MarkDomain:         "mark/v1",
			InformedDomain:     "informed/v1",
		}
		first, err := NewGenerator(config)
		if err != nil {
			if seed == 0 {
				return
			}
			t.Fatal(err)
		}
		second, err := NewGenerator(config)
		if err != nil {
			t.Fatal(err)
		}
		for count := 0; count < 128; count++ {
			left, leftOK, err := first.Next()
			if err != nil {
				t.Fatal(err)
			}
			right, rightOK, err := second.Next()
			if err != nil {
				t.Fatal(err)
			}
			if leftOK != rightOK || leftOK && !reflect.DeepEqual(left, right) {
				t.Fatalf("seed %d diverged: left=%+v/%v right=%+v/%v", seed, left, leftOK, right, rightOK)
			}
			if !leftOK {
				return
			}
			if left.Due < 0 || left.Action.ID == "" || left.Action.Source != SourceSystem {
				t.Fatalf("invalid generated action: %+v", left)
			}
			if err := first.Commit(left); err != nil {
				t.Fatal(err)
			}
			if err := second.Commit(right); err != nil {
				t.Fatal(err)
			}
		}
		t.Fatal("generator did not terminate within 128 actions")
	})
}

func FuzzCheckpointValidation(f *testing.F) {
	for _, status := range []string{string(StatusPreparing), string(StatusPaused), string(StatusCompleted), "invalid"} {
		f.Add(status, int64(0), uint64(0), uint64(0))
	}
	f.Fuzz(func(t *testing.T, status string, elapsed int64, next, sequence uint64) {
		checkpoint := Checkpoint{Status: Status(status), Elapsed: time.Duration(elapsed), NextScheduled: next, Sequence: sequence}
		valid := checkpoint.Elapsed >= 0 && checkpoint.NextScheduled <= checkpoint.Sequence
		switch checkpoint.Status {
		case StatusPreparing:
			valid = valid && checkpoint.Elapsed == 0 && checkpoint.NextScheduled == 0 && checkpoint.Sequence == 0
		case StatusPaused:
		case StatusCompleted:
			valid = valid && checkpoint.Sequence > 0
		default:
			valid = false
		}
		if err := validateCheckpoint(checkpoint); (err == nil) != valid {
			t.Fatalf("checkpoint=%+v error=%v valid=%v", checkpoint, err, valid)
		}
	})
}
