package entity

import (
	"fmt"
	"strings"
)

type Pair struct {
	Base  Currency
	Quote Currency
}

func ParsePair(value string) (Pair, error) {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) != 2 {
		return Pair{}, fmt.Errorf("pair must have format AAA/BBB")
	}
	base, err := ParseCurrency(parts[0])
	if err != nil {
		return Pair{}, err
	}
	quote, err := ParseCurrency(parts[1])
	if err != nil {
		return Pair{}, err
	}
	if base == quote {
		return Pair{}, fmt.Errorf("pair currencies must be distinct")
	}
	return Pair{Base: base, Quote: quote}, nil
}

func (pair Pair) String() string {
	return string(pair.Base) + "/" + string(pair.Quote)
}
