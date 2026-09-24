package entity

import (
	"strings"
	"testing"
)

func TestParsePairCanonicalizesSupportedCurrencies(t *testing.T) {
	pair, err := ParsePair(" usd / eur ")
	if err != nil {
		t.Fatalf("ParsePair returned an error: %v", err)
	}
	if pair.Base != USD || pair.Quote != EUR || pair.String() != "USD/EUR" {
		t.Fatalf("ParsePair() = %#v (%q), want USD/EUR", pair, pair.String())
	}
}

func TestParsePairRejectsInvalidOrUnsupportedPairs(t *testing.T) {
	for _, input := range []string{"USDEUR", "USD/EUR/MXN", "GBP/USD", "USD/USD", "US/EUR"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParsePair(input); err == nil {
				t.Fatalf("ParsePair(%q) accepted an invalid pair", input)
			}
		})
	}
}

func TestParseRateKeepsExactCanonicalDecimal(t *testing.T) {
	for input, want := range map[string]string{
		"00012.34000": "12.34",
		".50":         "0.5",
		"10.":         "10",
	} {
		t.Run(input, func(t *testing.T) {
			rate, err := ParseRate(input)
			if err != nil {
				t.Fatalf("ParseRate(%q) returned an error: %v", input, err)
			}
			if rate.String() != want {
				t.Fatalf("ParseRate(%q).String() = %q, want %q", input, rate.String(), want)
			}
		})
	}
}

func TestParseRateRejectsNonPositiveExponentAndOversizedValues(t *testing.T) {
	for _, input := range []string{"", "-1", "0", "0.000", "1e3", "12.3.4", strings.Repeat("1", 16), "1." + strings.Repeat("1", 16)} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseRate(input); err == nil {
				t.Fatalf("ParseRate(%q) accepted an invalid rate", input)
			}
		})
	}
}
