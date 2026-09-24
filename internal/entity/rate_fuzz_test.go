package entity

import (
	"math/big"
	"strings"
	"testing"
)

func FuzzRateRoundTrip(f *testing.F) {
	for _, s := range []string{"0", "1", "0001.2300", ".1", "999999999999999.999999999999999", "1e3", "NaN", "é", "1..2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := ParseRate(s)
		if err != nil {
			return
		}
		q, err := ParseRate(r.String())
		if err != nil || r != q {
			t.Fatalf("roundtrip %q -> %q", s, r.String())
		}
		before, ok := new(big.Rat).SetString(strings.TrimSpace(s))
		if !ok {
			t.Fatalf("accepted nonnumeric %q", s)
		}
		after, ok := new(big.Rat).SetString(r.String())
		if !ok || after.Sign() <= 0 || before.Cmp(after) != 0 {
			t.Fatalf("precision %q -> %q", s, r.String())
		}
	})
}
