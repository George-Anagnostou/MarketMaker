package fixed

import "testing"

func TestExactNotional(t *testing.T) {
	p, err := ParsePrice("99.1250")
	if err != nil {
		t.Fatal(err)
	}
	q, err := ParseQty("1.2345")
	if err != nil {
		t.Fatal(err)
	}
	n, err := Notional(p, q)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := n.String(), "122.36981250"; got != want {
		t.Fatalf("notional=%s want %s", got, want)
	}
}

func TestRejectPrecisionAndExponent(t *testing.T) {
	for _, input := range []string{"1.00001", "NaN", "1e2", ""} {
		if _, err := ParsePrice(input); err == nil {
			t.Errorf("ParsePrice(%q) succeeded", input)
		}
	}
}
