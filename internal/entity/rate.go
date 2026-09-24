package entity

import (
	"fmt"
	"strings"
)

type Rate struct{ decimal string }

func ParseRate(value string) (Rate, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "eE+-") || strings.Count(value, ".") > 1 {
		return Rate{}, fmt.Errorf("rate must be a plain positive decimal")
	}
	parts := strings.SplitN(value, ".", 2)
	integer := parts[0]
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if integer == "" {
		integer = "0"
	}
	if !allDigits(integer) || (fraction != "" && !allDigits(fraction)) {
		return Rate{}, fmt.Errorf("rate must be a plain positive decimal")
	}
	if len(integer) > 15 || len(fraction) > 15 {
		return Rate{}, fmt.Errorf("rate exceeds 15 digit precision")
	}
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	fraction = strings.TrimRight(fraction, "0")
	if integer == "0" && fraction == "" {
		return Rate{}, fmt.Errorf("rate must be positive")
	}
	if fraction == "" {
		return Rate{decimal: integer}, nil
	}
	return Rate{decimal: integer + "." + fraction}, nil
}

func (rate Rate) String() string { return rate.decimal }

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
