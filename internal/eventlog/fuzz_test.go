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
