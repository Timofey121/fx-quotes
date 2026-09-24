package entity

import (
	"fmt"
	"strings"
)

type Currency string

const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	MXN Currency = "MXN"
)

func ParseCurrency(value string) (Currency, error) {
	currency := Currency(strings.ToUpper(strings.TrimSpace(value)))
	switch currency {
	case USD, EUR, MXN:
		return currency, nil
	default:
		return "", fmt.Errorf("unsupported currency %q", value)
	}
}
