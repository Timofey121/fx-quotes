package httpapi

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/Timofey121/fx-quotes/internal/entity"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func canonicalPair(value string) (entity.Pair, bool) {
	pair, err := entity.ParsePair(value)
	return pair, err == nil && pair.String() == value
}

// Разбор по токенам отклоняет повторные и лишние поля, неверный регистр имени,
// null, нестроковую пару и дополнительный JSON после объекта.
func decodePair(body []byte) (entity.Pair, bool) {
	d := json.NewDecoder(strings.NewReader(string(body)))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return entity.Pair{}, false
	}
	if !d.More() {
		return entity.Pair{}, false
	}
	token, err = d.Token()
	if err != nil || token != "pair" {
		return entity.Pair{}, false
	}
	var value string
	if err = d.Decode(&value); err != nil || d.More() {
		return entity.Pair{}, false
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') {
		return entity.Pair{}, false
	}
	if _, err = d.Token(); err != io.EOF {
		return entity.Pair{}, false
	}
	return canonicalPair(value)
}

func visibleASCII(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
