package fixed

import (
	"encoding/json"
	"testing"
)

func FuzzDecimalRoundTrip(f *testing.F) {
	for _, seed := range []string{"0", "1", "-1", "99.1250", "922337203685477.5807", "-922337203685477.5808", "1e2", "invalid"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 256 {
			return
		}
		assertDecimalRoundTrip(t, input, ParsePrice)
		assertDecimalRoundTrip(t, input, ParseQty)
		assertDecimalRoundTrip(t, input, ParseMoney)
	})
}

type decimal interface {
	Price | Qty | Money
	String() string
}

func assertDecimalRoundTrip[T decimal](t *testing.T, input string, parse func(string) (T, error)) {
	t.Helper()
	value, err := parse(input)
	if err != nil {
		return
	}
	canonical, err := parse(value.String())
	if err != nil || canonical != value {
		t.Fatalf("canonical parse of %q returned %d, %v; want %d", value.String(), canonical, err, value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded T
	if err := json.Unmarshal(data, &decoded); err != nil || decoded != value {
		t.Fatalf("JSON round trip of %q returned %d, %v; want %d", data, decoded, err, value)
	}
}
