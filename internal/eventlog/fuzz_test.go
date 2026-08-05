package eventlog

import (
	"encoding/json"
	"testing"
)

func FuzzDecodeStrictJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`null`),
		[]byte(`{"key":"value","nested":{"count":1}}`),
		[]byte(`{"key":1,"key":2}`),
		[]byte(`{"key":1} {"next":2}`),
		[]byte(`[`),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		var decoded any
		if err := decodeStrictJSON(data, &decoded); err != nil {
			return
		}
		canonical, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		var reparsed any
		if err := decodeStrictJSON(canonical, &reparsed); err != nil {
			t.Fatalf("strict decoder rejected its canonical JSON %q: %v", canonical, err)
		}
	})
}
