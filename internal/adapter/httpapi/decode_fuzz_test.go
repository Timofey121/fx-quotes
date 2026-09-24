package httpapi

import (
	"encoding/json"
	"testing"
)

func FuzzDecodePair(f *testing.F) {
	for _, s := range []string{`{"pair":"USD/EUR"}`, `{"pair":null}`, `{"pair":"USD/EUR","pair":"EUR/USD"}`, `{"Pair":"USD/EUR"}`, `[]`, `null`, `{"pair":"usd/eur"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, ok := decodePair([]byte(s))
		if !ok {
			return
		}
		if !json.Valid([]byte(s)) {
			t.Fatalf("accepted invalid JSON %q", s)
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &v); err != nil || len(v) != 1 {
			t.Fatalf("invalid fields %q", s)
		}
		var pair string
		if err := json.Unmarshal(v["pair"], &pair); err != nil || pair != p.String() {
			t.Fatalf("invalid pair %q", s)
		}
	})
}
