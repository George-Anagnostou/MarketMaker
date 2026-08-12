package eventlog

import (
	"encoding/json"
	"reflect"
	"testing"
)

type strictFuzzDocument struct {
	Name  string `json:"name,omitempty"`
	Count int    `json:"count,omitempty"`
	Child *struct {
		Enabled bool `json:"enabled,omitempty"`
	} `json:"child,omitempty"`
}

func FuzzDecodeStrictJSON(f *testing.F) {
	valid := []byte(`{"name":"value","count":1,"child":{"enabled":true}}`)
	f.Add(valid)
	for _, seed := range [][]byte{
		[]byte(`{"name":"first","name":"second"}`),
		[]byte(`{"name":"value"} {"name":"next"}`),
		[]byte(`{"unknown":true}`),
	} {
		var decoded strictFuzzDocument
		if err := decodeStrictJSON(seed, &decoded); err == nil {
			f.Fatalf("strict decoder accepted malformed seed %q", seed)
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		var decoded strictFuzzDocument
		if err := decodeStrictJSON(data, &decoded); err != nil {
			return
		}
		canonical, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		var reparsed strictFuzzDocument
		if err := decodeStrictJSON(canonical, &reparsed); err != nil {
			t.Fatalf("strict decoder rejected its canonical JSON %q: %v", canonical, err)
		}
		if !reflect.DeepEqual(reparsed, decoded) {
			t.Fatalf("strict decoder changed canonical JSON %q", canonical)
		}
	})
}

func FuzzRealTimeRecordJSON(f *testing.F) {
	for _, seed := range []string{
		`{"schema":5,"version":1,"action":{"id":"start","kind":"start_session","source":"participant","payload":{"bid":"99.0000","ask":"101.0000"}},"lifecycle":"countdown"}`,
		`{"schema":5,"version":1,"action":{"id":"system/countdown","kind":"countdown_complete","source":"system"},"lifecycle":"running"}`,
		`{"schema":5,"version":1,"action":{"id":"bad","kind":"mark_move","source":"operator"},"lifecycle":"running"}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			return
		}
		var record Record
		if err := decodeStrictJSON(data, &record); err != nil {
			return
		}
		if record.Action == nil {
			return
		}
		if _, err := record.Action.Decode(); err != nil {
			return
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		var reparsed Record
		if err := decodeStrictJSON(encoded, &reparsed); err != nil {
			t.Fatalf("canonical record rejected: %v", err)
		}
		if !reflect.DeepEqual(reparsed, record) {
			t.Fatalf("record round trip changed value: got=%+v want=%+v", reparsed, record)
		}
	})
}
